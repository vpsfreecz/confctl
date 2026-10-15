//go:build linux

package core

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/gendata"
)

// These tests use real provider parsing/auth, native redirected-input prompting,
// the shared formatter and disposable files. Only the HTTP destination is
// supplied by the injected test Do; production's endpoint is never changed.
type publicGDHTTP struct {
	t             *testing.T
	clientHTTP    *http.Client
	transport     *http.Transport
	workers       sync.WaitGroup
	mu            sync.Mutex
	steps         []coreGDStep
	at            int
	factories     int
	wantFactories int
	rootPEM       []byte
}

func publicGDServer(t *testing.T, steps []coreGDStep) *publicGDHTTP {
	t.Helper()
	s := &publicGDHTTP{t: t, steps: steps, wantFactories: 1}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.at >= len(s.steps) {
			t.Error("request escaped the local gen-data script")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		step := s.steps[s.at]
		s.at++
		if r.Method != step.method || r.URL.RequestURI() != step.uri || r.Header.Get("X-Test-Token") != step.token || r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") != "haveapi-client" {
			t.Error("gen-data request method/route/token/identity differs")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/login" {
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("login input is not JSON")
			}
			want := map[string]any{"credentials": map[string]any{"user": "synthetic-user", "password": "synthetic-password", "lifetime": "fixed", "interval": float64(60)}}
			if !reflect.DeepEqual(body, want) {
				t.Error("login credentials or fixed token lifetime differs")
			}
		}
		if step.before != nil {
			step.before(r)
		}
		status := step.status
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, step.body)
	})
	// A disposable TLS peer over net.Pipe exercises the real HTTP transport
	// without needing a socket/listener permission or any external network.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"api.vpsfree.cz"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		BasicConstraintsValid: true, IsCA: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	s.rootPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	serverTLS := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	s.transport = &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{RootCAs: roots},
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			client, peer := net.Pipe()
			_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
			s.workers.Add(1)
			go func() {
				defer s.workers.Done()
				defer peer.Close()
				secure := tls.Server(peer, serverTLS)
				r, err := http.ReadRequest(bufio.NewReader(secure))
				if err != nil {
					t.Error("local TLS peer could not read the HTTP request")
					return
				}
				defer r.Body.Close()
				out := httptest.NewRecorder()
				handler.ServeHTTP(out, r)
				response := out.Result()
				defer response.Body.Close()
				if err := response.Write(secure); err != nil {
					t.Error("local TLS peer could not write the HTTP response")
				}
			}()
			return client, nil
		}}
	s.clientHTTP = &http.Client{Transport: s.transport}
	t.Cleanup(func() {
		s.transport.CloseIdleConnections()
		s.workers.Wait()
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.at != len(s.steps) || s.factories != s.wantFactories {
			t.Error("gen-data skipped a request or did not reuse one client")
		}
	})
	return s
}

func (s *publicGDHTTP) client(prompt gendata.Prompt, warn func(string) error) (*gendata.VPSAdmin, error) {
	s.factories++
	return gendata.NewVPSAdminWithWarningBindings(prompt, warn, func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.vpsfree.cz" {
			s.t.Fatal("production origin changed before the test transport")
		}
		return s.clientHTTP.Do(r)
	}, time.Now)
}

