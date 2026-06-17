package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	zaplib "github.com/luxfi/zap"
	zcap "github.com/zap-proto/go/cap"

	gen "github.com/hanzoai/platform-services/gen"
)

// Client is a PlatformServices ZAP capability-RPC client — what the console's
// bridge substitutes for the 11 in-process tRPC routers. A thin typed wrapper
// over a luxfi/zap connection that ships the verified capability with every
// call. One entry point per call (Call), a typed Result, plus a pipelining
// demonstrator (Pipeline).
type Client struct {
	node   *zaplib.Node
	peerID string
	capBuf []byte

	promiseSeq uint32 // monotonic PromiseID allocator

	sendLog *SendLog
}

// SendEvent is one entry in the instrumentation log: a call left the client
// (send) or its answer arrived (recv), with a monotonic sequence number.
type SendEvent struct {
	Seq       uint64
	Kind      string // "send" or "recv"
	Method    uint32
	Op        uint32
	PromiseID uint32
	Target    uint32
	At        time.Time
}

// SendLog is a concurrency-safe ordered record of send/recv events. It owns its
// OWN lock so that two clients sharing one log (the pipelining demonstrator
// ships the dependent call on a second client) serialize their appends through
// a single mutex — synchronization belongs to the shared log, not the per-client
// struct.
type SendLog struct {
	mu     sync.Mutex
	events []SendEvent
}

// append records one event under the log's lock.
func (l *SendLog) append(e SendEvent) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
}

// Events returns a snapshot copy of the recorded events, safe to range over.
func (l *SendLog) Events() []SendEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]SendEvent, len(l.events))
	copy(out, l.events)
	return out
}

// Reset clears the log (between phases of a probe/test).
func (l *SendLog) Reset() {
	l.mu.Lock()
	l.events = l.events[:0]
	l.mu.Unlock()
}

var sendEventSeq uint64

// pipelineIDSeq hands out process-unique promise ids for pipelined call groups.
var pipelineIDSeq uint32 = 1 << 20

func nextPipelineID() uint32 { return atomic.AddUint32(&pipelineIDSeq, 1) }

// Result is the decoded PSResponse: an HTTP-ish status plus the JSON body. The
// console decodes Body into the per-procedure TS type (same shape the tRPC
// procedure returned).
type Result struct {
	Status uint32
	Body   []byte // JSON
}

// OK reports whether the call succeeded (status 200).
func (r Result) OK() bool { return r.Status == StatusOK }

// Into unmarshals the JSON body into dst.
func (r Result) Into(dst any) error { return json.Unmarshal(r.Body, dst) }

// Dial constructs a Client over an already-started local node, connecting to the
// service at addr (e.g. "127.0.0.1:9998"). capBuf is the caller's opaque
// capability buffer (a zcap.Cap.Bytes()). peerID is the service's ZAP node id.
func Dial(node *zaplib.Node, addr, peerID string, capBuf []byte) (*Client, error) {
	if err := node.ConnectDirect(addr); err != nil {
		return nil, fmt.Errorf("platform-services client: connect %s: %w", addr, err)
	}
	return &Client{node: node, peerID: peerID, capBuf: capBuf}, nil
}

// WithSendLog attaches a shared SendLog. Two clients may share one log (the
// pipelining demonstrator does) — the log serializes appends internally. Returns
// the client for chaining.
func (c *Client) WithSendLog(log *SendLog) *Client {
	c.sendLog = log
	return c
}

func (c *Client) record(kind string, method, op, promiseID, target uint32) {
	if c.sendLog == nil {
		return
	}
	c.sendLog.append(SendEvent{
		Seq:       atomic.AddUint64(&sendEventSeq, 1),
		Kind:      kind,
		Method:    method,
		Op:        op,
		PromiseID: promiseID,
		Target:    target,
		At:        time.Now(),
	})
}

func (c *Client) nextPromise() uint32 { return atomic.AddUint32(&c.promiseSeq, 1) }

