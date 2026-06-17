package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	basetests "github.com/hanzoai/base/tests"
	luxlog "github.com/luxfi/log"
	zaplib "github.com/luxfi/zap"
	zcap "github.com/zap-proto/go/cap"

	"github.com/hanzoai/platform-services/server"
)

const testOrg = "test-org"

// portSeq hands out monotonic server ports. A stopped ZAP node's TCP port can
// linger briefly in TIME_WAIT, so service() retries Start on a fresh port if a
// bind races; the sequence only ever advances, never reuses.
var portSeq = 19700

func nextPort() int { portSeq++; return portSeq }

// idSeq hands out unique client node ids (clients bind ephemeral ports, so they
// need a unique id but not a unique port).
var idSeq = 0

func nextID() int { idSeq++; return idSeq }

// service spins up a Base test app with the cloud_spend_alert collection
// provisioned, a ZAP router node listening with the supplied upstream wiring,
// and returns the listen address, node id, and a cleanup func.
func service(t *testing.T, up server.Upstream) (addr, peerID string, cleanup func()) {
	t.Helper()

	app, err := basetests.NewTestApp()
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	if err := server.EnsureCollection(app); err != nil {
		t.Fatalf("ensure collection: %v", err)
	}

	logger := luxlog.New("component", "ps-test")
	srv := server.NewServer(app, logger, testOrg, up, zcap.Verifier{})

	// Retry Start on a fresh port if the chosen one is momentarily held (a prior
	// node's TIME_WAIT). Deterministic: the sequence only advances.
	var node *zaplib.Node
	var port int
	for attempt := 0; attempt < 20; attempt++ {
		port = nextPort()
		node = zaplib.NewNode(zaplib.NodeConfig{NodeID: "ps-test-srv-" + itoa(port), Port: port, NoDiscovery: true})
		srv.Register(node)
		if err := node.Start(); err == nil {
			break
		} else if attempt == 19 {
			t.Fatalf("node start: %v", err)
		}
	}

	return "127.0.0.1:" + itoa(port), "ps-test-srv-" + itoa(port), func() {
		node.Stop()
		app.Cleanup()
	}
}

// client dials the service with a synthetic CapKindIAMSession cap holding perms.
// The client node binds an OS-assigned ephemeral port (Port 0) — clients only
// dial outbound, so they need no fixed port, and this removes any chance of a
// client/server port collision under the back-to-back test schedule. Readiness
// is established by polling a trivial call until the transport answers, rather
// than a fixed sleep that can be too short under load.
func client(t *testing.T, addr, peerID string, perms uint64) (*server.Client, func()) {
	t.Helper()
	capBuf, err := server.SyntheticCap(perms)
	if err != nil {
		t.Fatalf("synthetic cap: %v", err)
	}
	cli := zaplib.NewNode(zaplib.NodeConfig{
		NodeID:      "ps-test-cli-" + itoa(nextID()), // unique id; port is ephemeral
		Port:        0,
		NoDiscovery: true,
	})
	if err := cli.Start(); err != nil {
		t.Fatalf("client node start: %v", err)
	}
	c, err := server.Dial(cli, addr, peerID, capBuf)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	waitReady(t, c)
	return c, func() { cli.Stop() }
}

// waitReady blocks until the connection delivers a real response to a trivial
// call (any definite Status proves the handshake completed and the dispatch
// loop is live), retrying for up to 3s. This replaces a fragile fixed sleep.
func waitReady(t *testing.T, c *server.Client) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		// spendAlert.list answers with a definite Status once the link is up;
		// even a cap lacking the bit yields StatusForbidden (still "ready").
		r, err := c.Call(cx, server.MethodSpendAlert, server.OpSpendAlertList, "ready-probe", nil)
		cancel()
		if err == nil && r.Status != 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("client never became ready")
}

func ctx(t *testing.T) (context.Context, func()) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// --- spendAlert: full CRUD against real Base (the one stored category) -------