func publicGDInput(t *testing.T, text string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func publicGDInvocation(t *testing.T, leaf string, extra ...string) cli.Invocation {
	t.Helper()
	inv, err := cli.BuiltinRegistry().Parse(append([]string{"gen-data", "vpsadmin", leaf}, extra...))
	if err != nil {
		t.Fatal(err)
	}
	return inv // availability/dispatch is the separate shared CLI handoff
}

func TestGenDataCommandLeaves(t *testing.T) {
	const rows = `[{"ip_version":6,"address":"fd00::","prefix":64},{"ip_version":4,"address":"192.0.2.0","prefix":24},{"ip_version":"4","address":{},"prefix":{}},{"ip_version":4.0,"address":"198.51.100.0","prefix":9007199254740993}]`
	const networks = "{\n  \"ipv4\" = [\n    {\n      address = \"192.0.2.0\";\n      prefix = 24;\n    }\n    {\n      address = \"198.51.100.0\";\n      prefix = 9007199254740993;\n    }\n  ];\n  \"ipv6\" = [\n    {\n      address = \"fd00::\";\n      prefix = 64;\n    }\n  ];\n}\n"
	for _, leaf := range []string{"containers", "network", "all"} {
		t.Run(leaf, func(t *testing.T) {
			root := coreGDSeed(t)
			want := coreGDTree(t, root)
			steps := append(coreGDLoginSteps(), coreGDResourceStep())
			var machines []Machine
			if leaf != "network" {
				first, last := coreGDMachine(json.Number("2")), coreGDMachine(json.Number("1"))
				first.Managed = false
				carrier := "parent"
				first.CarrierName = &carrier
				machines = []Machine{{Name: "no-container"}, first, last}
				steps = append(steps, coreGDShowStep("2", coreGDNode), coreGDShowStep("1", strings.Replace(coreGDNode, `"name":"node"`, `"name":"last"`, 1)))
				coreGDReplace(want, coreGDContainerPath, strings.Replace(coreGDContainers, `name = "node"`, `name = "last"`, 1))
			}
			if leaf != "containers" {
				step := coreGDListStep(rows)
				if leaf == "all" {
					step.before = func(*http.Request) {
						b, err := os.ReadFile(filepath.Join(root, coreGDContainerPath))
						if err != nil || string(b) != want[coreGDContainerPath].body {
							t.Error("network began before containers replacement")
						}
					}
				}
				steps = append(steps, step)
				coreGDReplace(want, coreGDNetworkPath, networks)
			}
			s := publicGDServer(t, steps)
			e := &Engine{Root: root, Context: context.Background(), Inventory: machines}
			in := publicGDInput(t, " synthetic-user \t\nsynthetic-password\nremaining\n")
			var out, warnings bytes.Buffer
			if err := e.genDataCommand(publicGDInvocation(t, leaf), in, &out, &warnings, s.client); err != nil {
				t.Fatal("gen-data leaf failed:", err)
			}
			if out.String() != "User name: Password: \n" || warnings.Len() != 0 {
				t.Fatal("prompt order changed, confirmation added, or credentials echoed")
			}
			coreGDCheckTree(t, root, want)
			// Both borrowed input and output remain usable after the command.
			if _, err := in.Seek(0, io.SeekStart); err != nil {
				t.Fatal("input was closed")
			}
		})
	}
}

func TestGenDataCommandAuthAndWarningFailure(t *testing.T) {
	for _, phase := range []string{"auth", "prompt-eof", "warning"} {
		t.Run(phase, func(t *testing.T) {
			root := coreGDSeed(t)
			want := coreGDTree(t, root)
			e := &Engine{Root: root, Context: context.Background()}
			steps := coreGDLoginSteps()
			input := "synthetic-user\nsynthetic-password\n"
			if phase == "auth" {
				steps[1].body = `{"status":false,"message":"synthetic-secret-response"}`
			} else if phase == "prompt-eof" {
				steps = nil
				input = ""
			} else {
				steps = []coreGDStep{{method: "OPTIONS", uri: "/?describe=default", body: strings.Replace(coreGDDescription, `"version":"2.0"`, `"version":"2.1"`, 1)}}
			}
			s := publicGDServer(t, steps)
			var out, warnings bytes.Buffer
			var sink io.Writer = &warnings
			cause := errors.New("synthetic-secret-writer")
			if phase == "warning" {
				sink = publicGDWriterError{cause}
			}
			err := e.genDataCommand(publicGDInvocation(t, "all"), publicGDInput(t, input), &out, sink, s.client)
			var auth *gendata.AuthenticationError
			if !errors.As(err, &auth) || strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatal("failure lost safe authentication boundary")
			}
			if phase == "warning" {
				if !errors.Is(err, cause) || err.Error() != "authentication failed: HaveAPI protocol warning output failed" {
					t.Fatal("warning failure was delayed or exposed raw cause")
				}
			}
			if e.Inventory != nil {
				t.Fatal("failed authentication reached inventory")
			}
			coreGDCheckTree(t, root, want)
		})
	}
}

