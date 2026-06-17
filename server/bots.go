package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// Bot category — faithful port of the console botRouter. Three upstreams:
//
//   - ZAP Bot Gateway (bot lifecycle, team presets, agent DID/wallet): a single
//     POST /v1/tools/call with {name, args} returning {content}. The router
//     mapped each procedure to a tool name (bots.list, bots.create, …); we keep
//     that table in botTool().
//   - Commerce API (billing, payments, credits, balance): REST under
//     /v1/users/{project}/… and /v1/billing/balance.
//   - KMS (bot.listSecrets): bot-scoped secret read, via the shared token mgr.
//
// The router's billing GATE on bot.start/restart (refuse when balance <= 0) is
// preserved: we read the Commerce balance first and refuse before invoking the
// gateway.

// handleBot dispatches the bot category by op.
func (s *Server) handleBot(ctx context.Context, org string, req psRequest) (uint32, []byte) {
	switch req.Op {
	// --- ZAP Bot Gateway tool calls (non-billing-gated) ---
	case OpBotList, OpBotGetByID, OpBotCreate, OpBotUpdate, OpBotDelete,
		OpBotStop, OpBotGetUsage, OpBotGetLogs,
		OpBotListTeamPresets, OpBotGetTeamPreset, OpBotProvisionTeamPreset, OpBotProvisionAllPresets,
		OpBotGetAgentDID, OpBotCreateAgentDID, OpBotGetAgentWallet, OpBotCreateAgentWallet, OpBotGetAgentIdentity:
		tool, ok := botTool(req.Op)
		if !ok {
			return fail(StatusBadRequest, fmt.Sprintf("bot: unmapped gateway op %d", req.Op))
		}
		return s.botGatewayCall(ctx, tool, req.Params)

	// --- billing-gated lifecycle (start/restart): balance check, then gateway ---
	case OpBotStart, OpBotRestart:
		if status, msg := s.botBillingGate(ctx, req.Project); status != StatusOK {
			return fail(status, msg)
		}
		tool, _ := botTool(req.Op)
		return s.botGatewayCall(ctx, tool, req.Params)

	// --- Commerce API ---
	case OpBotGetBilling:
		return s.botGetBilling(ctx, req)
	case OpBotUpgradePlan:
		return s.botUpgradePlan(ctx, req)
	case OpBotListPaymentMethods:
		return s.commerceProxy(ctx, "GET", "/v1/users/"+url.PathEscape(req.Project)+"/payment-methods", nil)
	case OpBotAddPaymentMethod:
		return s.commerceProxy(ctx, "POST", "/v1/users/"+url.PathEscape(req.Project)+"/payment-methods", req.Params)
	case OpBotGetCredits:
		return s.commerceProxy(ctx, "GET", "/v1/users/"+url.PathEscape(req.Project)+"/credits", nil)
	case OpBotGetBalance:
		return s.commerceProxy(ctx, "GET", "/v1/billing/balance?user="+url.QueryEscape(req.Project)+"&currency=usd", nil)

	// --- KMS bot-scoped secrets ---
	case OpBotListSecrets:
		return s.botListSecrets(ctx, org, req)

	default:
		return fail(StatusBadRequest, fmt.Sprintf("bot: unknown op %d", req.Op))
	}
}

