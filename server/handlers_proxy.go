package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// This file holds the pure-proxy category handlers — cloudStatus, platform,
// infrastructure, explorer, search, vector, natural-language filters. Each is a
// typed front for the same HTTP API the original tRPC router called: it
// validates required upstream config at the boundary (StatusPrecondition when
// unset, mirroring the routers' PRECONDITION_FAILED), decodes its JSON Params,
// and proxies via doJSON. None stores state locally.

// --- cloudStatus (cloudStatusRouter) ---------------------------------------
//
// getStatus: incident.io summary. Self-host (no cloud region) returns null,
// exactly as the router short-circuited.
func (s *Server) handleCloudStatus(ctx context.Context, _ string, req psRequest) (uint32, []byte) {
	if req.Op != OpCloudGetStatus {
		return fail(StatusBadRequest, fmt.Sprintf("cloudStatus: unknown op %d", req.Op))
	}
	if s.up.CloudRegion == "" {
		// Self-host: no cloud status feed. Router returned { status: null }.
		return respond(StatusOK, map[string]any{"status": nil})
	}
	pr, err := doJSON(ctx, "GET", s.up.CloudStatusURL, nil, "", nil)
	if err != nil {
		s.logger.Debug("cloudStatus proxy", "err", err)
		// Router swallowed fetch errors and returned null rather than failing.
		return respond(StatusOK, map[string]any{"status": nil})
	}
	return proxied(pr)
}

// --- platform (platformRouter) ---------------------------------------------
//
// All four ops hit the PaaS container API. paasBase asserts the required env;
// the container/pipeline path is derived per op.
func (s *Server) handlePlatform(ctx context.Context, _ string, req psRequest) (uint32, []byte) {
	base, status, msg := s.paasBase()
	if status != StatusOK {
		return fail(status, msg)
	}
	var p struct {
		ProjectID   string `json:"projectId"`
		ContainerID string `json:"containerId"`
	}
	if !req.bind(&p) {
		return fail(StatusBadRequest, "platform: malformed params")
	}

	switch req.Op {
	case OpPlatformListContainers:
		return proxyOr(s, ctx, "GET", base+"/container", nil)
	case OpPlatformGetContainer:
		if p.ContainerID == "" {
			return fail(StatusBadRequest, "platform.getContainer: containerId required")
		}
		return proxyOr(s, ctx, "GET", base+"/container/"+url.PathEscape(p.ContainerID), nil)
	case OpPlatformListPipelines:
		if p.ContainerID == "" {
			return fail(StatusBadRequest, "platform.listPipelines: containerId required")
		}
		return proxyOr(s, ctx, "GET", base+"/container/"+url.PathEscape(p.ContainerID)+"/pipelines", nil)
	case OpPlatformTriggerBuild:
		if p.ContainerID == "" {
			return fail(StatusBadRequest, "platform.triggerBuild: containerId required")
		}
		return proxyOr(s, ctx, "POST", base+"/container/"+url.PathEscape(p.ContainerID)+"/redeploy", nil)
	default:
		return fail(StatusBadRequest, fmt.Sprintf("platform: unknown op %d", req.Op))
	}
}

// --- infrastructure (infrastructureRouter) ---------------------------------
//
// getServiceHealth and getDeploymentEvents both read the PaaS container list
// (and, for events, each container's pipelines) then DERIVE their result. The
// derivation (health status, event aggregation) lived in the router; here it is
// preserved as a TODO because it requires multi-call fan-out + reshaping that
// the console client currently performs against the raw PaaS payload. We proxy
// the raw container list through so the client keeps working unchanged.
//
// TODO(infrastructure): port deriveHealthStatus + getDeploymentEvents
// aggregation (read /container, then per-container /pipelines, fold to
// ServiceHealth[] / DeploymentEvent[]). Until then the raw PaaS container list
// is returned and the console derives client-side. Driven by PAAS_* env.
func (s *Server) handleInfra(ctx context.Context, _ string, req psRequest) (uint32, []byte) {
	base, status, msg := s.paasBase()
	if status != StatusOK {
		return fail(status, msg)
	}
	switch req.Op {
	case OpInfraServiceHealth, OpInfraDeploymentEvents:
		return proxyOr(s, ctx, "GET", base+"/container", nil)
	default:
		return fail(StatusBadRequest, fmt.Sprintf("infrastructure: unknown op %d", req.Op))
	}
}

