// Package server implements the PlatformServices ZAP capability-RPC service —
// the native Go peer that replaces 11 console tRPC routers (cloud status,
// platform, infrastructure, explorer, search, vector, natural-language filters,
// bots, kms, cloud billing, spend alerts).
//
// Wire model — three concentric, decoupled layers (values, not places):
//
//  1. Transport: github.com/luxfi/zap Node. Frames are length-prefixed,
//     request/response-correlated, and routed by msgType = flags>>8. This
//     service owns MsgTypeRouterBase (209), disjoint from Base's generic ORM
//     plugin (100–103) and from the sibling typed routers (200, 201, …). One
//     TCP listener, port 9998.
//
//  2. Envelope: a luxfi/zap object carrying the call shape. Request =
//     (Method u32, PromiseID u32, Target u32, Cap bytes, Payload bytes);
//     response = (Status u32, PromiseID u32, Body bytes). The Cap field is the
//     OPAQUE zap-proto/go capability buffer — re-Wrapped server-side, never
//     decoded by the transport.
//
//  3. Payload/Body: zap-proto/go typed views generated from the .zap schema
//     (gen/). Payload carries a PSRequest (Op + Org + Project + JSON Params);
//     Body carries a PSResponse (Status + JSON Body). The transport moves them
//     as opaque bytes — the contract (data) is fully separate from the
//     transport (place).
//
// Pipelining: PromiseID + Target let a caller reference the not-yet-resolved
// answer of an earlier call. A second call shipped with Target = the first
// call's PromiseID is dispatched against that promise's resolved value —
// Cap'n Proto-style promise pipelining (see client.go Pipeline()).
package server

import (
	"fmt"

	zaplib "github.com/luxfi/zap"
)

// MsgTypeRouterBase is this service's ZAP message-type slot. Base's generic ORM
// transport plugin uses 100–103; typed capability routers start at 200, and
// THIS service (the 10th in the migration) is assigned 209.
const MsgTypeRouterBase uint16 = 209

// Method identifiers — one per .zap interface method (the `@n` ordinals). Each
// method maps 1:1 to a migrated tRPC router AND to exactly one PSPerm* bit (see
// server.go categoryPermission). The per-router procedure is selected by
// PSRequest.Op (see ops.go).
const (
	MethodCloudStatus uint32 = iota // @0  cloudStatusRouter
	MethodPlatform                  // @1  platformRouter
	MethodInfra                     // @2  infrastructureRouter
	MethodExplorer                  // @3  explorerRouter
	MethodSearch                    // @4  searchRouter
	MethodVector                    // @5  vectorRouter
	MethodNLFilter                  // @6  naturalLanguageFilterRouter
	MethodBot                       // @7  botRouter
	MethodKMS                       // @8  kmsRouter
	MethodBilling                   // @9  cloudBillingRouter (EE)
	MethodSpendAlert                // @10 spendAlertRouter (EE)

	methodCount // sentinel: number of methods
)

// NoTarget is the Target value for a call that does not pipeline off an earlier
// promise (i.e. it acts on the root capability directly).
const NoTarget uint32 = 0

// Request-frame field offsets within the envelope object's fixed section.
const (
	reqMethodOff    = 0  // u32: which interface method (category)
	reqPromiseIDOff = 4  // u32: caller-assigned id this call's answer resolves to
	reqTargetOff    = 8  // u32: promise this call pipelines off (NoTarget = root)
	reqCapOff       = 12 // bytes: opaque zap-proto/go capability buffer
	reqPayloadOff   = 20 // bytes: PSRequest (zap-encoded)
	reqFixedSize    = 28
)

// Response-frame field offsets.
const (
	respStatusOff    = 0  // u32: 200 ok, else error
	respPromiseIDOff = 4  // u32: echoes the request's PromiseID
	respBodyOff      = 12 // bytes: PSResponse (zap-encoded), or error JSON
	respFixedSize    = 20
)

// Status codes mirror the HTTP-ish convention Base's ZAP plugin already uses —
// and, since the migrated routers are HTTP proxies, the upstream status maps
// straight through here.
const (
	StatusOK           uint32 = 200
	StatusBadRequest   uint32 = 400
	StatusUnauthorized uint32 = 401
	StatusForbidden    uint32 = 403
	StatusNotFound     uint32 = 404
	StatusPrecondition uint32 = 412 // mirrors tRPC PRECONDITION_FAILED (missing env config)
	StatusInternal     uint32 = 500
	StatusBadGateway   uint32 = 502 // upstream proxy unreachable / errored
)

// Call is the decoded request envelope.
type Call struct {
	Method    uint32
	PromiseID uint32
	Target    uint32
	Cap       []byte // opaque capability buffer
	Payload   []byte // opaque PSRequest bytes
}

// buildRequest encodes a Call into a luxfi/zap message tagged for this router.
func buildRequest(c Call) (*zaplib.Message, error) {
	b := zaplib.NewBuilder(len(c.Cap) + len(c.Payload) + reqFixedSize + 64)
	ob := b.StartObject(reqFixedSize)
	ob.SetUint32(reqMethodOff, c.Method)
	ob.SetUint32(reqPromiseIDOff, c.PromiseID)
	ob.SetUint32(reqTargetOff, c.Target)
	ob.SetBytes(reqCapOff, c.Cap)
	ob.SetBytes(reqPayloadOff, c.Payload)
	ob.FinishAsRoot()
	data := b.FinishWithFlags(MsgTypeRouterBase << 8)
	return zaplib.Parse(data)
}

// parseRequest decodes a luxfi/zap message into a Call.
func parseRequest(msg *zaplib.Message) Call {
	root := msg.Root()
	return Call{
		Method:    root.Uint32(reqMethodOff),
		PromiseID: root.Uint32(reqPromiseIDOff),
		Target:    root.Uint32(reqTargetOff),
		Cap:       root.Bytes(reqCapOff),
		Payload:   root.Bytes(reqPayloadOff),
	}
}

// buildResponse encodes a status + body into a router-tagged luxfi/zap message.
func buildResponse(status, promiseID uint32, body []byte) (*zaplib.Message, error) {
	b := zaplib.NewBuilder(len(body) + respFixedSize + 64)
	ob := b.StartObject(respFixedSize)
	ob.SetUint32(respStatusOff, status)
	ob.SetUint32(respPromiseIDOff, promiseID)
	ob.SetBytes(respBodyOff, body)
	ob.FinishAsRoot()
	data := b.FinishWithFlags(MsgTypeRouterBase << 8)
	return zaplib.Parse(data)
}

// Response is the decoded response envelope.
type Response struct {
	Status    uint32
	PromiseID uint32
	Body      []byte
}

// parseResponse decodes a luxfi/zap response message.
func parseResponse(msg *zaplib.Message) Response {
	root := msg.Root()
	return Response{
		Status:    root.Uint32(respStatusOff),
		PromiseID: root.Uint32(respPromiseIDOff),
		Body:      root.Bytes(respBodyOff),
	}
}

// errorBody is a minimal JSON error body, matching the shape Base's plugin and
// the upstream HTTP services return ({"error": "..."}) so a single client error
// path covers all of them.
func errorBody(msg string) []byte {
	return []byte(fmt.Sprintf(`{"error":%q}`, msg))
}
