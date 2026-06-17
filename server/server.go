package server

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hanzoai/base/core"
	luxlog "github.com/luxfi/log"
	zaplib "github.com/luxfi/zap"
	zcap "github.com/zap-proto/go/cap"
)

// PSPermissions — one bit per migrated router category. The caller's verified
// CapKindIAMSession capability carries these in its u64 Permissions bitmask.
// Each ZAP method gates on exactly one bit via the single chokepoint
// (authorize → categoryPermission). Bits are stable wire values; never reorder.
const (
	PSPermCloudStatusRead uint64 = 1 << iota // cloudStatus
	PSPermPlatformOps                        // platform (deploy/redeploy)
	PSPermInfraRead                          // infrastructure
	PSPermExplorerQuery                      // explorer
	PSPermSearchQuery                        // search
	PSPermVectorWrite                        // vector
	PSPermNLFilterExec                       // natural-language filters
	PSPermBotInvoke                          // bots
	PSPermKMSAccess                          // kms
	PSPermBillingRead                        // cloud billing
	PSPermSpendAlertWrite                    // spend alerts
)

// categoryPermission maps a ZAP method to the single permission bit that gates
// it. This is the ONE place category→permission policy lives; handlers never
// re-check. An unknown method returns 0, which no capability can satisfy
// (gating fails closed).
func categoryPermission(method uint32) uint64 {
	switch method {
	case MethodCloudStatus:
		return PSPermCloudStatusRead
	case MethodPlatform:
		return PSPermPlatformOps
	case MethodInfra:
		return PSPermInfraRead
	case MethodExplorer:
		return PSPermExplorerQuery
	case MethodSearch:
		return PSPermSearchQuery
	case MethodVector:
		return PSPermVectorWrite
	case MethodNLFilter:
		return PSPermNLFilterExec
	case MethodBot:
		return PSPermBotInvoke
	case MethodKMS:
		return PSPermKMSAccess
	case MethodBilling:
		return PSPermBillingRead
	case MethodSpendAlert:
		return PSPermSpendAlertWrite
	default:
		return 0
	}
}

// Server implements the PlatformServices ZAP capability-RPC interface. It is the
// Go peer of the console's 11 tRPC routers: each ZAP method dispatches to one
// category handler, gated by the caller's capability through the single
// chokepoint authorize, then either reads/writes the Base collection (the
// stored spendAlert category) or proxies to the upstream service (every other
// category — config.go holds the wiring).
type Server struct {
	app        core.App
	logger     luxlog.Logger
	defaultOrg string
	up         Upstream

	// kms manages the Hanzo KMS bearer token (static or universal-auth with a
	// 55-minute cache). Shared by the kms category and bot.listSecrets.
	kms *kmsAuth

	// verifier validates capability buffers. Wired to ed25519 (the bootstrap
	// scheme); a PQ deployment swaps in an ML-DSA-65 SchemeVerify + the IAM
	// pubkey registry for IssuerKey. See README "Auth".
	verifier zcap.Verifier

	// promises is the server-side pipelining table: a call may carry PromiseID,
	// and a later call may Target it. Promises are FUTURES — a dependent call
	// that arrives before its target resolves WAITS on the target (it is not
	// rejected), then dispatches against the resolved answer. Cap'n Proto
	// promise pipelining; entries are short-lived (one connection turn).
	mu       sync.Mutex
	promises map[uint32]*promiseSlot
}

// promiseSlot is a future for a pipelined call's answer. done is closed when the
// slot resolves; org is then readable. For this service the pipelined value is
// the authenticated org. resolvedAt drives reaping.
type promiseSlot struct {
	done       chan struct{}
	org        string
	resolvedAt time.Time
}

// promiseWaitTimeout bounds how long a dependent call waits for its target to
// resolve before failing. Generous relative to a same-connection turn.
const promiseWaitTimeout = 5 * time.Second

// NewServer builds a PlatformServices server. verifier supplies the capability
// trust anchor; pass a Verifier whose IssuerKey resolves your IAM issuer key.
// up carries the upstream-service wiring (LoadUpstream reads it from env).
func NewServer(app core.App, logger luxlog.Logger, defaultOrg string, up Upstream, verifier zcap.Verifier) *Server {
	return &Server{
		app:        app,
		logger:     logger,
		defaultOrg: defaultOrg,
		up:         up,
		kms:        newKMSAuth(up),
		verifier:   verifier,
		promises:   make(map[uint32]*promiseSlot),
	}
}

// Register wires the server's handler onto a luxfi/zap node at this service's
// message-type slot. Called from main once the node is constructed.
func (s *Server) Register(node *zaplib.Node) {
	node.Handle(MsgTypeRouterBase, s.handle)
}

// handle is the ZAP dispatch entrypoint: decode envelope → authorize → route.
func (s *Server) handle(ctx context.Context, from string, msg *zaplib.Message) (*zaplib.Message, error) {
	call := parseRequest(msg)

	org, status, errMsg := s.authorize(call)
	if status != StatusOK {
		s.logger.Debug("ps: auth rejected", "from", from, "method", call.Method, "status", status, "err", errMsg)
		return buildResponse(status, call.PromiseID, errorBody(errMsg))
	}

	// Resolve this call's promise (the authenticated org) so any dependent call
	// WAITING on it can proceed. authorize() already awaited our own target if
	// we had one, so by here `org` is the fully-resolved scope.
	if call.PromiseID != NoTarget {
		s.resolve(call.PromiseID, org)
	}

	// Decode the typed PSRequest payload. Org from the envelope is advisory; the
	// authoritative scope is `org` (from the cap), which we pass to handlers.
	req := decodePSRequest(call.Payload)
	respStatus, body := s.dispatch(ctx, call.Method, org, req)
	return buildResponse(respStatus, call.PromiseID, body)
}

