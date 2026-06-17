# platform-services.zap — canonical wire schema for the PlatformServices service.
#
# Dialect: zap-spec (the `package … / Field Type @off` grammar that
# github.com/zap-proto/go/cmd/zapgen consumes). ONE schema, two code targets:
# this repo compiles it to Go views (gen/); the console compiles the SAME file
# with `zapgen --target=ts`. No capnp on either side — the field set below is
# the single byte-for-byte contract both speak.
#
# RPC surface (hand-dispatched in server/, exactly as cap/ hand-writes Verify on
# top of zapgen'd views — zapgen emits DATA views, never method stubs):
#
#   interface PlatformServices @ MsgTypeRouterBase (209) {
#     # One method PER category (== one PSPermissions bit). The per-category
#     # operation is selected by PSRequest.Op; params/results travel as JSON in
#     # the opaque Params/Body byte fields (the proxy body is opaque to the
#     # transport — this IS the proxy pattern, kept ZAP-native via a typed
#     # envelope). The single storable entity (SpendAlert) has a typed view.
#     cloudStatus  @0  (PSRequest) -> (PSResponse)   # requires PSPermCloudStatusRead
#     platform     @1  (PSRequest) -> (PSResponse)   # requires PSPermPlatformOps
#     infra        @2  (PSRequest) -> (PSResponse)   # requires PSPermInfraRead
#     explorer     @3  (PSRequest) -> (PSResponse)   # requires PSPermExplorerQuery
#     search       @4  (PSRequest) -> (PSResponse)   # requires PSPermSearchQuery
#     vector       @5  (PSRequest) -> (PSResponse)   # requires PSPermVectorWrite
#     nlFilter     @6  (PSRequest) -> (PSResponse)   # requires PSPermNLFilterExec
#     bot          @7  (PSRequest) -> (PSResponse)   # requires PSPermBotInvoke
#     kms          @8  (PSRequest) -> (PSResponse)   # requires PSPermKMSAccess
#     billing      @9  (PSRequest) -> (PSResponse)   # requires PSPermBillingRead
#     spendAlert   @10 (PSRequest) -> (PSResponse)   # requires PSPermSpendAlertWrite
#   }
#
# Permission model: the caller's verified Capability (CapKindIAMSession = 0x01)
# carries a u64 Permissions bitmask. Each method gates on exactly one PSPerm*
# bit through the single chokepoint server.authorize → categoryPermission(method).

package ps

# PSRequest is the uniform call envelope. Op selects the operation within the
# method's category (the per-router procedure, e.g. for kms: listSecrets,
# createSecret, …). Org/Project scope the call (Org from the verified cap, never
# the client). Params is the JSON-encoded procedure input — opaque to transport,
# decoded by the category handler.
struct PSRequest {
    Op      u32   @0    # operation ordinal within the category
    Org     text  @4    # organization scope (authoritative: from the cap)
    Project text  @12   # project/workspace scope (Hanzo project id)
    Params  bytes @20   # JSON-encoded procedure params (proxy body / filter)
}

# PSResponse is the uniform reply envelope. Status mirrors the HTTP-ish code the
# handler resolved (200 ok, 4xx/5xx error). Body is the JSON-encoded result: the
# upstream service's response for proxied methods, or the local row(s) for the
# stored spendAlert category. Error bodies are {"error":"…"}.
struct PSResponse {
    Status u32   @0   # 200 ok, else error (mirrors wire.go Status* codes)
    Body   bytes @4   # JSON result, or {"error":"…"}
}

# SpendAlert is the ONE locally-stored entity in this service — the cloud spend
# alert thresholds that spendAlertRouter persisted in the `cloudSpendAlert`
# Prisma table. Mirrored 1:1 to the Base `cloud_spend_alert` collection. Every
# other category is a pass-through to an upstream service. Timestamps are unix
# seconds (0 = unset, e.g. TriggeredAt for an alert that has not fired).
struct SpendAlert {
    Id          text @0    # collection record id
    Org         text @8    # owning organization
    Title       text @16   # human label (1..100 chars, validated at the boundary)
    Threshold   f64  @24   # USD threshold (> 0, <= 1_000_000)
    TriggeredAt i64  @32    # unix seconds the alert last fired (0 = never)
    Created     i64  @40    # unix seconds created
    Updated     i64  @48    # unix seconds last updated
}

# SpendAlertList wraps the typed list returned by spendAlert.list — a distinct,
# pipeline-able read. Elements are pre-built SpendAlert sub-buffers.
struct SpendAlertList {
    Alerts list<SpendAlert> @0
}
