package server

import "os"

// Upstream resolves the configuration for every external service the migrated
// routers proxy to. ONE place, read once at construction; each field carries
// the exact env var(s) and default the original tRPC router used, so the proxy
// behaviour is byte-for-byte preserved. Empty Token/URL where an upstream is
// mandatory surfaces as StatusPrecondition at call time (mirroring the tRPC
// PRECONDITION_FAILED the routers threw when env was unset) — never a silent
// fallback, never a hardcoded secret.
//
// No secret is ever stored or logged here; tokens live only in process env
// (injected from KMS in production) and travel only in the outbound
// Authorization header to the upstream.
type Upstream struct {
	// CloudStatus: incident.io summary feed. URL configurable; the region flag
	// gates whether the call runs at all (self-host returns null).
	CloudStatusURL string // HANZO_CLOUD_STATUS_URL (default https://status.hanzo.ai/api/v1/summary)
	CloudRegion    string // NEXT_PUBLIC_HANZO_CLOUD_REGION ("" => self-host, skip)

	// PaaS (platform + infrastructure routers): platform.hanzo.ai.
	PaaSURL          string // PAAS_API_URL (default https://platform.hanzo.ai)
	PaaSOrgID        string // PAAS_ORG_ID (required)
	PaaSProjectID    string // PAAS_PROJECT_ID (required)
	PaaSEnvID        string // PAAS_ENV_ID (required)
	PaaSServiceToken string // PAAS_SERVICE_TOKEN (required, Bearer)

	// Explorer: Lux Explorer per-network APIs. URLs are derived per network
	// (api-explore-{network}.lux.network); no env config upstream.

	// Search + Vector: Hanzo Search/Vector API (shared base + key).
	SearchURL    string // HANZO_SEARCH_API_URL (default https://api.cloud.hanzo.ai)
	SearchAPIKey string // HANZO_SEARCH_API_KEY (Bearer)

	// Natural-language filters: AWS Bedrock + Hanzo AI Features prompt mgmt.
	BedrockModel      string // HANZO_AWS_BEDROCK_MODEL (required)
	AIFeaturesHost    string // HANZO_AI_FEATURES_HOST (default https://cloud.hanzo.ai)
	AIFeaturesPublic  string // HANZO_AI_FEATURES_PUBLIC_KEY (required)
	AIFeaturesSecret  string // HANZO_AI_FEATURES_SECRET_KEY (required)
	AIFeaturesProject string // HANZO_AI_FEATURES_PROJECT_ID (required)

	// Bots: ZAP Bot Gateway + Commerce API (billing) + KMS (bot-scoped secrets).
	BotGatewayURL   string // ZAP_BOT_GATEWAY_URL | BOT_GATEWAY_URL (default https://bot.hanzo.ai)
	BotGatewayToken string // ZAP_BOT_GATEWAY_TOKEN | BOT_GATEWAY_TOKEN (required, Bearer)
	CommerceURL     string // COMMERCE_API_URL (default http://commerce.hanzo.svc.cluster.local:8001)
	CommerceToken   string // COMMERCE_SERVICE_TOKEN (required, Bearer)

	// KMS (kms router + bot secret reads): Hanzo KMS service.
	KMSURL          string // KMS_API_URL (default https://kms.hanzo.ai)
	KMSServiceToken string // KMS_SERVICE_TOKEN (static Bearer; takes precedence)
	KMSClientID     string // KMS_CLIENT_ID (universal-auth login)
	KMSClientSecret string // KMS_CLIENT_SECRET (universal-auth login)
	KMSProjectID    string // KMS_PROJECT_ID (workspace fallback; org metadata wins)

	// Billing: Stripe. The console talks to Stripe via its own server SDK; this
	// service proxies billing reads/writes to the same Stripe account.
	StripeSecretKey string // STRIPE_SECRET_KEY (required for billing category)
	NextAuthURL     string // NEXTAUTH_URL (return_url for checkout/portal)
}

// LoadUpstream reads the upstream configuration from the process environment,
// applying the same defaults the original routers used.
func LoadUpstream() Upstream {
	return Upstream{
		CloudStatusURL: envOr("HANZO_CLOUD_STATUS_URL", "https://status.hanzo.ai/api/v1/summary"),
		CloudRegion:    os.Getenv("NEXT_PUBLIC_HANZO_CLOUD_REGION"),

		PaaSURL:          envOr("PAAS_API_URL", "https://platform.hanzo.ai"),
		PaaSOrgID:        os.Getenv("PAAS_ORG_ID"),
		PaaSProjectID:    os.Getenv("PAAS_PROJECT_ID"),
		PaaSEnvID:        os.Getenv("PAAS_ENV_ID"),
		PaaSServiceToken: os.Getenv("PAAS_SERVICE_TOKEN"),

		SearchURL:    envOr("HANZO_SEARCH_API_URL", "https://api.cloud.hanzo.ai"),
		SearchAPIKey: os.Getenv("HANZO_SEARCH_API_KEY"),

		BedrockModel:      os.Getenv("HANZO_AWS_BEDROCK_MODEL"),
		AIFeaturesHost:    envOr("HANZO_AI_FEATURES_HOST", "https://cloud.hanzo.ai"),
		AIFeaturesPublic:  os.Getenv("HANZO_AI_FEATURES_PUBLIC_KEY"),
		AIFeaturesSecret:  os.Getenv("HANZO_AI_FEATURES_SECRET_KEY"),
		AIFeaturesProject: os.Getenv("HANZO_AI_FEATURES_PROJECT_ID"),

		BotGatewayURL:   firstNonEmpty(os.Getenv("ZAP_BOT_GATEWAY_URL"), os.Getenv("BOT_GATEWAY_URL"), "https://bot.hanzo.ai"),
		BotGatewayToken: firstNonEmpty(os.Getenv("ZAP_BOT_GATEWAY_TOKEN"), os.Getenv("BOT_GATEWAY_TOKEN")),
		CommerceURL:     envOr("COMMERCE_API_URL", "http://commerce.hanzo.svc.cluster.local:8001"),
		CommerceToken:   os.Getenv("COMMERCE_SERVICE_TOKEN"),

		KMSURL:          envOr("KMS_API_URL", "https://kms.hanzo.ai"),
		KMSServiceToken: os.Getenv("KMS_SERVICE_TOKEN"),
		KMSClientID:     os.Getenv("KMS_CLIENT_ID"),
		KMSClientSecret: os.Getenv("KMS_CLIENT_SECRET"),
		KMSProjectID:    os.Getenv("KMS_PROJECT_ID"),

		StripeSecretKey: os.Getenv("STRIPE_SECRET_KEY"),
		NextAuthURL:     os.Getenv("NEXTAUTH_URL"),
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