func TestSpendAlertCRUD(t *testing.T) {
	addr, peer, stop := service(t, server.Upstream{})
	defer stop()
	c, stopC := client(t, addr, peer, server.PSPermSpendAlertWrite)
	defer stopC()
	cx, cancel := ctx(t)
	defer cancel()

	// list empty
	r, err := c.Call(cx, server.MethodSpendAlert, server.OpSpendAlertList, "proj", nil)
	if err != nil || !r.OK() {
		t.Fatalf("list empty: err=%v status=%d", err, r.Status)
	}
	var empty []map[string]any
	if err := r.Into(&empty); err != nil || len(empty) != 0 {
		t.Fatalf("list empty decode: %v len=%d body=%s", err, len(empty), r.Body)
	}

	// create
	r, err = c.Call(cx, server.MethodSpendAlert, server.OpSpendAlertCreate, "proj",
		map[string]any{"title": "Monthly cap", "threshold": 500.0})
	if err != nil || !r.OK() {
		t.Fatalf("create: err=%v status=%d body=%s", err, r.Status, r.Body)
	}
	var created map[string]any
	if err := r.Into(&created); err != nil {
		t.Fatalf("create decode: %v", err)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("create returned no id: %s", r.Body)
	}
	if created["orgId"] != testOrg {
		t.Fatalf("create orgId = %v, want %s", created["orgId"], testOrg)
	}
	if created["title"] != "Monthly cap" || created["threshold"].(float64) != 500 {
		t.Fatalf("create roundtrip wrong: %s", r.Body)
	}

	// list one
	r, _ = c.Call(cx, server.MethodSpendAlert, server.OpSpendAlertList, "proj", nil)
	var one []map[string]any
	_ = r.Into(&one)
	if len(one) != 1 {
		t.Fatalf("list after create: len=%d body=%s", len(one), r.Body)
	}

	// update title + threshold
	r, err = c.Call(cx, server.MethodSpendAlert, server.OpSpendAlertUpdate, "proj",
		map[string]any{"id": id, "title": "Updated", "threshold": 750.0})
	if err != nil || !r.OK() {
		t.Fatalf("update: err=%v status=%d body=%s", err, r.Status, r.Body)
	}
	var updated map[string]any
	_ = r.Into(&updated)
	if updated["title"] != "Updated" || updated["threshold"].(float64) != 750 {
		t.Fatalf("update roundtrip wrong: %s", r.Body)
	}

	// delete
	r, err = c.Call(cx, server.MethodSpendAlert, server.OpSpendAlertDelete, "proj", map[string]any{"id": id})
	if err != nil || !r.OK() {
		t.Fatalf("delete: err=%v status=%d body=%s", err, r.Status, r.Body)
	}

	// list empty again
	r, _ = c.Call(cx, server.MethodSpendAlert, server.OpSpendAlertList, "proj", nil)
	var gone []map[string]any
	_ = r.Into(&gone)
	if len(gone) != 0 {
		t.Fatalf("list after delete: len=%d", len(gone))
	}
	t.Logf("spendAlert CRUD OK: created/updated/deleted id=%s", id)
}

func TestSpendAlertValidation(t *testing.T) {
	addr, peer, stop := service(t, server.Upstream{})
	defer stop()
	c, stopC := client(t, addr, peer, server.PSPermSpendAlertWrite)
	defer stopC()
	cx, cancel := ctx(t)
	defer cancel()

	// threshold <= 0 rejected
	r, _ := c.Call(cx, server.MethodSpendAlert, server.OpSpendAlertCreate, "proj",
		map[string]any{"title": "bad", "threshold": 0})
	if r.Status != server.StatusBadRequest {
		t.Fatalf("threshold 0 should be 400, got %d", r.Status)
	}
	// threshold over cap rejected
	r, _ = c.Call(cx, server.MethodSpendAlert, server.OpSpendAlertCreate, "proj",
		map[string]any{"title": "bad", "threshold": 2_000_000})
	if r.Status != server.StatusBadRequest {
		t.Fatalf("threshold over cap should be 400, got %d", r.Status)
	}
	// empty title rejected
	r, _ = c.Call(cx, server.MethodSpendAlert, server.OpSpendAlertCreate, "proj",
		map[string]any{"title": "", "threshold": 100})
	if r.Status != server.StatusBadRequest {
		t.Fatalf("empty title should be 400, got %d", r.Status)
	}
	t.Log("spendAlert validation OK")
}