// botTool maps a gateway-backed op to its ZAP Bot Gateway tool name.
func botTool(op uint32) (string, bool) {
	switch op {
	case OpBotList:
		return "bots.list", true
	case OpBotGetByID:
		return "bots.get", true
	case OpBotCreate:
		return "bots.create", true
	case OpBotUpdate:
		return "bots.update", true
	case OpBotDelete:
		return "bots.delete", true
	case OpBotStart:
		return "bots.start", true
	case OpBotStop:
		return "bots.stop", true
	case OpBotRestart:
		return "bots.restart", true
	case OpBotGetUsage:
		return "bots.usage", true
	case OpBotGetLogs:
		return "bots.logs", true
	case OpBotListTeamPresets:
		return "team.presets.list", true
	case OpBotGetTeamPreset:
		return "team.presets.get", true
	case OpBotProvisionTeamPreset:
		return "team.provision", true
	case OpBotProvisionAllPresets:
		return "team.provision.all", true
	case OpBotGetAgentDID:
		return "agent.did.get", true
	case OpBotCreateAgentDID:
		return "agent.did.create", true
	case OpBotGetAgentWallet:
		return "agent.wallet.get", true
	case OpBotCreateAgentWallet:
		return "agent.wallet.create", true
	case OpBotGetAgentIdentity:
		return "agent.identity.full", true
	default:
		return "", false
	}
}

// botGatewayCall posts a tool call to the ZAP Bot Gateway and returns its
// {content} body. args is the client's JSON params, passed through as the tool
// args (the router forwarded the procedure input verbatim).
func (s *Server) botGatewayCall(ctx context.Context, tool string, args []byte) (uint32, []byte) {
	if s.up.BotGatewayToken == "" {
		return fail(StatusPrecondition, "bot: ZAP_BOT_GATEWAY_TOKEN not configured")
	}
	pr, err := doJSON(ctx, "POST", s.up.BotGatewayURL+"/v1/tools/call", botToolBody(tool, args), s.up.BotGatewayToken, nil)
	if err != nil {
		return fail(StatusBadGateway, "bot gateway: "+err.Error())
	}
	return proxied(pr)
}

// botToolBody builds the {name, args} gateway tool-call body.
func botToolBody(tool string, args []byte) []byte {
	if len(args) == 0 {
		args = []byte("{}")
	}
	body, _ := json.Marshal(map[string]json.RawMessage{
		"name": mustJSON(tool),
		"args": json.RawMessage(args),
	})
	return body
}

// botBillingGate reads the Commerce balance and refuses (mirrors the router)
// when available credit is <= 0. Returns StatusOK to proceed.
func (s *Server) botBillingGate(ctx context.Context, project string) (uint32, string) {
	if s.up.CommerceToken == "" {
		return StatusPrecondition, "bot: COMMERCE_SERVICE_TOKEN not configured"
	}
	pr, err := doJSON(ctx, "GET",
		s.up.CommerceURL+"/v1/billing/balance?user="+url.QueryEscape(project)+"&currency=usd",
		nil, s.up.CommerceToken, nil)
	if err != nil {
		return StatusBadGateway, "bot billing balance: " + err.Error()
	}
	if pr.Status != StatusOK {
		return StatusBadGateway, "bot billing balance: upstream status " + fmt.Sprint(pr.Status)
	}
	var bal struct {
		Available float64 `json:"available"`
	}
	_ = json.Unmarshal(pr.Body, &bal)
	if bal.Available <= 0 {
		return StatusForbidden, "bot: insufficient balance to start/restart (top up credit)"
	}
	return StatusOK, ""
}

// commerceProxy proxies a Commerce API call (Bearer = COMMERCE_SERVICE_TOKEN).
func (s *Server) commerceProxy(ctx context.Context, method, path string, body []byte) (uint32, []byte) {
	if s.up.CommerceToken == "" {
		return fail(StatusPrecondition, "bot: COMMERCE_SERVICE_TOKEN not configured")
	}
	pr, err := doJSON(ctx, method, s.up.CommerceURL+path, body, s.up.CommerceToken, nil)
	if err != nil {
		return fail(StatusBadGateway, "commerce: "+err.Error())
	}
	return proxied(pr)
}

