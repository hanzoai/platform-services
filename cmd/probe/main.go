// Command probe exercises a LIVE platform-services instance over ZAP — the
// out-of-process smoke test. It mints a synthetic CapKindIAMSession capability
// holding all PSPerm* bits, connects to the service at --addr, and calls a
// representative op per category, printing each result. It also runs a pipelined
// pair (spendAlert.list + cloudStatus.getStatus on two connections) and asserts
// the dependent call shipped before the first answer resolved. Exit 0 on
// success.
//
//	go run ./cmd/probe --addr 127.0.0.1:9998 --peer platform-services
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	zaplib "github.com/luxfi/zap"

	"github.com/hanzoai/platform-services/server"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9998", "service ZAP address")
	peer := flag.String("peer", "platform-services", "service ZAP node id")
	flag.Parse()

	if err := run(*addr, *peer); err != nil {
		fmt.Fprintln(os.Stderr, "PROBE FAILED:", err)
		os.Exit(1)
	}
	fmt.Println("PROBE OK")
}

func run(addr, peer string) error {
	capBuf, err := server.SyntheticCap(server.AllPermissions)
	if err != nil {
		return fmt.Errorf("mint cap: %w", err)
	}

	clientN := 0
	mkClient := func(log *server.SendLog) (*server.Client, func(), error) {
		clientN++
		node := zaplib.NewNode(zaplib.NodeConfig{
			NodeID:      fmt.Sprintf("ps-probe-%d-%d", os.Getpid(), clientN),
			Port:        0,
			NoDiscovery: true,
		})
		if err := node.Start(); err != nil {
			return nil, nil, err
		}
		c, err := server.Dial(node, addr, peer, capBuf)
		if err != nil {
			node.Stop()
			return nil, nil, err
		}
		if log != nil {
			c.WithSendLog(log)
		}
		return c, node.Stop, nil
	}

	log := &server.SendLog{}
	cli, stop1, err := mkClient(log)
	if err != nil {
		return err
	}
	defer stop1()
	dep, stop2, err := mkClient(log)
	if err != nil {
		return err
	}
	defer stop2()

	time.Sleep(200 * time.Millisecond) // handshake settle

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// spendAlert.list is the one op that does not need an upstream — it reads
	// Base directly, so it is the reliable smoke target.
	r, err := cli.Call(ctx, server.MethodSpendAlert, server.OpSpendAlertList, "probe-project", nil)
	if err != nil {
		return fmt.Errorf("spendAlert.list: %w", err)
	}
	fmt.Printf("spendAlert.list: status=%d body=%s\n", r.Status, r.Body)

	// cloudStatus.getStatus returns a stored shape (null on self-host).
	cs, err := cli.Call(ctx, server.MethodCloudStatus, server.OpCloudGetStatus, "probe-project", nil)
	if err != nil {
		return fmt.Errorf("cloudStatus.getStatus: %w", err)
	}
	fmt.Printf("cloudStatus.getStatus: status=%d body=%s\n", cs.Status, cs.Body)

	// Pipelined pair on two connections.
	log.Reset()
	_, _, err = cli.Pipeline(ctx, dep,
		server.MethodSpendAlert, server.OpSpendAlertList, nil,
		server.MethodCloudStatus, server.OpCloudGetStatus, nil,
		"probe-project")
	if err != nil {
		return fmt.Errorf("pipeline: %w", err)
	}
	sends, firstRecv := 0, -1
	for i, e := range log.Events() {
		if e.Kind == "recv" {
			firstRecv = i
			break
		}
		sends++
	}
	if firstRecv == -1 || sends < 2 {
		return fmt.Errorf("pipelining not observed: %d sends before first recv", sends)
	}
	fmt.Printf("Pipelining verified: %d calls in flight before the first answer\n", sends)
	return nil
}
