package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/cli"
)

func TestGenDataDeliveryMainHelpAndErrors(t *testing.T) {
	for _, leaf := range []string{"all", "containers", "network"} {
		t.Run(leaf, func(t *testing.T) {
			path := []string{"gen-data", "vpsadmin", leaf}
			help, err := cli.BuiltinRegistry().Help(path, 80)
			if err != nil || strings.Contains(help, "Unavailable") {
				t.Fatal(help, err)
			}
			for _, tc := range []struct {
				extra []string
				code  int
			}{
				{[]string{"--help"}, 0},
				{[]string{"--yes"}, 64},
				{[]string{"--endpoint=fixture.invalid"}, 64},
				{[]string{"extra"}, 64},
			} {
				r := connectionCLI(t, append(append([]string{}, path...), tc.extra...), "", nil, "exit 99\n", 0, false, "")
				if r.code != tc.code || r.out != help || r.log != "" || r.trace != "" || (r.stderr != "") != (tc.code != 0) {
					t.Fatal(r)
				}
			}
			for _, input := range []string{"", "synthetic-user\nsecret-before-interrupt\x03"} {
				r := connectionCLI(t, path, input, nil, "exit 99\n", 0, false, "")
				if r.code != 1 || r.trace != "" || !strings.HasPrefix(r.out, "User name: ") || strings.Contains(r.out, "Continue?") || !strings.Contains(r.stderr, "authentication failed") || strings.Contains(r.stderr, "has no flake.nix") {
					t.Fatal("auth failure did not precede inventory", r)
				}
				genDataDeliveryLog(t, r.root, leaf, true)
				if strings.Contains(r.out+r.stderr+r.log, "synthetic-user") || strings.Contains(r.out+r.stderr+r.log, "secret-before-interrupt") {
					t.Fatal("authentication input was exposed")
				}
			}
		})
	}
}

func genDataDeliveryLog(t *testing.T, root, leaf string, retained bool) {
	t.Helper()
	logs, err := filepath.Glob(filepath.Join(root, ".confctl/logs/*.log"))
	if err != nil || retained && len(logs) != 1 || !retained && len(logs) != 0 {
		t.Fatal("gen-data log lifecycle changed", logs, err)
	}
	if !retained {
		return
	}
	if !strings.HasSuffix(logs[0], "-confctl-gen-data-vpsadmin-"+leaf+".log") {
		t.Fatal("multi-leaf log name changed", logs[0])
	}
	b, err := os.ReadFile(logs[0])
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, part := range []string{`"gen-data"`, `"vpsadmin"`, `"` + leaf + `"`, "arguments: []"} {
		if !strings.Contains(text, part) {
			t.Fatal("gen-data log lost invocation metadata")
		}
	}
	for _, secret := range []string{"synthetic-", "secret-before-interrupt", "Password:", "User name:", "The client uses"} {
		if strings.Contains(text, secret) {
			t.Fatal("gen-data log gained authentication or warning data")
		}
	}
}

// CONNECT carries the unmodified production client's TLS connection into the
// accepted API script over net.Pipe. Its only destination is that in-memory
// peer; it never resolves or dials the public API. The child trusts the fixture
// certificate through its private SSL_CERT_FILE, without a production seam.
func genDataDeliveryProxy(t *testing.T, script *publicGDHTTP) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("local gen-data proxy listener", err)
	}
	script.wantFactories = 0 // Main constructs its real provider, not s.client.
	var workers sync.WaitGroup
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != "api.vpsfree.cz:443" {
			t.Error("fixture proxy received an unexpected destination")
			http.Error(w, "fixture destination refused", http.StatusBadGateway)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		workers.Add(1)
		defer workers.Done()
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		peer, err := script.transport.DialContext(r.Context(), "tcp", "fixture")
		if err != nil {
			t.Error(err)
			return
		}
		defer peer.Close()
		if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			t.Error(err)
			return
		}
		copied := make(chan struct{})
		go func() {
			defer close(copied)
			_, _ = io.Copy(peer, conn)
		}()
		_, _ = io.Copy(conn, peer)
		_ = conn.Close()
		_ = peer.Close()
		<-copied
	})
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		<-done
		workers.Wait()
	})
	return "http://" + listener.Addr().String()
}

