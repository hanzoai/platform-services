package server

import (
	"encoding/json"

	gen "github.com/hanzoai/platform-services/gen"
)

// psRequest is the decoded, handler-facing form of the wire PSRequest. Op
// selects the per-category operation; Project scopes the call (Hanzo project /
// workspace id, supplied by the client); Params is the raw JSON procedure
// input, decoded lazily per-operation. Org is NOT taken from here — the
// authoritative org comes from the verified capability (see Server.authorize)
// and is threaded separately into each handler.
type psRequest struct {
	Op      uint32
	Project string
	Params  []byte // raw JSON, decoded per-op
}

// decodePSRequest parses the typed PSRequest view off the envelope payload. A
// zero/garbage payload yields a zero psRequest (Op 0, empty fields), which each
// category handler treats as "operation 0 with no params" — handlers validate
// their own required fields at the boundary.
func decodePSRequest(payload []byte) psRequest {
	v, err := gen.WrapPSRequest(payload)
	if err != nil {
		return psRequest{}
	}
	return psRequest{
		Op:      v.Op(),
		Project: v.Project(),
		Params:  v.Params(),
	}
}

// bind unmarshals the request's JSON Params into dst. Returns false (and the
// caller should reply StatusBadRequest) on malformed JSON. An empty Params
// binds successfully to the zero value of dst, matching tRPC's behaviour for
// all-optional inputs.
func (r psRequest) bind(dst any) bool {
	if len(r.Params) == 0 {
		return true
	}
	return json.Unmarshal(r.Params, dst) == nil
}

// respond marshals v as JSON and wraps it in a PSResponse with the given status.
// On a marshal error (never expected for the small result shapes here) it
// degrades to a StatusInternal error body.
func respond(status uint32, v any) (uint32, []byte) {
	body, err := json.Marshal(v)
	if err != nil {
		return StatusInternal, gen.NewPSResponse(gen.PSResponseInput{
			Status: StatusInternal,
			Body:   errorBody("marshal result: " + err.Error()),
		})
	}
	return status, gen.NewPSResponse(gen.PSResponseInput{Status: status, Body: body})
}

// fail builds a PSResponse carrying an error status + {"error":msg} body.
func fail(status uint32, msg string) (uint32, []byte) {
	return status, gen.NewPSResponse(gen.PSResponseInput{Status: status, Body: errorBody(msg)})
}

// proxied wraps a proxyResult (an upstream HTTP call's normalized outcome) in a
// PSResponse: the upstream status is carried through and its JSON body is passed
// verbatim. This is the single adapter from the proxy layer to the wire layer.
func proxied(pr proxyResult) (uint32, []byte) {
	body := pr.Body
	if len(body) == 0 {
		body = []byte("{}")
	}
	return pr.Status, gen.NewPSResponse(gen.PSResponseInput{Status: pr.Status, Body: body})
}
