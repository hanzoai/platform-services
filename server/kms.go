package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sync"
	"time"
)

// KMS category — faithful port of the console kmsRouter's client logic. Secrets
// and keys are NEVER stored in Base: Hanzo KMS is the sole source of truth, and
// every op proxies straight through. Two things the router did that we preserve
// exactly:
//
//  1. Auth resolution: a static KMS_SERVICE_TOKEN takes precedence; otherwise
//     universal-auth (KMS_CLIENT_ID + KMS_CLIENT_SECRET) logs in and the access
//     token is cached for 55 minutes, refreshed on demand / cleared on 401.
//  2. Workspace resolution: the org's KMS workspace id wins (carried per-org;
//     here scoped to the cap's org via KMS_PROJECT_ID until IAM org-metadata
//     lookup is wired — see TODO). The router pulled it from
//     Organization.metadata.kmsProjectId, falling back to KMS_PROJECT_ID.
//
// The same token manager backs bot.listSecrets (bot-scoped secret reads).

// kmsTokenTTL is the universal-auth access-token cache lifetime (router used 55
// minutes against a 60-minute KMS token).
const kmsTokenTTL = 55 * time.Minute

// kmsAuth manages the KMS bearer token. Safe for concurrent use.
type kmsAuth struct {
	up Upstream

	mu      sync.Mutex
	token   string
	expires time.Time
}

func newKMSAuth(up Upstream) *kmsAuth { return &kmsAuth{up: up} }