// botGetBilling composes the subscription + invoices + usage view the router
// returned: Commerce subscription, Commerce invoice list, gateway usage.
func (s *Server) botGetBilling(ctx context.Context, req psRequest) (uint32, []byte) {
	var p struct {
		BotID string `json:"botId"`
	}
	if !req.bind(&p) || p.BotID == "" {
		return fail(StatusBadRequest, "bot.getBilling: botId required")
	}
	user := url.PathEscape(req.Project)
	sub := s.commerceFetch(ctx, "GET", "/v1/users/"+user+"/subscriptions?botId="+url.QueryEscape(p.BotID))
	inv := s.commerceFetch(ctx, "GET", "/v1/users/"+user+"/orders?botId="+url.QueryEscape(p.BotID)+"&type=invoice")
	usage := s.gatewayContent(ctx, "bots.usage", req.Params)
	return respond(StatusOK, map[string]json.RawMessage{
		"subscription": sub,
		"invoices":     inv,
		"usage":        usage,
	})
}

// botUpgradePlan posts the subscription change to Commerce then refreshes the
// bot via the gateway (router behaviour).
func (s *Server) botUpgradePlan(ctx context.Context, req psRequest) (uint32, []byte) {
	var p struct {
		BotID string `json:"botId"`
		Tier  string `json:"tier"`
	}
	if !req.bind(&p) || p.BotID == "" || p.Tier == "" {
		return fail(StatusBadRequest, "bot.upgradePlan: botId and tier required")
	}
	body, _ := json.Marshal(map[string]string{"botId": p.BotID, "plan": p.Tier})
	if status, b := s.commerceProxy(ctx, "POST", "/v1/users/"+url.PathEscape(req.Project)+"/subscriptions", body); status != StatusOK {
		return status, b
	}
	getArgs, _ := json.Marshal(map[string]string{"botId": p.BotID})
	return s.botGatewayCall(ctx, "bots.get", getArgs)
}

// botListSecrets reads bot-scoped secrets from KMS (never local). Path is
// /bots/{botId}, environment production, workspace = the org's KMS workspace.
func (s *Server) botListSecrets(ctx context.Context, org string, req psRequest) (uint32, []byte) {
	var p struct {
		BotID string `json:"botId"`
	}
	if !req.bind(&p) || p.BotID == "" {
		return fail(StatusBadRequest, "bot.listSecrets: botId required")
	}
	ws, ok := s.kmsWorkspace(org)
	if !ok {
		return fail(StatusPrecondition, "bot.listSecrets: KMS workspace unresolved")
	}
	q := url.Values{
		"workspaceId": {ws},
		"environment": {"production"},
		"secretPath":  {"/bots/" + p.BotID},
	}
	return s.kmsProxy(ctx, "GET", s.up.KMSURL+"/api/v3/secrets/raw?"+q.Encode(), nil)
}

// --- small JSON helpers shared by the bot handler ---

// commerceFetch performs a Commerce GET and returns its raw body (or null on any
// failure), for composition into a larger object.
func (s *Server) commerceFetch(ctx context.Context, method, path string) json.RawMessage {
	if s.up.CommerceToken == "" {
		return json.RawMessage("null")
	}
	pr, err := doJSON(ctx, method, s.up.CommerceURL+path, nil, s.up.CommerceToken, nil)
	if err != nil || pr.Status != StatusOK || len(pr.Body) == 0 {
		return json.RawMessage("null")
	}
	return json.RawMessage(pr.Body)
}

// gatewayContent calls a gateway tool and returns its raw {content} body (or
// null on failure), for composition.
func (s *Server) gatewayContent(ctx context.Context, tool string, args []byte) json.RawMessage {
	if s.up.BotGatewayToken == "" {
		return json.RawMessage("null")
	}
	pr, err := doJSON(ctx, "POST", s.up.BotGatewayURL+"/v1/tools/call", botToolBody(tool, args), s.up.BotGatewayToken, nil)
	if err != nil || pr.Status != StatusOK || len(pr.Body) == 0 {
		return json.RawMessage("null")
	}
	return json.RawMessage(pr.Body)
}

// mustJSON marshals a string to a JSON value (never errors for a string).
func mustJSON(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