func TestGenDataDeliveryMainTLS(t *testing.T) {
	for _, leaf := range []string{"containers", "network", "all"} {
		for _, failure := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/failure=%v", leaf, failure), func(t *testing.T) {
				root := coreGDSeed(t)
				steps := coreGDLoginSteps()
				if leaf != "containers" {
				}
				if leaf != "containers" {
					step := coreGDListStep(`[]`)
					if failure {
						step.status, step.body = 503, ""
					}
					steps = append(steps, step)
				}
				script := publicGDServer(t, steps)
				proxy := genDataDeliveryProxy(t, script)
				cert := filepath.Join(t.TempDir(), "fixture-ca.pem")
				coreGDWrite(t, cert, string(script.rootPEM), 0600)
				tools := filepath.Join(root, "tools")
				if err := os.Mkdir(tools, 0700); err != nil {
					t.Fatal(err)
				}
				if !(failure && leaf == "containers") && leaf != "network" {
					coreGDWrite(t, filepath.Join(root, "flake.nix"), "{}\n", 0600)
					coreGDWrite(t, filepath.Join(root, "machines.json"), "{}", 0600)
				}
				healthScript(t, filepath.Join(tools, "nix"), `printf 'nix\n' >> "$HOME/trace"
case "$1" in
  eval) printf '%s\n' '{"nix":{"maxJobs":"auto","impureEval":false}}';;
  build) printf '[{"outputs":{"out":"%s/machines.json"}}]\n' "$PWD";;
  *) exit 99;;
esac
`)
				args, _ := json.Marshal([]string{"gen-data", "vpsadmin", leaf})
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLIStage1Subprocess$")
				cmd.Dir = root
				cmd.Env = []string{
					"PATH=" + tools, "HOME=" + root, "LANG=C.UTF-8",
					"CONFCTL_STAGE1_SUBPROCESS=1", "CONFCTL_STAGE1_ARGV=" + string(args),
					"HTTPS_PROXY=" + proxy, "SSL_CERT_FILE=" + cert, "SSL_CERT_DIR=" + filepath.Dir(cert),
				}
				cmd.Stdin = publicGDInput(t, "synthetic-user\nsynthetic-password\n")
				var out, stderr strings.Builder
				cmd.Stdout, cmd.Stderr = &out, &stderr
				code := comparisonDeliveryExit(t, cmd.Run())
				wantCode := 0
				if failure {
					wantCode = 1
				}
				if code != wantCode || strings.Contains(stderr.String(), "The client uses protocol v2.0") || strings.Contains(out.String(), "Continue?") || strings.Contains(out.String()+stderr.String(), "synthetic-") {
					t.Fatal(code, out.String(), stderr.String())
				}
				logs, err := filepath.Glob(filepath.Join(root, ".confctl/logs/*.log"))
				if err != nil || (len(logs) == 1) != failure {
					t.Fatal("gen-data log lifecycle changed", logs, err)
				}
				if failure {
					b, err := os.ReadFile(logs[0])
					if err != nil || strings.Contains(string(b), "synthetic-") || strings.Contains(string(b), "The client uses") || strings.Contains(string(b), "Password:") || !strings.HasSuffix(logs[0], "-confctl-gen-data-vpsadmin-"+leaf+".log") {
						t.Fatal("auth or warning data reached the command log", err)
					}
				}
				if leaf == "network" {
					if _, err := os.Stat(filepath.Join(root, "trace")); !os.IsNotExist(err) {
						t.Fatal("network command loaded inventory", err)
					}
				}
				if leaf != "network" && !(failure && leaf == "containers") {
					b, err := os.ReadFile(filepath.Join(root, coreGDContainerPath))
					if err != nil || string(b) != coreGDEmptyContainers {
						t.Fatal("containers did not save before later work", err)
					}
				}
				if leaf != "containers" && !failure {
					b, err := os.ReadFile(filepath.Join(root, coreGDNetworkPath))
					if err != nil || string(b) != coreGDEmptyNetworks {
						t.Fatal("network output was not saved", err)
					}
				}
			})
		}
	}
}
