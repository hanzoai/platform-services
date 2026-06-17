package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// httpClient is the single shared client for all upstream proxy calls. One
// client = pooled connections across categories. Per-call context can shorten
// the timeout below.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// proxyResult is the normalized outcome of an upstream HTTP call: the upstream
// status mapped into our wire Status* space, plus the raw JSON body (passed
// through verbatim — the console client decodes the upstream shape).
type proxyResult struct {
	Status uint32
	Body   []byte
}

// doJSON performs one upstream HTTP request and normalizes the result. method,
// url, and an optional JSON body + bearer token are supplied by the category
// handler. The body is passed through untouched: this service is a typed
// transport in front of the same HTTP APIs the routers called, so the upstream
// JSON IS the contract. A transport error becomes StatusBadGateway; a non-2xx
// upstream status maps through mapHTTPStatus.
//
// Auth: bearer is sent as `Authorization: Bearer <token>` when non-empty. The
// token is never logged.
func doJSON(ctx context.Context, method, url string, body []byte, bearer string, headers map[string]string) (proxyResult, error) {
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return proxyResult{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return proxyResult{Status: StatusBadGateway}, fmt.Errorf("upstream %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB cap
	if err != nil {
		return proxyResult{Status: StatusBadGateway}, fmt.Errorf("read upstream body: %w", err)
	}

	return proxyResult{Status: mapHTTPStatus(resp.StatusCode), Body: respBody}, nil
}

// mapHTTPStatus folds an upstream HTTP status code into this service's Status*
// space. 2xx → StatusOK; recognized 4xx pass through; everything else → 502
// (the upstream is reachable but unhappy, which from the caller's view is a
// gateway-level problem).
func mapHTTPStatus(code int) uint32 {
	switch {
	case code >= 200 && code < 300:
		return StatusOK
	case code == http.StatusBadRequest:
		return StatusBadRequest
	case code == http.StatusUnauthorized:
		return StatusUnauthorized
	case code == http.StatusForbidden:
		return StatusForbidden
	case code == http.StatusNotFound:
		return StatusNotFound
	case code == http.StatusPreconditionFailed:
		return StatusPrecondition
	default:
		return StatusBadGateway
	}
}