// dispatch routes an authorized call to its category handler. Each handler
// returns (status, JSON-PSResponse-bytes). This is the ONLY switch on category;
// permission policy already ran in authorize.
func (s *Server) dispatch(ctx context.Context, method uint32, org string, req psRequest) (uint32, []byte) {
	switch method {
	case MethodCloudStatus:
		return s.handleCloudStatus(ctx, org, req)
	case MethodPlatform:
		return s.handlePlatform(ctx, org, req)
	case MethodInfra:
		return s.handleInfra(ctx, org, req)
	case MethodExplorer:
		return s.handleExplorer(ctx, org, req)
	case MethodSearch:
		return s.handleSearch(ctx, org, req)
	case MethodVector:
		return s.handleVector(ctx, org, req)
	case MethodNLFilter:
		return s.handleNLFilter(ctx, org, req)
	case MethodBot:
		return s.handleBot(ctx, org, req)
	case MethodKMS:
		return s.handleKMS(ctx, org, req)
	case MethodBilling:
		return s.handleBilling(ctx, org, req)
	case MethodSpendAlert:
		return s.handleSpendAlert(ctx, org, req)
	default:
		return StatusBadRequest, errorBody(fmt.Sprintf("unknown method %d", method))
	}
}

// authorize resolves the call's effective org and enforces the capability.
// Returns (org, StatusOK, "") on success; otherwise an error status + message.
//
// Pipelining: if the call Targets an earlier promise, its org is inherited from
// that promise's resolved answer. The capability is STILL verified on every
// call — pipelining elides round trips, never authorization.
func (s *Server) authorize(call Call) (org string, status uint32, errMsg string) {
	c, err := zcap.Wrap(call.Cap)
	if err != nil {
		return "", StatusBadRequest, "malformed capability: " + err.Error()
	}

	// Kind gate: every method here is defined on a CapKindIAMSession cap.
	if c.Kind() != uint32(zcap.KindIAMSession) {
		return "", StatusForbidden, "capability is not a CapKindIAMSession"
	}

	// Permission gate — the single chokepoint. The method selects the required
	// category bit; the cap must carry it. categoryPermission fails closed (0)
	// for unknown methods.
	need := categoryPermission(call.Method)
	if need == 0 {
		return "", StatusBadRequest, fmt.Sprintf("unknown method %d", call.Method)
	}
	if c.Permissions()&need == 0 {
		return "", StatusForbidden, fmt.Sprintf("capability lacks permission bit 0x%x for method %d", need, call.Method)
	}

	// Cryptographic verification. The full chain check (signature, expiry,
	// revocation, chain links) lives in verifier.Verify. We run it whenever an
	// issuer registry is wired; with no registry (bootstrap/tests) we skip the
	// signature step but STILL enforce Kind + Permissions above.
	//
	// TODO(README "Auth"): walk the parent chain with verifier.VerifyChain and
	// bind the cap to the live session via a holderSig over a server nonce once
	// the IAM pubkey registry is wired here (set verifier.IssuerKey).
	if s.verifier.IssuerKey != nil {
		if err := s.verifier.Verify(c, time.Now().Unix()); err != nil {
			return "", StatusUnauthorized, "capability verify failed: " + err.Error()
		}
	}

	// Effective org: inherited from a targeted promise, else the service default.
	// (Holder→org mapping is an IAM lookup; until that's wired, scope to the
	// default org. The transport is FIFO per connection, so a targeted promise
	// has already resolved by the time a dependent call dispatches; await's
	// timeout is only a safety net against an out-of-order client.)
	if call.Target != NoTarget {
		org, ok := s.await(call.Target)
		if !ok {
			return "", StatusBadRequest, fmt.Sprintf("pipelined target %d did not resolve in time", call.Target)
		}
		return org, StatusOK, ""
	}
	return s.defaultOrg, StatusOK, ""
}

// --- pipelining future table (identical mechanism to the reference service) --

// getOrCreate returns the slot for id, creating an unresolved one if absent.
func (s *Server) getOrCreate(id uint32) *promiseSlot {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapLocked()
	slot, ok := s.promises[id]
	if !ok {
		slot = &promiseSlot{done: make(chan struct{})}
		s.promises[id] = slot
	}
	return slot
}

// reapLocked drops promise slots that resolved more than promiseWaitTimeout ago.
// Caller must hold s.mu.
func (s *Server) reapLocked() {
	cutoff := time.Now().Add(-promiseWaitTimeout)
	for id, slot := range s.promises {
		if !slot.resolvedAt.IsZero() && slot.resolvedAt.Before(cutoff) {
			delete(s.promises, id)
		}
	}
}

// resolve fills a promise slot with its answer and wakes any waiters. Safe to
// call once per slot; a double-resolve is guarded under the lock.
func (s *Server) resolve(id uint32, org string) {
	slot := s.getOrCreate(id)
	s.mu.Lock()
	select {
	case <-slot.done:
		// already resolved — leave as-is
	default:
		slot.org = org
		slot.resolvedAt = time.Now()
		close(slot.done)
	}
	s.mu.Unlock()
}

// await blocks until the target promise resolves or the timeout elapses,
// returning the resolved org.
func (s *Server) await(target uint32) (string, bool) {
	slot := s.getOrCreate(target)
	select {
	case <-slot.done:
		s.mu.Lock()
		org := slot.org
		s.mu.Unlock()
		return org, true
	case <-time.After(promiseWaitTimeout):
		return "", false
	}
}