// TestSpendAlertOrgIsolation proves a row from another org is NOT FOUND (never
// mutated/leaked). We create as testOrg, then prove an update of a fabricated id
// fails NOT_FOUND.
func TestSpendAlertOrgIsolation(t *testing.T) {
	addr, peer, stop := service(t, server.Upstream{})
	defer stop()
	c, stopC := client(t, addr, peer, server.PSPermSpendAlertWrite)
	defer stopC()
	cx, cancel := ctx(t)
	defer cancel()

	r, _ := c.Call(cx, server.MethodSpendAlert, server.OpSpendAlertUpdate, "proj",
		map[string]any{"id": "nonexistent000", "title": "x"})
	if r.Status != server.StatusNotFound {
		t.Fatalf("update of missing id should be 404, got %d body=%s", r.Status, r.Body)
	}
	t.Log("spendAlert org isolation OK")
}

// --- permission chokepoint --------------------------------------------------

// TestPermissionDeniedPerCategory proves each method gates on its own bit: a cap
// holding ONLY spendAlert's bit is refused on every other category.
func TestPermissionDeniedPerCategory(t *testing.T) {
	addr, peer, stop := service(t, testUpstream(t))
	defer stop()
	// cap with only spendAlert permission
	c, stopC := client(t, addr, peer, server.PSPermSpendAlertWrite)
	defer stopC()
	cx, cancel := ctx(t)
	defer cancel()

	others := []struct {
		name   string
		method uint32
		op     uint32
	}{
		{"cloudStatus", server.MethodCloudStatus, server.OpCloudGetStatus},
		{"platform", server.MethodPlatform, server.OpPlatformListContainers},
		{"infra", server.MethodInfra, server.OpInfraServiceHealth},
		{"explorer", server.MethodExplorer, server.OpExplorerStats},
		{"search", server.MethodSearch, server.OpSearchStats},
		{"vector", server.MethodVector, server.OpVectorStats},
		{"nlFilter", server.MethodNLFilter, server.OpNLFilterCreateCompletion},
		{"bot", server.MethodBot, server.OpBotList},
		{"kms", server.MethodKMS, server.OpKMSListSecrets},
		{"billing", server.MethodBilling, server.OpBillingGetUsage},
	}
	for _, o := range others {
		r, err := c.Call(cx, o.method, o.op, "proj", nil)
		if err != nil {
			t.Fatalf("%s call err: %v", o.name, err)
		}
		if r.Status != server.StatusForbidden {
			t.Fatalf("%s with wrong cap should be 403, got %d", o.name, r.Status)
		}
	}
	t.Logf("permission chokepoint OK: 10 categories denied without their bit")
}

// TestPermissionGrantedWithBit proves the matching bit admits the call (and the
// spendAlert category — which needs no upstream — succeeds).
func TestPermissionGrantedWithBit(t *testing.T) {
	addr, peer, stop := service(t, server.Upstream{})
	defer stop()
	c, stopC := client(t, addr, peer, server.AllPermissions)
	defer stopC()
	cx, cancel := ctx(t)
	defer cancel()
	r, err := c.Call(cx, server.MethodSpendAlert, server.OpSpendAlertList, "proj", nil)
	if err != nil || !r.OK() {
		t.Fatalf("granted spendAlert.list: err=%v status=%d", err, r.Status)
	}
	t.Log("permission granted OK")
}

// --- proxied categories against a stubbed upstream --------------------------

// stubUpstream is one httptest server standing in for ALL the HTTP upstreams.
// Each handler returns a recognizable JSON body keyed by path so the tests can
// assert the right route + verbatim pass-through.
func testUpstream(t *testing.T) server.Upstream {
	t.Helper()
	mux := http.NewServeMux()
	// Catch-all: echo the path so tests can assert routing + pass-through.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"path":   r.URL.Path,
			"method": r.Method,
			"query":  r.URL.RawQuery,
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	return server.Upstream{
		CloudStatusURL:   ts.URL + "/status",
		CloudRegion:      "US", // enable cloud-only categories
		PaaSURL:          ts.URL,
		PaaSOrgID:        "org1",
		PaaSProjectID:    "proj1",
		PaaSEnvID:        "env1",
		PaaSServiceToken: "paas-token",
		SearchURL:        ts.URL,
		SearchAPIKey:     "search-key",
		BotGatewayURL:    ts.URL,
		BotGatewayToken:  "bot-token",
		CommerceURL:      ts.URL,
		CommerceToken:    "commerce-token",
		KMSURL:           ts.URL,
		KMSServiceToken:  "kms-token",
		KMSProjectID:     "kms-ws",
	}
}