func TestGenDataCommandChallengeAndLog(t *testing.T) {
	root := coreGDSeed(t)
	want := coreGDTree(t, root)
	coreGDReplace(want, coreGDContainerPath, coreGDEmptyContainers)
	description := strings.Replace(coreGDDescription, `"version":"2.0"`, `"version":"2.1"`, 1)
	steps := []coreGDStep{
		{method: "OPTIONS", uri: "/?describe=default", body: description},
		{method: "POST", uri: "/login", body: `{"status":true,"response":{"login":{"complete":false,"next_action":"device","token":"synthetic-step"}}}`},
		{method: "PUT", uri: "/device", body: coreGDLogin, before: func(r *http.Request) {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error("challenge input is not JSON")
			}
			want := map[string]any{"challenge": map[string]any{"token": "synthetic-step", "answer": "synthetic-answer", "approved": true}}
			if !reflect.DeepEqual(body, want) {
				t.Error("challenge input/protection/order differs")
			}
		}},
	}
	s := publicGDServer(t, steps)
	e := &Engine{Root: root, Context: context.Background(), Inventory: []Machine{}, Color: "never"}
	if err := e.OpenLog("gen-data-vpsadmin-containers"); err != nil {
		t.Fatal(err)
	}
	defer e.CloseLog(false)
	inv := publicGDInvocation(t, "containers")
	if err := e.LogGenDataCLI(inv); err != nil {
		t.Fatal(err)
	}
	// Line's read-ahead retains the password and two challenge answers. Editing
	// and Ruby stripping are done by the existing prompt/provider, only once.
	in := publicGDInput(t, " synthetic-user \nsynthetic-paX\bssword\nold\x15 synthetic-answer \ntrue\n")
	var out, warnings bytes.Buffer
	if err := e.genDataCommand(inv, in, &out, &warnings, s.client); err != nil {
		t.Fatal("challenge command failed:", err)
	}
	if out.String() != "User name: Password: \nAnswer: \napproved: " || strings.Count(warnings.String(), "The client uses protocol v2.0") != 1 {
		t.Fatal("challenge/prompt/warning presentation changed operational order")
	}
	log, err := os.ReadFile(e.Log.Name())
	if err != nil || bytes.Contains(log, []byte("synthetic-")) || bytes.Contains(log, []byte("Password:")) {
		t.Fatal("authentication input, prompt, token or response reached the log")
	}
	// Logging has its own files; compare only data effects here.
	data := coreGDTree(t, filepath.Join(root, "data"))
	expected := map[string]coreGDEntry{".": want["data"]}
	for p, entry := range want {
		if strings.HasPrefix(p, "data/") {
			expected[strings.TrimPrefix(p, "data/")] = entry
		}
	}
	if !reflect.DeepEqual(data, expected) {
		t.Fatal("challenge command changed unrelated generated data")
	}
}

func TestGenDataCommandInventoryFailure(t *testing.T) {
	// An ordinary missing configuration is observed only after successful auth.
	root := coreGDSeed(t)
	want := coreGDTree(t, root)
	s := publicGDServer(t, coreGDLoginSteps())
	e := &Engine{Root: root, Context: context.Background(), SettingsCache: map[string]any{}}
	err := e.genDataCommand(publicGDInvocation(t, "containers"), publicGDInput(t, "synthetic-user\nsynthetic-password\n"), io.Discard, io.Discard, s.client)
	if err == nil || !strings.Contains(err.Error(), "has no flake.nix") {
		t.Fatal("inventory did not fail at the reached configuration boundary")
	}
	coreGDCheckTree(t, root, want)
}

func TestGenDataCommandDeclaration(t *testing.T) {
	for _, leaf := range []string{"all", "containers", "network"} {
		inv := publicGDInvocation(t, leaf)
		if len(inv.Command.Options) != 0 || len(inv.Command.Arguments) != 0 {
			t.Fatal("gen-data gained options or positional arguments")
		}
		for _, extra := range []string{"extra", "--yes", "--endpoint=synthetic.invalid"} {
			if _, err := cli.BuiltinRegistry().Parse([]string{"gen-data", "vpsadmin", leaf, extra}); err == nil {
				t.Fatal("gen-data accepted an undeclared argument/option")
			}
		}
	}
}

type publicGDWriterError struct{ err error }

func (w publicGDWriterError) Write([]byte) (int, error) { return 0, w.err }