// Call is the single typed entry point: it ships a PSRequest for (method, op)
// scoped to project with JSON params, and decodes the PSResponse. params may be
// nil. This is what each generated TS client method maps onto.
func (c *Client) Call(ctx context.Context, method, op uint32, project string, params any) (Result, error) {
	return c.callTarget(ctx, method, op, project, params, c.nextPromise(), NoTarget)
}

// callTarget is Call with explicit promise/target ids (used by Pipeline).
func (c *Client) callTarget(ctx context.Context, method, op uint32, project string, params any, promiseID, target uint32) (Result, error) {
	var raw []byte
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return Result{}, fmt.Errorf("marshal params: %w", err)
		}
		raw = b
	}
	payload := gen.NewPSRequest(gen.PSRequestInput{Op: op, Project: project, Params: raw})

	msg, err := buildRequest(Call{Method: method, PromiseID: promiseID, Target: target, Cap: c.capBuf, Payload: payload})
	if err != nil {
		return Result{}, err
	}
	c.record("send", method, op, promiseID, target)
	resp, err := c.node.Call(ctx, c.peerID, msg)
	if err != nil {
		return Result{}, err
	}
	c.record("recv", method, op, promiseID, target)

	r := parseResponse(resp)
	// Unwrap the PSResponse envelope so callers see the inner status + JSON. On
	// a transport-level rejection (auth failure) the envelope body is the raw
	// error JSON and r.Status carries the wire status.
	if r.Status == StatusOK {
		if pv, perr := gen.WrapPSResponse(r.Body); perr == nil {
			return Result{Status: pv.Status(), Body: pv.Body()}, nil
		}
	}
	return Result{Status: r.Status, Body: r.Body}, nil
}

// Pipeline demonstrates Cap'n Proto promise pipelining across two category calls
// on SEPARATE connections (the transport is FIFO per connection). The dependent
// call (`dep`, a second client) ships before the first call's answer resolves,
// Targeting the first call's promise; the server's promise table joins them.
// Returns both results.
//
// Proof (on the shared send log both clients append to): the dependent call's
// "send" precedes the first call's "recv".
func (c *Client) Pipeline(ctx context.Context, dep *Client, m1, op1 uint32, p1 any, m2, op2 uint32, p2 any, project string) (Result, Result, error) {
	firstPromise := nextPipelineID()
	depPromise := nextPipelineID()

	var (
		r1, r2     Result
		err1, err2 error
		wg         sync.WaitGroup
	)
	barrier := make(chan struct{})
	wg.Add(2)

	go func() {
		defer wg.Done()
		close(barrier)
		r1, err1 = c.callTarget(ctx, m1, op1, project, p1, firstPromise, NoTarget)
	}()
	go func() {
		defer wg.Done()
		<-barrier
		r2, err2 = dep.callTarget(ctx, m2, op2, project, p2, depPromise, firstPromise)
	}()
	wg.Wait()
	if err1 != nil {
		return Result{}, Result{}, err1
	}
	if err2 != nil {
		return Result{}, Result{}, err2
	}
	return r1, r2, nil
}

// SyntheticCap mints an in-memory CapKindIAMSession capability for tests and
// bootstrap: ed25519-signed (the SPEC bootstrap scheme), holding the given
// permission bits. The signature is real (ed25519); production wires an
// IAM-issued cap instead. Returns the opaque buffer to pass to Dial.
func SyntheticCap(perms uint64) ([]byte, error) {
	signer, err := zcap.NewEd25519Signer()
	if err != nil {
		return nil, err
	}
	c, err := zcap.Issue(zcap.Issuance{
		Kind:        uint32(zcap.KindIAMSession),
		Holder:      signer.Public(),
		Permissions: perms,
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	}, signer)
	if err != nil {
		return nil, err
	}
	return c.Bytes(), nil
}

// AllPermissions is every PSPerm* bit OR'd together — a convenience for a
// fully-authorized test/bootstrap cap.
const AllPermissions = PSPermCloudStatusRead | PSPermPlatformOps | PSPermInfraRead |
	PSPermExplorerQuery | PSPermSearchQuery | PSPermVectorWrite | PSPermNLFilterExec |
	PSPermBotInvoke | PSPermKMSAccess | PSPermBillingRead | PSPermSpendAlertWrite