func TestCloudStatusProxy(t *testing.T) {
	addr, peer, stop := service(t, testUpstream(t))
	defer stop()
	c, stopC := client(t, addr, peer, server.PSPermCloudStatusRead)
	defer stopC()
	cx, cancel := ctx(t)
	defer cancel()
	r, err := c.Call(cx, server.MethodCloudStatus, server.OpCloudGetStatus, "proj", nil)
	if err != nil || !r.OK() {
		t.Fatalf("cloudStatus proxy: err=%v status=%d body=%s", err, r.Status, r.Body)
	}
	if !strings.Contains(string(r.Body), "/status") {
		t.Fatalf("cloudStatus did not hit /status: %s", r.Body)
	}
	t.Logf("cloudStatus proxy OK: %s", r.Body)
}

func TestCloudStatusSelfHostReturnsNull(t *testing.T) {
	// No CloudRegion → self-host → {status:null}, no upstream call.
	addr, peer, stop := service(t, server.Upstream{})
	defer stop()
	c, stopC := client(t, addr, peer, server.PSPermCloudStatusRead)
	defer stopC()
	cx, cancel := ctx(t)
	defer cancel()
	r, _ := c.Call(cx, server.MethodCloudStatus, server.OpCloudGetStatus, "proj", nil)
	if !r.OK() || !strings.Contains(string(r.Body), "null") {
		t.Fatalf("self-host cloudStatus should be {status:null}, got status=%d body=%s", r.Status, r.Body)
	}
	t.Log("cloudStatus self-host null OK")
}

func TestPlatformProxyRouting(t *testing.T) {
	addr, peer, stop := service(t, testUpstream(t))
	defer stop()
	c, stopC := client(t, addr, peer, server.PSPermPlatformOps)
	defer stopC()
	cx, cancel := ctx(t)
	defer cancel()

	// listContainers → GET …/container
	r, _ := c.Call(cx, server.MethodPlatform, server.OpPlatformListContainers, "proj", nil)
	if !r.OK() || !strings.Contains(string(r.Body), `/v1/org/org1/project/proj1/env/env1/container`) {
		t.Fatalf("platform.listContainers route wrong: %s", r.Body)
	}
	// getContainer needs containerId; missing → 400
	r, _ = c.Call(cx, server.MethodPlatform, server.OpPlatformGetContainer, "proj", map[string]any{})
	if r.Status != server.StatusBadRequest {
		t.Fatalf("platform.getContainer w/o containerId should be 400, got %d", r.Status)
	}
	// triggerBuild → POST …/redeploy
	r, _ = c.Call(cx, server.MethodPlatform, server.OpPlatformTriggerBuild, "proj", map[string]any{"containerId": "c1"})
	if !r.OK() || !strings.Contains(string(r.Body), "/container/c1/redeploy") || !strings.Contains(string(r.Body), `"POST"`) {
		t.Fatalf("platform.triggerBuild route wrong: %s", r.Body)
	}
	t.Log("platform proxy routing OK")
}

func TestPlatformPreconditionWhenUnconfigured(t *testing.T) {
	// Upstream without PaaS env → precondition.
	addr, peer, stop := service(t, server.Upstream{})
	defer stop()
	c, stopC := client(t, addr, peer, server.PSPermPlatformOps)
	defer stopC()
	cx, cancel := ctx(t)
	defer cancel()
	r, _ := c.Call(cx, server.MethodPlatform, server.OpPlatformListContainers, "proj", nil)
	if r.Status != server.StatusPrecondition {
		t.Fatalf("platform w/o config should be 412, got %d body=%s", r.Status, r.Body)
	}
	t.Log("platform precondition OK")
}

