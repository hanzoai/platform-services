# platform-services

A Hanzo Base-native Go service binary, one of 10 in the console tRPC→ZAP
migration. It replaces **11 console tRPC routers** with a single typed
[ZAP](../zap) capability-RPC interface on [Hanzo Base](../base):

| tRPC router | ZAP method | Permission bit | Backing |
|---|---|---|---|
| `cloudStatusRouter` | `cloudStatus` @0 | `PSPermCloudStatusRead` | PROXY → incident.io (status.hanzo.ai) |
| `platformRouter` | `platform` @1 | `PSPermPlatformOps` | PROXY → PaaS (platform.hanzo.ai) |
| `infrastructureRouter` | `infra` @2 | `PSPermInfraRead` | PROXY → PaaS |
| `explorerRouter` | `explorer` @3 | `PSPermExplorerQuery` | PROXY → Lux Explorer |
| `searchRouter` | `search` @4 | `PSPermSearchQuery` | PROXY → Hanzo Search API |
| `vectorRouter` | `vector` @5 | `PSPermVectorWrite` | PROXY → Hanzo Vector API |
| `naturalLanguageFilterRouter` | `nlFilter` @6 | `PSPermNLFilterExec` | PROXY → Bedrock + AI Features |
| `botRouter` | `bot` @7 | `PSPermBotInvoke` | PROXY → Bot Gateway / Commerce / KMS |
| `kmsRouter` | `kms` @8 | `PSPermKMSAccess` | PROXY → Hanzo KMS (source of truth) |
| `cloudBillingRouter` (EE) | `billing` @9 | `PSPermBillingRead` | PROXY → Stripe |
| `spendAlertRouter` (EE) | `spendAlert` @10 | `PSPermSpendAlertWrite` | **BASE** (`cloud_spend_alert`) |

No Prisma, no Postgres-as-source-of-truth, no Mongo, no Redis, no tRPC in the
backend. 10 categories proxy to upstream services; **spendAlert is the only one
that stores state in Base**.

```
.zap schema (source of truth)  ──zapgen──▶  gen/ (Go views)
        │                                        │
        └──zapgen --target=ts──▶ console TS      ▼
                                   server/  ─ ZAP RPC handler (cap-gated, msgType 209)
                                   main.go  ─ base.New() + ZAP router :9998
                                              Base HTTP (health/metrics) :8090
```

## Design

Each ZAP **method** = one router category = one permission bit. The per-router
**procedure** is selected by `PSRequest.Op` (see `server/ops.go`). Params and
results travel as JSON inside the opaque `PSRequest.Params` / `PSResponse.Body`
byte fields — the proxy body is opaque to the transport, so this *is* the proxy
pattern kept ZAP-native via a typed envelope. The one stored entity
(`SpendAlert`) has a typed zero-copy view.

This decomplects three concerns:
- **which capability** (method → permission bit) — `Server.authorize`, one chokepoint.
- **which procedure** (`Op`) — the per-category handler's switch.
- **store vs proxy** (the handler's lane) — Base for spendAlert, `doJSON` for the rest.

## Run

```bash
make build
./platform-services serve --http=127.0.0.1:8090 --zap=127.0.0.1:9998
```

Upstream wiring is read from env at boot (see `server/config.go`); a missing
required upstream surfaces as `StatusPrecondition` (412) at call time, mirroring
the routers' `PRECONDITION_FAILED`. Secrets come from KMS in production — never
hardcoded, never logged. Optional per-org encrypted SQLite: `--vaultDir=/data/vaults`.

## Smoke test

```bash
make test            # in-process: CRUD + per-category permission gate + proxy routing + pipelining
go run ./cmd/probe --addr 127.0.0.1:9998 --peer platform-services   # out-of-process, live binary
```

## Auth

Capability auth is the single chokepoint `Server.authorize`: it Wraps the opaque
capability buffer, enforces `Kind == CapKindIAMSession` and the method's
`PSPerm*` bit, and (when an issuer registry is wired) verifies the signature.
The signature step is stubbed in bootstrap (`verifier.IssuerKey == nil`) — Kind
+ Permissions are **always** enforced. Wire `verifier.IssuerKey` to the IAM
pubkey registry to enable full cryptographic verification + chain walk.

## Upstream TODO blockers

Most categories proxy cleanly. Three carry an explicit blocker (named env var +
extraction note in code), because their upstream is not a thin REST mapping:

- **nlFilter** (`handlers_proxy.go`): AWS Bedrock `InvokeModel` behind a SigV4
  signer + AI-Features `getPrompt`. Needs `HANZO_AWS_BEDROCK_MODEL`,
  `HANZO_AI_FEATURES_{HOST,PUBLIC_KEY,SECRET_KEY,PROJECT_ID}`.
- **billing** (`billing.go`): Stripe orchestration (proration, scheduled
  downgrades, invoice pagination). Should be extracted to a shared Go billing
  package that both this service and the console call — not duplicated. Needs
  `STRIPE_SECRET_KEY` (+ `NEXTAUTH_URL`).
- **infrastructure** (`handlers_proxy.go`): the raw PaaS container list is
  proxied; the `deriveHealthStatus` / deployment-event aggregation fan-out is
  TODO (console derives client-side meanwhile).

## Wiring the console (handoff)

The console keeps its TS client (same `.zap` contract, compiled to TS via
`zapgen --target=ts`). Its ZAP bridge substitutes the in-process routers for a
ZAP client to this service:

1. Set `PLATFORM_SERVICES_ZAP_URL=tcp://127.0.0.1:9998` in console runtime.
2. Route each of the 11 capabilities to a ZAP client dialed at that URL instead
   of constructing the tRPC router in-process.
3. Wire envelope: `(method:u32, promiseID:u32, target:u32, cap:bytes,
   payload:bytes)` at ZAP msgType **209**; responses `(status:u32,
   promiseID:u32, body:bytes)`. Method ordinals + `Op` values match `server/`.
4. Delete the 11 router files from console once routes are live.

This repo does **not** edit console.

## Layout

| Path | What |
|------|------|
| `proto/platform-services.zap` | Canonical schema (zap-spec dialect → Go). |
| `gen/` | zapgen output (`make zap-gen`). Generated; do not edit. |
| `server/wire.go` | Transport envelope codec + msgType/method/status consts. |
| `server/server.go` | ZAP handler: cap auth chokepoint, dispatch, pipelining, permission bits. |
| `server/ops.go` | Per-category operation ordinals (mirror the routers). |
| `server/config.go` | Upstream endpoint/token config (one place, env-driven). |
| `server/proxy.go` | Shared upstream HTTP primitive (`doJSON`). |
| `server/psrequest.go` | PSRequest decode + PSResponse build adapters. |
| `server/handlers_proxy.go` | cloudStatus/platform/infra/explorer/search/vector/nlFilter. |
| `server/kms.go` | KMS proxy + universal-auth token manager (refresh-on-401). |
| `server/bots.go` | Bot Gateway + Commerce + KMS bot-secret proxy. |
| `server/billing.go` | Stripe billing (TODO: shared Go billing pkg). |
| `server/spendalert.go` | The one Base-stored category (CRUD, org-scoped). |
| `server/collections.go` | `cloud_spend_alert` collection + per-category store/proxy map. |
| `server/client.go` | Typed pipelining client + `SyntheticCap`. |
| `main.go` | Binary: `base.New()` + vault + ZAP router (NodeID `platform-services`, :9998). |
| `cmd/probe/` | Out-of-process smoke probe. |

Container registry: `ghcr.io/hanzoai/platform-services` (CI-built, multi-arch).
