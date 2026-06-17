// Command platform-services is a Hanzo Base-native Go service binary: a typed
// ZAP capability-RPC backend that replaces 11 console tRPC routers (cloud
// status, platform, infrastructure, explorer, search, vector, natural-language
// filters, bots, kms, cloud billing, spend alerts) with native capability RPC.
//
// Architecture (the reference service-binary pattern, msgType 209):
//
//	base.New()                    → Base app: embedded SQLite, hooks, migrations
//	  ├── vault (optional)        → per-org encrypted SQLite shard (KEK)
//	  ├── server.RegisterColl…    → cloud_spend_alert collection (the one stored entity)
//	  └── server.Register(node)   → THIS service's typed router (msgType 209)
//	apis.NewRouter(app)           → sidecar HTTP (health/metrics), NOT app data
//	app.Start()                   → serves HTTP :8090 + ZAP :9998
//
// 10 of the 11 categories proxy to upstream services (config.go holds the
// wiring, read from env / KMS); spendAlert is the only one stored in Base. The
// .zap schema (proto/) is the source of truth; gen/ is its Go projection.
package main

import (
	"crypto/rand"
	"log"
	"os"

	"github.com/hanzoai/base"
	"github.com/hanzoai/base/core"
	"github.com/hanzoai/base/plugins/vault"
	"github.com/hanzoai/base/tools/hook"
	luxlog "github.com/luxfi/log"
	zaplib "github.com/luxfi/zap"
	zcap "github.com/zap-proto/go/cap"

	"github.com/hanzoai/platform-services/server"
)

func main() {
	app := base.New()

	var zapAddr string
	app.RootCmd.PersistentFlags().StringVar(&zapAddr, "zap", envOr("ZAP_ADDR", "127.0.0.1:9998"),
		"address for the typed ZAP capability-RPC listener")

	var defaultOrg string
	app.RootCmd.PersistentFlags().StringVar(&defaultOrg, "org", envOr("PLATFORM_SERVICES_ORG", "default"),
		"default organization scope when a capability carries no org binding")

	var vaultDir string
	app.RootCmd.PersistentFlags().StringVar(&vaultDir, "vaultDir", os.Getenv("VAULT_DIR"),
		"directory for per-org encrypted SQLite shards (enables the vault plugin)")

	app.RootCmd.ParseFlags(os.Args[1:])

	// Optional: per-org encrypted SQLite backing via the vault plugin. Enabled
	// only when --vaultDir is set so local dev stays single-file SQLite. The
	// master KEK comes from KMS in production; a process-ephemeral key is used
	// when VAULT_MASTER_KEY is unset (dev only).
	if vaultDir != "" {
		vault.MustRegister(app, vault.Config{
			Enabled:   true,
			DataDir:   vaultDir,
			OrgID:     defaultOrg,
			MasterKey: masterKey(),
		})
	}

	// Ensure the one stored collection (cloud_spend_alert) exists at boot.
	server.RegisterCollections(app)

	// Upstream wiring for the 10 proxied categories, read once from env.
	up := server.LoadUpstream()

	// Stand up the typed ZAP router alongside Base's serve lifecycle. We run a
	// dedicated luxfi/zap node for the capability RPC (NoDiscovery: direct dial
	// only — service discovery is the gateway's job, not mDNS here).
	logger := luxlog.New("component", "platform-services")
	node := zaplib.NewNode(zaplib.NodeConfig{
		NodeID:      "platform-services",
		Port:        portOf(zapAddr),
		NoDiscovery: true,
	})

	// Verifier: bootstrap (ed25519, no issuer registry → Kind+Permissions
	// enforced, signature step skipped). Wire IssuerKey to the IAM pubkey
	// registry to enable full cryptographic verification.
	srv := server.NewServer(app, logger, defaultOrg, up, zcap.Verifier{})
	srv.Register(node)

	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: "platformServicesZapNode",
		Func: func(e *core.ServeEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			if err := node.Start(); err != nil {
				return err
			}
			logger.Info("platform-services ZAP router listening", "addr", zapAddr, "msgType", server.MsgTypeRouterBase)
			return nil
		},
	})
	app.OnTerminate().Bind(&hook.Handler[*core.TerminateEvent]{
		Id: "platformServicesZapNodeStop",
		Func: func(e *core.TerminateEvent) error {
			node.Stop()
			return e.Next()
		},
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

// masterKey returns the 32-byte vault master KEK: from VAULT_MASTER_KEY (hex or
// raw 32 bytes) in production, else a process-ephemeral random key for dev.
func masterKey() []byte {
	if v := os.Getenv("VAULT_MASTER_KEY"); len(v) >= 32 {
		return []byte(v)[:32]
	}
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return k
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// portOf extracts the port from a host:port address, defaulting to 9998.
func portOf(addr string) int {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			p := 0
			for _, c := range addr[i+1:] {
				if c < '0' || c > '9' {
					return 9998
				}
				p = p*10 + int(c-'0')
			}
			if p == 0 {
				return 9998
			}
			return p
		}
	}
	return 9998
}