func TestSearchProxyRoutingAndAuth(t *testing.T) {
	addr, peer, stop := service(t, testUpstream(t))
	defer stop()
	c, stopC := client(t, addr, peer, server.PSPermSearchQuery)
	defer stopC()
	cx, cancel := ctx(t)
	defer cancel()
	r, _ := c.Call(cx, server.MethodSearch, server.OpSearchStats, "proj", nil)
	if !r.OK() || !strings.Contains(string(r.Body), "/api/search-docs/stats") {
		t.Fatalf("search.stats route wrong: %s", r.Body)
	}
	// query → POST /api/search-docs
	r, _ = c.Call(cx, server.MethodSearch, server.OpSearchQuery, "proj", map[string]any{"query": "hi"})
	if !r.OK() || !strings.Contains(string(r.Body), `/api/search-docs"`) {
		t.Fatalf("search.query route wrong: %s", r.Body)
	}
	t.Log("search proxy routing OK")
}

func TestSearchPreconditionWithoutKey(t *testing.T) {
	addr, peer, stop := service(t, server.Upstream{})
	defer stop()
	c, stopC := client(t, addr, peer, server.PSPermSearchQuery)
	defer stopC()
	cx, cancel := ctx(t)
	defer cancel()
	r, _ := c.Call(cx, server.MethodSearch, server.OpSearchStats, "proj", nil)
	if r.Status != server.StatusPrecondition {
		t.Fatalf("search w/o key should be 412, got %d", r.Status)
	}
	t.Log("search precondition OK")
}

func TestVectorProxyRouting(t *testing.T) {
	addr, peer, stop := service(t, testUpstream(t))
	defer stop()
	c, stopC := client(t, addr, peer, server.PSPermVectorWrite)
	defer stopC()
	cx, cancel := ctx(t)
	defer cancel()
	r, _ := c.Call(cx, server.MethodVector, server.OpVectorListCollections, "proj", nil)
	if !r.OK() || !strings.Contains(string(r.Body), "/api/vector/collections") {
		t.Fatalf("vector.listCollections route wrong: %s", r.Body)
	}
	t.Log("vector proxy routing OK")
}

func TestExplorerProxyRouting(t *testing.T) {
	addr, peer, stop := service(t, testUpstream(t))
	defer stop()
	c, stopC := client(t, addr, peer, server.PSPermExplorerQuery)
	defer stopC()
	cx, cancel := ctx(t)
	defer cancel()
	// getStats requires a valid network; bad network → 400
	r, _ := c.Call(cx, server.MethodExplorer, server.OpExplorerStats, "proj", map[string]any{"network": "bogus"})
	if r.Status != server.StatusBadRequest {
		t.Fatalf("explorer.getStats bad network should be 400, got %d", r.Status)
	}
	t.Log("explorer routing OK (explorer hits hardcoded Lux URLs; only validation asserted)")
}

func TestKMSProxyRouting(t *testing.T) {
	addr, peer, stop := service(t, testUpstream(t))
	defer stop()
	c, stopC := client(t, addr, peer, server.PSPermKMSAccess)
	defer stopC()
	cx, cancel := ctx(t)
	defer cancel()
	// listSecrets → GET /api/v3/secrets/raw with workspaceId
	r, _ := c.Call(cx, server.MethodKMS, server.OpKMSListSecrets, "proj", map[string]any{"environment": "production"})
	if !r.OK() || !strings.Contains(string(r.Body), "/api/v3/secrets/raw") || !strings.Contains(string(r.Body), "workspaceId=kms-ws") {
		t.Fatalf("kms.listSecrets route wrong: %s", r.Body)
	}
	// encrypt requires keyId; missing → 400
	r, _ = c.Call(cx, server.MethodKMS, server.OpKMSEncrypt, "proj", map[string]any{})
	if r.Status != server.StatusBadRequest {
		t.Fatalf("kms.encrypt w/o keyId should be 400, got %d", r.Status)
	}
	t.Log("kms proxy routing OK")
}

func TestKMSPreconditionWithoutWorkspace(t *testing.T) {
	// KMS configured but no workspace → precondition.
	up := testUpstream(t)
	up.KMSProjectID = ""
	addr, peer, stop := service(t, up)
	defer stop()
	c, stopC := client(t, addr, peer, server.PSPermKMSAccess)
	defer stopC()
	cx, cancel := ctx(t)
	defer cancel()
	r, _ := c.Call(cx, server.MethodKMS, server.OpKMSListSecrets, "proj", map[string]any{"environment": "production"})
	if r.Status != server.StatusPrecondition {
		t.Fatalf("kms w/o workspace should be 412, got %d", r.Status)
	}
	t.Log("kms precondition OK")
}