// --- explorer (explorerRouter) ---------------------------------------------
//
// Lux Explorer per-network stats/health. URLs are derived per network; no env
// config (the router baked them in).
func (s *Server) handleExplorer(ctx context.Context, _ string, req psRequest) (uint32, []byte) {
	var p struct {
		Network string `json:"network"`
	}
	if !req.bind(&p) {
		return fail(StatusBadRequest, "explorer: malformed params")
	}

	switch req.Op {
	case OpExplorerStats:
		if !validNetwork(p.Network) {
			return fail(StatusBadRequest, "explorer.getStats: network must be mainnet|testnet|devnet")
		}
		return proxyOr(s, ctx, "GET", explorerStatsURL(p.Network), nil)
	case OpExplorerAllStats:
		return s.explorerAll(ctx)
	case OpExplorerHealth:
		return s.explorerAll(ctx)
	default:
		return fail(StatusBadRequest, fmt.Sprintf("explorer: unknown op %d", req.Op))
	}
}

// --- search (searchRouter) -------------------------------------------------
//
// Hanzo Search/Docs API. All ops require HANZO_SEARCH_API_KEY. Paths mirror the
// router 1:1 (/api/search-docs/*, /api/scrape-docs, /api/chat-docs).
func (s *Server) handleSearch(ctx context.Context, _ string, req psRequest) (uint32, []byte) {
	if s.up.SearchAPIKey == "" {
		return fail(StatusPrecondition, "search: HANZO_SEARCH_API_KEY not configured")
	}
	base := s.up.SearchURL
	key := s.up.SearchAPIKey
	var p struct {
		StoreName string `json:"storeName"`
		KeyType   string `json:"keyType"`
	}
	if !req.bind(&p) {
		return fail(StatusBadRequest, "search: malformed params")
	}

	switch req.Op {
	case OpSearchStats:
		return proxyKeyed(s, ctx, "GET", base+"/api/search-docs/stats", nil, key)
	case OpSearchListIndexes:
		return proxyKeyed(s, ctx, "GET", base+"/api/search-docs/indexes", nil, key)
	case OpSearchCreateIndex:
		return proxyKeyed(s, ctx, "POST", base+"/api/scrape-docs", req.Params, key)
	case OpSearchDeleteIndex:
		if p.StoreName == "" {
			return fail(StatusBadRequest, "search.deleteIndex: storeName required")
		}
		return proxyKeyed(s, ctx, "DELETE", base+"/api/search-docs/indexes/"+url.PathEscape(p.StoreName), nil, key)
	case OpSearchReindex:
		if p.StoreName == "" {
			return fail(StatusBadRequest, "search.reindex: storeName required")
		}
		return proxyKeyed(s, ctx, "POST", base+"/api/search-docs/indexes/"+url.PathEscape(p.StoreName)+"/reindex", nil, key)
	case OpSearchQuery:
		return proxyKeyed(s, ctx, "POST", base+"/api/search-docs", req.Params, key)
	case OpSearchChat:
		return proxyKeyed(s, ctx, "POST", base+"/api/chat-docs", req.Params, key)
	case OpSearchGetKeys:
		return proxyKeyed(s, ctx, "GET", base+"/api/search-docs/keys", nil, key)
	case OpSearchRegenerateKey:
		return proxyKeyed(s, ctx, "POST", base+"/api/search-docs/keys/regenerate", req.Params, key)
	case OpSearchScrapePreview:
		return proxyKeyed(s, ctx, "POST", base+"/api/scrape-docs/preview", req.Params, key)
	default:
		return fail(StatusBadRequest, fmt.Sprintf("search: unknown op %d", req.Op))
	}
}