// bearer returns a valid KMS bearer token, logging in via universal-auth if no
// static token is set and the cache is empty/expired.
func (a *kmsAuth) bearer(ctx context.Context) (string, error) {
	if a.up.KMSServiceToken != "" {
		return a.up.KMSServiceToken, nil // static token wins, no refresh
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && time.Now().Before(a.expires) {
		return a.token, nil
	}
	if a.up.KMSClientID == "" || a.up.KMSClientSecret == "" {
		return "", fmt.Errorf("KMS auth unconfigured: set KMS_SERVICE_TOKEN or KMS_CLIENT_ID+KMS_CLIENT_SECRET")
	}
	body, _ := json.Marshal(map[string]string{
		"clientId":     a.up.KMSClientID,
		"clientSecret": a.up.KMSClientSecret,
	})
	pr, err := doJSON(ctx, "POST", a.up.KMSURL+"/api/v1/auth/universal-auth/login", body, "", nil)
	if err != nil {
		return "", fmt.Errorf("KMS universal-auth login: %w", err)
	}
	if pr.Status != StatusOK {
		return "", fmt.Errorf("KMS universal-auth login: upstream status %d", pr.Status)
	}
	var out struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(pr.Body, &out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("KMS universal-auth login: no accessToken in response")
	}
	a.token = out.AccessToken
	a.expires = time.Now().Add(kmsTokenTTL)
	return a.token, nil
}

// clear drops the cached token (called after a 401 so the next call re-logs in).
func (a *kmsAuth) clear() {
	a.mu.Lock()
	a.token = ""
	a.expires = time.Time{}
	a.mu.Unlock()
}

// kmsParams is the decoded shape of the KMS op params we route on.
type kmsParams struct {
	Environment string `json:"environment"`
	SecretPath  string `json:"secretPath"`
	SecretName  string `json:"secretName"`
	KeyID       string `json:"keyId"`
}

// kmsWorkspace resolves the KMS workspace id for the org.
//
// TODO(kms): resolve the org's KMS workspace from IAM org metadata
// (Organization.metadata.kmsProjectId) keyed by the cap's org, as the router
// did. Until that IAM lookup is wired, fall back to KMS_PROJECT_ID (the router's
// single-tenant fallback). Without either, the call fails precondition.
func (s *Server) kmsWorkspace(_ string) (string, bool) {
	if s.up.KMSProjectID != "" {
		return s.up.KMSProjectID, true
	}
	return "", false
}

// handleKMS dispatches the kms category. Every op proxies to Hanzo KMS with a
// bearer from the token manager; on a 401 the cache is cleared once and the call
// is retried, mirroring the router's refresh-on-401 behaviour.
func (s *Server) handleKMS(ctx context.Context, org string, req psRequest) (uint32, []byte) {
	ws, ok := s.kmsWorkspace(org)
	if !ok {
		return fail(StatusPrecondition, "kms: workspace unresolved (set KMS_PROJECT_ID or wire org metadata)")
	}

	var p kmsParams
	if !req.bind(&p) {
		return fail(StatusBadRequest, "kms: malformed params")
	}
	if p.SecretPath == "" {
		p.SecretPath = "/"
	}

	method, u, sendBody, status, msg := s.kmsRoute(req, ws, p)
	if status != StatusOK {
		return fail(status, msg)
	}
	return s.kmsProxy(ctx, method, u, sendBody)
}

// kmsRoute resolves the upstream method/URL/body for a kms op. Returns a non-OK
// status (with message) for unknown ops or missing required fields.
func (s *Server) kmsRoute(req psRequest, ws string, p kmsParams) (method, u string, body []byte, status uint32, msg string) {
	base := s.up.KMSURL
	switch req.Op {
	case OpKMSListSecrets:
		q := url.Values{"workspaceId": {ws}, "environment": {p.Environment}, "secretPath": {p.SecretPath}}
		return "GET", base + "/api/v3/secrets/raw?" + q.Encode(), nil, StatusOK, ""
	case OpKMSCreateSecret:
		return "POST", base + "/api/v3/secrets/raw", s.kmsSecretBody(req, ws, p.Environment, p.SecretPath), StatusOK, ""
	case OpKMSUpdateSecret:
		if p.SecretName == "" {
			return "", "", nil, StatusBadRequest, "kms.updateSecret: secretName required"
		}
		return "PATCH", base + "/api/v3/secrets/raw/" + url.PathEscape(p.SecretName), s.kmsSecretBody(req, ws, p.Environment, p.SecretPath), StatusOK, ""
	case OpKMSDeleteSecret:
		if p.SecretName == "" {
			return "", "", nil, StatusBadRequest, "kms.deleteSecret: secretName required"
		}
		q := url.Values{"workspaceId": {ws}, "environment": {p.Environment}, "secretPath": {p.SecretPath}}
		return "DELETE", base + "/api/v3/secrets/raw/" + url.PathEscape(p.SecretName) + "?" + q.Encode(), nil, StatusOK, ""
	case OpKMSListEnvironments:
		return "GET", base + "/api/v1/workspace/" + url.PathEscape(ws) + "/environments", nil, StatusOK, ""
	case OpKMSListKeys:
		return "GET", base + "/api/v1/kms/keys?projectId=" + url.QueryEscape(ws), nil, StatusOK, ""
	case OpKMSCreateKey:
		return "POST", base + "/api/v1/kms/keys", s.kmsKeyCreateBody(req, ws), StatusOK, ""
	case OpKMSUpdateKey:
		if p.KeyID == "" {
			return "", "", nil, StatusBadRequest, "kms.updateKey: keyId required"
		}
		return "PATCH", base + "/api/v1/kms/keys/" + url.PathEscape(p.KeyID), req.Params, StatusOK, ""
	case OpKMSDeleteKey:
		if p.KeyID == "" {
			return "", "", nil, StatusBadRequest, "kms.deleteKey: keyId required"
		}
		return "DELETE", base + "/api/v1/kms/keys/" + url.PathEscape(p.KeyID), nil, StatusOK, ""
	case OpKMSEncrypt:
		if p.KeyID == "" {
			return "", "", nil, StatusBadRequest, "kms.encrypt: keyId required"
		}
		return "POST", base + "/api/v1/kms/keys/" + url.PathEscape(p.KeyID) + "/encrypt", req.Params, StatusOK, ""
	case OpKMSDecrypt:
		if p.KeyID == "" {
			return "", "", nil, StatusBadRequest, "kms.decrypt: keyId required"
		}
		return "POST", base + "/api/v1/kms/keys/" + url.PathEscape(p.KeyID) + "/decrypt", req.Params, StatusOK, ""
	default:
		return "", "", nil, StatusBadRequest, fmt.Sprintf("kms: unknown op %d", req.Op)
	}
}

// kmsSecretBody injects workspaceId into the client's secret params (the KMS
// /secrets/raw body the router built). Falls back to a minimal object if the
// client sent no params.
func (s *Server) kmsSecretBody(req psRequest, ws, env, path string) []byte {
	m := map[string]any{}
	_ = json.Unmarshal(req.Params, &m)
	m["workspaceId"] = ws
	if env != "" {
		m["environment"] = env
	}
	if path != "" {
		m["secretPath"] = path
	}
	b, _ := json.Marshal(m)
	return b
}

// kmsKeyCreateBody injects projectId (== workspace) into the key-create params.
func (s *Server) kmsKeyCreateBody(req psRequest, ws string) []byte {
	m := map[string]any{}
	_ = json.Unmarshal(req.Params, &m)
	m["projectId"] = ws
	b, _ := json.Marshal(m)
	return b
}

// kmsProxy performs the KMS call with a managed bearer, retrying once on a 401
// after clearing the token cache (refresh-on-401, as the router did).
func (s *Server) kmsProxy(ctx context.Context, method, u string, body []byte) (uint32, []byte) {
	tok, err := s.kms.bearer(ctx)
	if err != nil {
		return fail(StatusPrecondition, "kms: "+err.Error())
	}
	pr, err := doJSON(ctx, method, u, body, tok, nil)
	if err != nil {
		return fail(StatusBadGateway, "kms upstream: "+err.Error())
	}
	if pr.Status == StatusUnauthorized {
		s.kms.clear()
		if tok2, err2 := s.kms.bearer(ctx); err2 == nil {
			if pr2, err3 := doJSON(ctx, method, u, body, tok2, nil); err3 == nil {
				return proxied(pr2)
			}
		}
	}
	return proxied(pr)
}