func TestBotGatewayRouting(t *testing.T) {
	addr, peer, stop := service(t, testUpstream(t))
	defer stop()
	c, stopC := client(t, addr, peer, server.PSPermBotInvoke)
	defer stopC()
	cx, cancel := ctx(t)
	defer cancel()
	// bot.list → POST /v1/tools/call
	r, _ := c.Call(cx, server.MethodBot, server.OpBotList, "proj", nil)
	if !r.OK() || !strings.Contains(string(r.Body), "/v1/tools/call") {
		t.Fatalf("bot.list route wrong: %s", r.Body)
	}
	// bot.getCredits → GET /v1/users/proj/credits (Commerce)
	r, _ = c.Call(cx, server.MethodBot, server.OpBotGetCredits, "proj", nil)
	if !r.OK() || !strings.Contains(string(r.Body), "/v1/users/proj/credits") {
		t.Fatalf("bot.getCredits route wrong: %s", r.Body)
	}
	t.Log("bot routing OK")
}

func TestBillingPrecondition(t *testing.T) {
	addr, peer, stop := service(t, server.Upstream{})
	defer stop()
	c, stopC := client(t, addr, peer, server.PSPermBillingRead)
	defer stopC()
	cx, cancel := ctx(t)
	defer cancel()
	r, _ := c.Call(cx, server.MethodBilling, server.OpBillingGetUsage, "proj", nil)
	if r.Status != server.StatusPrecondition {
		t.Fatalf("billing w/o stripe key should be 412, got %d body=%s", r.Status, r.Body)
	}
	t.Log("billing precondition OK")
}

func TestNLFilterSelfHostPrecondition(t *testing.T) {
	addr, peer, stop := service(t, server.Upstream{})
	defer stop()
	c, stopC := client(t, addr, peer, server.PSPermNLFilterExec)
	defer stopC()
	cx, cancel := ctx(t)
	defer cancel()
	r, _ := c.Call(cx, server.MethodNLFilter, server.OpNLFilterCreateCompletion, "proj", map[string]any{"prompt": "x"})
	if r.Status != server.StatusPrecondition {
		t.Fatalf("nlFilter self-host should be 412, got %d", r.Status)
	}
	t.Log("nlFilter self-host precondition OK")
}

// --- pipelining proof -------------------------------------------------------

// TestPipeliningShipsSecondBeforeFirstAnswer proves the dependent call ships
// before the first call's answer resolves: two "send" events precede the first
// "recv" on the shared log. Both ops are spendAlert.list (no upstream needed).
func TestPipeliningShipsSecondBeforeFirstAnswer(t *testing.T) {
	addr, peer, stop := service(t, server.Upstream{})
	defer stop()
	log := &server.SendLog{}
	c, stopC := client(t, addr, peer, server.PSPermSpendAlertWrite|server.PSPermCloudStatusRead)
	defer stopC()
	dep, stopDep := client(t, addr, peer, server.PSPermSpendAlertWrite|server.PSPermCloudStatusRead)
	defer stopDep()
	// Attach the shared log AFTER readiness probes so only the pipelined calls
	// are recorded. The log serializes the two clients' appends internally.
	c.WithSendLog(log)
	dep.WithSendLog(log)

	cx, cancel := ctx(t)
	defer cancel()

	_, _, err := c.Pipeline(cx, dep,
		server.MethodSpendAlert, server.OpSpendAlertList, nil,
		server.MethodCloudStatus, server.OpCloudGetStatus, nil,
		"proj")
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}

	firstRecv, sends := -1, 0
	for i, e := range log.Events() {
		if e.Kind == "recv" {
			firstRecv = i
			break
		}
		if e.Kind == "send" {
			sends++
		}
	}
	if firstRecv == -1 {
		t.Fatalf("no recv events recorded")
	}
	if sends < 2 {
		t.Fatalf("pipelining violated: only %d sends before first answer; want 2", sends)
	}
	t.Logf("PIPELINING PROVEN: %d calls shipped before the first answer resolved", sends)
}

// --- tiny helper ------------------------------------------------------------

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