// --- vector (vectorRouter) -------------------------------------------------
//
// Hanzo Vector API — shares the Search base URL + API key. Paths /api/vector/*.
func (s *Server) handleVector(ctx context.Context, _ string, req psRequest) (uint32, []byte) {
	if s.up.SearchAPIKey == "" {
		return fail(StatusPrecondition, "vector: HANZO_SEARCH_API_KEY not configured")
	}
	base := s.up.SearchURL
	key := s.up.SearchAPIKey
	var p struct {
		Name           string `json:"name"`
		CollectionName string `json:"collectionName"`
	}
	if !req.bind(&p) {
		return fail(StatusBadRequest, "vector: malformed params")
	}

	switch req.Op {
	case OpVectorStats:
		return proxyKeyed(s, ctx, "GET", base+"/api/vector/stats", nil, key)
	case OpVectorListCollections:
		return proxyKeyed(s, ctx, "GET", base+"/api/vector/collections", nil, key)
	case OpVectorCreateCollection:
		return proxyKeyed(s, ctx, "POST", base+"/api/vector/collections", req.Params, key)
	case OpVectorDeleteCollection:
		if p.Name == "" {
			return fail(StatusBadRequest, "vector.deleteCollection: name required")
		}
		return proxyKeyed(s, ctx, "DELETE", base+"/api/vector/collections/"+url.PathEscape(p.Name), nil, key)
	case OpVectorSearch:
		if p.CollectionName == "" {
			return fail(StatusBadRequest, "vector.search: collectionName required")
		}
		return proxyKeyed(s, ctx, "POST", base+"/api/vector/collections/"+url.PathEscape(p.CollectionName)+"/search", req.Params, key)
	default:
		return fail(StatusBadRequest, fmt.Sprintf("vector: unknown op %d", req.Op))
	}
}

// --- naturalLanguageFilters (naturalLanguageFilterRouter) ------------------
//
// createCompletion turns a prompt into structured filter conditions via AWS
// Bedrock, using a managed prompt fetched from Hanzo AI Features. This is the
// one category whose upstream is NOT a single REST proxy — it needs the AWS
// Bedrock SigV4 invoke + the AI-Features getPrompt round trip. Wiring the AWS
// SDK + SigV4 signer into this pure-Go binary is deferred.
//
// TODO(nlFilter): wire AWS Bedrock InvokeModel (model HANZO_AWS_BEDROCK_MODEL)
// behind a SigV4 signer, fetch the "get-filter-conditions-from-query" prompt
// from Hanzo AI Features (HANZO_AI_FEATURES_HOST + PUBLIC/SECRET keys, project
// HANZO_AI_FEATURES_PROJECT_ID), run the completion, parse + validate the
// FilterCondition[]. All five env vars must be present (self-host returns the
// disabled signal, matching the router's region gate).
func (s *Server) handleNLFilter(_ context.Context, _ string, req psRequest) (uint32, []byte) {
	if req.Op != OpNLFilterCreateCompletion {
		return fail(StatusBadRequest, fmt.Sprintf("nlFilter: unknown op %d", req.Op))
	}
	if s.up.CloudRegion == "" {
		return fail(StatusPrecondition, "nlFilter: disabled on self-host (NEXT_PUBLIC_HANZO_CLOUD_REGION unset)")
	}
	for k, v := range map[string]string{
		"HANZO_AWS_BEDROCK_MODEL":      s.up.BedrockModel,
		"HANZO_AI_FEATURES_PUBLIC_KEY": s.up.AIFeaturesPublic,
		"HANZO_AI_FEATURES_SECRET_KEY": s.up.AIFeaturesSecret,
		"HANZO_AI_FEATURES_PROJECT_ID": s.up.AIFeaturesProject,
	} {
		if v == "" {
			return fail(StatusPrecondition, "nlFilter: "+k+" not configured")
		}
	}
	// Upstream Bedrock+SigV4 invoke not yet wired (see TODO above).
	return fail(StatusInternal, "nlFilter: Bedrock completion upstream not yet wired (see TODO in handlers_proxy.go)")
}

