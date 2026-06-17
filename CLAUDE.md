# CLAUDE.md — contract for AI helpers

A Hanzo Base-native Go service binary (one of 10 in the console tRPC→ZAP
migration). It replaces 11 tRPC routers with one typed ZAP capability-RPC
interface at msgType **209**. Cloned from the `ui-customization` reference.

## The one rule

**The `.zap` schema is the source of truth.** `proto/platform-services.zap`
defines the wire structs; `gen/` is its Go projection via `make zap-gen`. Never
hand-edit `gen/`. Change the schema, regenerate, then update `server/`.

## Shape (DRY, orthogonal, decomplected)

- **One method per category, one permission bit per method.** 11 categories →
  11 ZAP methods (`wire.go` Method*) → 11 `PSPerm*` bits (`server.go`). The
  per-router procedure is `PSRequest.Op` (`ops.go`). This keeps *which
  capability* (method/permission) orthogonal to *which procedure* (Op).
- **Uniform envelope.** Every method takes `PSRequest{Op, Org, Project, Params}`
  and returns `PSResponse{Status, Body}`; `Params`/`Body` are JSON (the proxy
  body is opaque to transport — this IS the proxy pattern, ZAP-native). Don't
  add a bespoke offset-struct per procedure; pass-throughs don't need them.
- **One auth chokepoint:** `Server.authorize`. Kind + the method's `PSPerm*` bit
  always enforced; signature verify gated on a wired issuer registry (TODO in
  `server.go`). Handlers never re-check permissions.
- **store vs proxy is the handler's lane.** `spendAlert` → Base
  (`cloud_spend_alert`, the ONLY stored collection). Every other category →
  `doJSON` proxy (`config.go` holds the upstream wiring). No cache layer.
- **Secrets:** only in env (KMS-injected in prod). Never stored, never logged.
  KMS/secret categories ALWAYS proxy — KMS is the source of truth.

## Build / test

- Pure-Go always: `CGO_ENABLED=0`, `GOWORK=off` (set by `make`). CGO pulls
  blst/accel C deps that break reproducibility; `go test -race` won't build here
  for the same reason — that's expected, not a regression.
- `make zap-gen` and `make build` are **idempotent** (byte-identical regen,
  reproducible binary). Keep them so.
- `make test` runs CRUD + per-category permission gate + proxy routing (stubbed
  upstream via httptest) + pipelining proof. Show it passing; don't claim "done"
  without it.

## Upstream proxies

Routers that proxied to external services keep the proxy pattern. Config + the
exact env vars live in `config.go`. Three categories carry an explicit TODO
blocker (nlFilter → Bedrock+SigV4; billing → shared Go Stripe pkg;
infrastructure → PaaS derive/aggregate fan-out). Each TODO names the env var(s)
that drive it. Do NOT invent upstream URLs — they're in `config.go`.

## Do not

- Build Docker images locally (CI does, multi-arch → ghcr.io/hanzoai).
- Push to GitHub from here unless asked.
- Touch the console (`~/work/hanzo/console`) — wiring is documented in README.
- Add plugins this service doesn't need. Lean binary.
- Reorder `PSPerm*` bits or method/Op ordinals — they're the wire contract the
  console's generated TS client agrees on.