func TestGenDataCommandCancellation(t *testing.T) {
	for _, phase := range []string{"before-input", "request", "redirected-input"} {
		t.Run(phase, func(t *testing.T) {
			root := coreGDSeed(t)
			want := coreGDTree(t, root)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e := &Engine{Root: root, Context: ctx}
			input := publicGDInput(t, "synthetic-user\nsynthetic-password\n")
			if phase == "before-input" {
				cancel()
			}
			if phase == "redirected-input" {
				rd, wr, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer rd.Close()
				defer wr.Close()
				input = rd
			}
			factories, requests := 0, 0
			factory := func(prompt gendata.Prompt, warn func(string) error) (*gendata.VPSAdmin, error) {
				factories++
				return gendata.NewVPSAdminWithWarningBindings(prompt, warn, func(r *http.Request) (*http.Response, error) {
					requests++
					cancel()
					return nil, r.Context().Err()
				}, time.Now)
			}
			var output bytes.Buffer
			var out io.Writer = &output
			if phase == "redirected-input" {
				out = publicGDCancelWriter{cancel, &output}
			}
			err := e.genDataCommand(publicGDInvocation(t, "all"), input, out, io.Discard, factory)
			wantRequests := 0
			if phase == "request" {
				wantRequests = 1
			}
			if !errors.Is(err, context.Canceled) || factories != 1 || requests != wantRequests || e.Inventory != nil {
				t.Fatal("cancellation failed to stop authentication/inventory")
			}
			coreGDCheckTree(t, root, want)
		})
	}
}

type publicGDCancelWriter struct {
	cancel context.CancelFunc
	output io.Writer
}

func (w publicGDCancelWriter) Write(b []byte) (int, error) {
	w.cancel()
	return w.output.Write(b)
}

func TestGenDataCommandAllPartialFiles(t *testing.T) {
	for _, phase := range []string{"network-http", "network-render", "network-rename", "containers-open"} {
		t.Run(phase, func(t *testing.T) {
			root := coreGDSeed(t)
			want := coreGDTree(t, root)
			steps := coreGDLoginSteps()
			if phase == "containers-open" {
				// Projection is empty; the first open must fail before resource HTTP.
				if err := os.RemoveAll(filepath.Join(root, "data/vpsadmin")); err != nil {
					t.Fatal(err)
				}
				want = coreGDTree(t, root)
			} else {
				coreGDReplace(want, coreGDContainerPath, coreGDEmptyContainers)
				steps = append(steps, coreGDResourceStep())
				step := coreGDListStep(`[]`)
				switch phase {
				case "network-http":
					step.status = 503
					step.body = ""
				case "network-render":
					step.body = coreGDNetworks(`[{"ip_version":4,"address":"bad\u0000address","prefix":24}]`)
					x := want[coreGDNetworkPath+".new"]
					x.body = ""
					want[coreGDNetworkPath+".new"] = x
				case "network-rename":
					if err := os.Remove(filepath.Join(root, coreGDNetworkPath)); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(filepath.Join(root, coreGDNetworkPath), 0755); err != nil {
						t.Fatal(err)
					}
					want[coreGDNetworkPath] = coreGDEntry{kind: "directory", mode: os.ModeDir | 0755}
					x := want[coreGDNetworkPath+".new"]
					x.body = coreGDEmptyNetworks
					want[coreGDNetworkPath+".new"] = x
				}
				step.before = func(*http.Request) {
					b, err := os.ReadFile(filepath.Join(root, coreGDContainerPath))
					if err != nil || string(b) != coreGDEmptyContainers {
						t.Error("All did network work before saving empty containers")
					}
				}
				steps = append(steps, step)
			}
			s := publicGDServer(t, steps)
			e := &Engine{Root: root, Context: context.Background(), Inventory: []Machine{}}
			err := e.genDataCommand(publicGDInvocation(t, "all"), publicGDInput(t, "synthetic-user\nsynthetic-password\n"), io.Discard, io.Discard, s.client)
			if err == nil {
				t.Fatal("expected reached API/file failure")
			}
			if phase == "network-render" && !strings.Contains(err.Error(), "NUL") {
				t.Fatal("failure did not reach the intended formatter boundary")
			}
			coreGDCheckTree(t, root, want)
		})
	}
}

func TestGenDataCommandInvalidPath(t *testing.T) {
	e := &Engine{Root: t.TempDir(), Context: context.Background()}
	for _, path := range [][]string{{}, {"gen-data", "vpsadmin"}, {"gen-data", "other", "all"}, {"gen-data", "vpsadmin", "other"}} {
		inv := cli.Invocation{Command: cli.CommandSpec{Path: path}}
		err := e.genDataCommand(inv, nil, nil, nil, func(gendata.Prompt, func(string) error) (*gendata.VPSAdmin, error) {
			t.Fatal("invalid path constructed a client")
			return nil, nil
		})
		if err == nil {
			t.Fatal("invalid command accepted")
		}
	}
}
