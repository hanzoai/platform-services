package server

import (
	"github.com/hanzoai/base/core"
	"github.com/hanzoai/base/tools/hook"
)

// This service is overwhelmingly a typed front for OTHER services: 10 of its 11
// categories are pass-throughs (config.go holds the upstream wiring). It owns
// exactly ONE Base collection — cloud_spend_alert — the native home of the
// console spendAlertRouter's `cloudSpendAlert` table. Documented per category:
//
//   cloudStatus   → PROXY (incident.io)             — no local state
//   platform      → PROXY (PaaS)                     — no local state
//   infrastructure→ PROXY (PaaS)                     — no local state
//   explorer      → PROXY (Lux Explorer)             — no local state
//   search        → PROXY (Hanzo Search API)         — no local state
//   vector        → PROXY (Hanzo Vector API)         — no local state
//   nlFilter      → PROXY (Bedrock + AI Features)    — no local state
//   bots          → PROXY (Bot Gateway/Commerce/KMS) — no local state
//   kms           → PROXY (Hanzo KMS)                — no local state (KMS is SoT)
//   billing       → PROXY (Stripe)                   — no local state
//   spendAlert    → BASE  (cloud_spend_alert)        — STORED here  ← the one row
//
// There is no cache layer: proxied reads go to the upstream every call (the
// routers did the same; cloudStatus's 60s cache lived in the console process and
// is intentionally not reintroduced — add it only if the upstream rate-limits).

// SpendAlertCollection is the single Base collection backing the spendAlert
// category. One row per alert, every row scoped by `org`.
const SpendAlertCollection = "cloud_spend_alert"

// Field names mirror the Prisma cloudSpendAlert model the router read/wrote.
const (
	faOrg         = "org"
	faTitle       = "title"
	faThreshold   = "threshold"
	faTriggeredAt = "triggeredAt"
	faCreated     = "created"
	faUpdated     = "updated"
)

// RegisterCollections ensures the cloud_spend_alert collection exists at
// startup. Idempotent: re-running finds the existing collection and no-ops.
// Wired via OnBootstrap so it runs once before the ZAP listener accepts calls.
func RegisterCollections(app core.App) {
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: "platformServicesCollections",
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			return EnsureCollection(app)
		},
	})
}

// EnsureCollection creates the cloud_spend_alert collection if absent. Exported
// so tests / one-shot migrations can provision it directly. Idempotent.
func EnsureCollection(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(SpendAlertCollection); err == nil {
		return nil // already exists
	}

	col := core.NewBaseCollection(SpendAlertCollection)
	col.Fields.Add(&core.TextField{Name: faOrg, Required: true, Max: 255})
	col.Fields.Add(&core.TextField{Name: faTitle, Required: true, Max: 100})
	col.Fields.Add(&core.NumberField{Name: faThreshold, Required: true})
	col.Fields.Add(&core.DateField{Name: faTriggeredAt}) // null until the alert fires
	col.Fields.Add(&core.AutodateField{Name: faCreated, OnCreate: true})
	col.Fields.Add(&core.AutodateField{Name: faUpdated, OnCreate: true, OnUpdate: true})

	// Index on org: alerts are always queried by org.
	col.AddIndex("idx_cloud_spend_alert_org", false, faOrg, "")

	return app.Save(col)
}