// --- shared proxy helpers ---------------------------------------------------

// paasBase asserts the required PaaS env and returns the org/project/env-scoped
// base URL prefix (…/v1/org/{org}/project/{proj}/env/{env}).
func (s *Server) paasBase() (base string, status uint32, msg string) {
	for k, v := range map[string]string{
		"PAAS_ORG_ID":        s.up.PaaSOrgID,
		"PAAS_PROJECT_ID":    s.up.PaaSProjectID,
		"PAAS_ENV_ID":        s.up.PaaSEnvID,
		"PAAS_SERVICE_TOKEN": s.up.PaaSServiceToken,
	} {
		if v == "" {
			return "", StatusPrecondition, "platform/infrastructure: " + k + " not configured"
		}
	}
	return fmt.Sprintf("%s/v1/org/%s/project/%s/env/%s",
		s.up.PaaSURL, s.up.PaaSOrgID, s.up.PaaSProjectID, s.up.PaaSEnvID), StatusOK, ""
}

// proxyOr proxies a PaaS request (Bearer = PAAS_SERVICE_TOKEN) and adapts the
// result, translating a transport error into a StatusBadGateway error body.
func proxyOr(s *Server, ctx context.Context, method, u string, body []byte) (uint32, []byte) {
	pr, err := doJSON(ctx, method, u, body, s.up.PaaSServiceToken, nil)
	if err != nil {
		s.logger.Debug("paas proxy", "method", method, "err", err)
		return fail(StatusBadGateway, "platform upstream: "+err.Error())
	}
	return proxied(pr)
}

// proxyKeyed proxies with an explicit bearer key (search/vector) and adapts.
func proxyKeyed(s *Server, ctx context.Context, method, u string, body []byte, key string) (uint32, []byte) {
	pr, err := doJSON(ctx, method, u, body, key, nil)
	if err != nil {
		s.logger.Debug("keyed proxy", "method", method, "err", err)
		return fail(StatusBadGateway, "upstream: "+err.Error())
	}
	return proxied(pr)
}

// validNetwork reports whether n is one of the three Lux networks.
func validNetwork(n string) bool {
	return n == "mainnet" || n == "testnet" || n == "devnet"
}

// explorerStatsURL builds the Lux Explorer v2 stats URL for a network.
func explorerStatsURL(network string) string {
	return fmt.Sprintf("https://api-explore-%s.lux.network/api/v2/stats", network)
}

// explorerAll fans out to all three networks and returns an array of raw stats,
// each tagged with its network, with a zero/unhealthy fallback per network —
// matching the router's Promise.allSettled behaviour.
func (s *Server) explorerAll(ctx context.Context) (uint32, []byte) {
	networks := []string{"mainnet", "testnet", "devnet"}
	out := make([]json.RawMessage, 0, len(networks))
	for _, n := range networks {
		pr, err := doJSON(ctx, "GET", explorerStatsURL(n), nil, "", nil)
		if err != nil || pr.Status != StatusOK || len(pr.Body) == 0 {
			out = append(out, json.RawMessage(fmt.Sprintf(`{"network":%q,"healthy":false}`, n)))
			continue
		}
		out = append(out, tagNetwork(n, pr.Body))
	}
	return respond(StatusOK, out)
}

// tagNetwork merges {"network":n} into a JSON object body. If the body is not a
// JSON object it is wrapped as {"network":n,"stats":<body>}.
func tagNetwork(n string, body []byte) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) == nil {
		obj["network"], _ = json.Marshal(n)
		if merged, err := json.Marshal(obj); err == nil {
			return merged
		}
	}
	return json.RawMessage(fmt.Sprintf(`{"network":%q,"stats":%s}`, n, body))
}
