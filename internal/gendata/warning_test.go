package gendata

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/ui"
)

const warningMessage = "The client uses protocol v2.0 while the API server uses a different same-major version"
const warningSafeError = "HaveAPI protocol warning output failed"

func TestVPSAdminWarningConstruction(t *testing.T) {
	called := func() { t.Fatal("construction reached a callback") }
	prompt := func(context.Context, string, bool) (string, error) { called(); return "", nil }
	sink := func(string) error { called(); return nil }
	do := func(*http.Request) (*http.Response, error) { called(); return nil, nil }
	now := func() time.Time { called(); return time.Time{} }
	for _, test := range []struct {
		warn func(string) error
		do   func(*http.Request) (*http.Response, error)
		now  func() time.Time
		want string
	}{
		{nil, nil, nil, "vpsAdmin warning binding is unavailable"}, {nil, do, now, "vpsAdmin warning binding is unavailable"},
		{sink, nil, nil, "vpsAdmin HTTP binding is unavailable"}, {sink, nil, now, "vpsAdmin HTTP binding is unavailable"},
		{sink, do, nil, "vpsAdmin clock binding is unavailable"},
	} {
		p, err := NewVPSAdminWithWarningBindings(prompt, test.warn, test.do, test.now)
		if p != nil || err == nil || err.Error() != test.want {
			t.Fatal("warning binding rejection order differs")
		}
	}
	if p, err := NewVPSAdminWithWarning(prompt, nil); p != nil || err == nil || err.Error() != "vpsAdmin warning binding is unavailable" {
		t.Fatal("ordinary warning binding not required")
	}
	p, err := NewVPSAdminWithWarning(prompt, sink)
	q, qerr := NewVPSAdminWithWarningBindings(prompt, sink, do, now)
	if err != nil || qerr != nil || p == q {
		t.Fatal("warning constructor failed")
	}
	for _, client := range []*VPSAdmin{p, q} {
		if client.origin.String() != productionAPI || client.budget != 60*time.Second || client.warn != nil || client.warnError == nil || client.authenticated || client.authDescription != nil || client.resources != nil || client.token != "" {
			t.Fatal("warning construction changed fixed/fresh policy")
		}
	}
	if _, ok := p.doer.(*http.Client); !ok {
		t.Fatal("ordinary HTTP owner replaced")
	}
	old := NewVPSAdmin(nil, nil)
	bound, err := NewVPSAdminWithBindings(nil, nil, do, now)
	if err != nil || old.warnError != nil || bound.warnError != nil {
		t.Fatal("legacy construction changed")
	}
}

func TestVPSAdminWarningVersion(t *testing.T) {
	for _, test := range []struct {
		name, version string
		warn, bad     bool
	}{
		{"exact", `"2.0"`, false, false}, {"minor", `"2.1"`, true, false}, {"suffix", `"2.fixture"`, true, false},
		{"major-text", `"2"`, true, false}, {"other-major", `"3.0"`, false, true}, {"null", `null`, false, true}, {"number", `2`, false, true}, {"missing", "", false, true},
	} {
		for _, mode := range []string{"legacy", "nil", "error", "both"} {
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				obj := map[string]json.RawMessage{}
				if test.version != "" {
					obj["version"] = json.RawMessage(test.version)
				}
				legacy, synchronous := 0, 0
				p := NewVPSAdmin(nil, nil)
				if mode == "legacy" || mode == "both" {
					p.warn = func(message string) {
						if message != warningMessage {
							t.Fatal("legacy message differs")
						}
						legacy++
					}
				}
				if mode == "error" || mode == "both" {
					p.warnError = func(message string) error {
						if message != warningMessage {
							t.Fatal("synchronous message differs")
						}
						synchronous++
						return nil
					}
				}
				err := p.descriptionVersion(obj)
				if (err != nil) != test.bad {
					t.Fatal("version admission changed")
				}
				wantLegacy, wantSync := 0, 0
				if test.warn {
					if mode == "legacy" {
						wantLegacy = 1
					}
					if mode == "error" || mode == "both" {
						wantSync = 1
					}
				}
				if legacy != wantLegacy || synchronous != wantSync {
					t.Fatal("warning exclusivity/reach changed")
				}
			})
		}
	}
	// Repeated reached real descriptions warn repeatedly; there is no suppression.
	count, closed := 0, 0
	p := warningFixture(t, nil, func(string) error {
		if closed != count+1 {
			t.Fatal("warning preceded body close")
		}
		count++
		return nil
	}, []warningReply{{body: warningDescription("2.1")}, {body: warningDescription("2.1")}}, &closed)
	if _, err := p.discover(context.Background(), false); err != nil {
		t.Fatal("first discovery failed")
	}
	if _, err := p.discover(context.Background(), false); err != nil || count != 2 {
		t.Fatal("repeated discovery suppressed warning")
	}
}

type warningReply struct {
	method, path, body string
	status             int
	before             func(*http.Request)
}
type warningBody struct {
	io.Reader
	close func()
}

func (b warningBody) Close() error { b.close(); return errors.New("synthetic ignored close cause") }
func warningDescription(version string) string {
	return strings.Replace(providerDescription, `"version":"2.0"`, `"version":"`+version+`"`, 1)
}
func warningFixture(t *testing.T, prompt Prompt, sink func(string) error, replies []warningReply, closed *int) *VPSAdmin {
	t.Helper()
	at := 0
	do := func(r *http.Request) (*http.Response, error) {
		if at >= len(replies) {
			t.Fatal("request escaped finite warning script")
		}
		reply := replies[at]
		at++
		method, path := reply.method, reply.path
		if method == "" {
			method = "OPTIONS"
		}
		if path == "" {
			path = "/?describe=default"
		}
		if r.Method != method || r.URL.RequestURI() != path || r.URL.Scheme != "https" || r.URL.Host != "api.vpsfree.cz" {
			t.Fatal("warning request phase/fixed origin changed")
		}
		if r.Method == "OPTIONS" && r.Body != nil {
			t.Fatal("warning discovery body changed")
		}
		if reply.before != nil {
			reply.before(r)
		}
		status := reply.status
		if status == 0 {
			status = 200
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: warningBody{strings.NewReader(reply.body), func() { *closed++ }}}, nil
	}
	p, err := NewVPSAdminWithWarningBindings(prompt, sink, do, func() time.Time { return time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal("warning fixture construction failed")
	}
	t.Cleanup(func() {
		if at != len(replies) {
			t.Error("warning request script incomplete")
		}
	})
	return p
}
func warningCredentialPrompt(context.Context, string, bool) (string, error) {
	return "synthetic-credential", nil
}
func warningAssertError(t *testing.T, err, cause error, auth bool) {
	t.Helper()
	var w *WarningError
	want := warningSafeError
	if auth {
		want = "authentication failed: " + want
		a, ok := err.(*AuthenticationError)
		if !ok {
			t.Fatal("warning lost auth classification")
		}
		w, _ = a.Unwrap().(*WarningError)
	} else {
		w, _ = err.(*WarningError)
	}
	if w == nil || !errors.Is(err, cause) || w.Unwrap() != cause || w.Error() != warningSafeError {
		t.Fatal("warning error identity/classification differs")
	}
	if err.Error() != want || strings.Contains(err.Error(), "synthetic-") {
		t.Fatal("warning cause escaped safe diagnostic")
	}
}

func TestVPSAdminWarningNestedAuthenticationCause(t *testing.T) {
	// Obtain the cause from an actual earlier attempt, rather than constructing
	// an auth wrapper that the warning API could never receive from its caller.
	priorPrompts, priorClosed := 0, 0
	priorClient := warningFixture(t, func(context.Context, string, bool) (string, error) {
		priorPrompts++
		return "", errors.New("synthetic-private-prior-input")
	}, func(string) error {
		t.Fatal("prior prompt failure reached warning")
		return nil
	}, nil, &priorClosed)
	prior := priorClient.Authenticate(context.Background())
	priorAuth, ok := prior.(*AuthenticationError)
	if !ok || prior.Error() != "authentication failed: authentication prompt failed" || priorPrompts != 1 || priorClosed != 0 {
		t.Fatal("prior actual authentication failure differs")
	}
	if authenticationError(nil) != nil || authenticationError(prior) != prior {
		t.Fatal("nil or already outer authentication boundary changed")
	}
	for _, wrapped := range []bool{false, true} {
		name := "direct"
		cause := prior
		if wrapped {
			name = "wrapped"
			cause = fmt.Errorf("synthetic-private-sink: %w", prior)
		}
		for _, stage := range []string{"auth", "resource-list", "resource-show"} {
			t.Run(name+"/"+stage, func(t *testing.T) {
				closed, prompts, warnings := 0, 0, 0
				replies := []warningReply{{body: warningDescription("2.1")}}
				if stage != "auth" {
					replies = []warningReply{
						{body: providerDescription},
						{method: "POST", path: "/auth/request", body: `{"status":true,"response":{"login":{"complete":true,"token":"synthetic-token"}}}`},
						{body: warningDescription("2.1")},
					}
				}
				p := warningFixture(t, func(context.Context, string, bool) (string, error) {
					prompts++
					return "synthetic-credential", nil
				}, func(message string) error {
					warnings++
					if message != warningMessage || closed != len(replies) {
						t.Fatal("nested-cause warning preceded body close or changed text")
					}
					return cause
				}, replies, &closed)
				err := p.Authenticate(context.Background())
				if stage == "auth" {
					if p.authDescription != nil || p.resources != nil || p.user != "" || p.password != "" || p.tokenHeader != "" || p.token != "" || p.validTo != nil || p.authenticated || p.authenticating {
						t.Fatal("failed nested-cause auth installed state")
					}
				} else {
					if err != nil {
						t.Fatal("nested-cause resource authentication setup failed")
					}
					if stage == "resource-list" {
						_, err = p.ListNetworks(context.Background())
					} else {
						_, err = p.ShowVPS(context.Background(), some(json.Number("1")))
					}
					if p.resources != nil || !p.authenticated || p.token != "synthetic-token" {
						t.Fatal("nested-cause resource warning changed auth/cache")
					}
				}
				if prompts != 2 || warnings != 1 || closed != len(replies) {
					t.Fatal("nested-cause warning repeated or skipped reached work")
				}
				warningAssertError(t, err, cause, stage == "auth")
				var currentWarning *WarningError
				var currentAuth *AuthenticationError
				if !errors.As(err, &currentWarning) || !errors.Is(err, prior) || !errors.As(currentWarning, &currentAuth) || currentAuth != priorAuth {
					t.Fatal("nested original authentication cause identity changed")
				}
				if currentWarning.Unwrap() != cause || !errors.Is(currentWarning, cause) || !errors.Is(currentWarning, priorAuth.Unwrap()) {
					t.Fatal("nested sink cause or earlier safe cause lost")
				}
				if stage == "auth" {
					outer := err.(*AuthenticationError)
					if outer == priorAuth || outer.Unwrap() != currentWarning || !errors.As(err, &currentAuth) || currentAuth != outer || authenticationError(err) != err {
						t.Fatal("current authentication wrapper is missing or duplicated")
					}
				}
			})
		}
	}
}

func TestVPSAdminWarningImmediateBoundary(t *testing.T) {
	cause := errors.New("synthetic-secret-writer")
	for _, stage := range []string{"auth", "resource-list", "resource-show"} {
		for _, malformed := range []bool{false, true} {
			t.Run(stage+map[bool]string{false: "/normal", true: "/malformed"}[malformed], func(t *testing.T) {
				closed, prompts, warnings := 0, 0, 0
				description := warningDescription("2.1")
				if malformed {
					description = `{"status":true,"version":"2.1","response":42}`
				}
				replies := []warningReply{{body: description}}
				if stage != "auth" {
					replies = []warningReply{{body: providerDescription}, {method: "POST", path: "/auth/request", body: `{"status":true,"response":{"login":{"complete":true,"token":"synthetic-token"}}}`}, {body: description, before: func(r *http.Request) {
						if r.Header.Get("X-Fixture-Token") != "synthetic-token" {
							t.Fatal("resource description lost token")
						}
					}}}
				}
				sink := func(message string) error {
					warnings++
					if message != warningMessage || closed != len(replies) {
						t.Fatal("warning ordering/body close changed")
					}
					return cause
				}
				p := warningFixture(t, func(context.Context, string, bool) (string, error) { prompts++; return "synthetic-credential", nil }, sink, replies, &closed)
				err := p.Authenticate(context.Background())
				if stage == "auth" {
					warningAssertError(t, err, cause, true)
					if p.authDescription != nil || p.user != "" || p.password != "" || p.tokenHeader != "" || p.token != "" || p.authenticated {
						t.Fatal("failed auth warning installed state")
					}
				} else {
					if err != nil {
						t.Fatal("initial authentication failed")
					}
					if stage == "resource-list" {
						_, err = p.ListNetworks(context.Background())
					} else {
						_, err = p.ShowVPS(context.Background(), some(json.Number("1")))
					}
					warningAssertError(t, err, cause, false)
					if p.resources != nil || !p.authenticated || p.token != "synthetic-token" {
						t.Fatal("resource failure changed prior auth/cache")
					}
				}
				if prompts != 2 || warnings != 1 {
					t.Fatal("warning reprompted or retried")
				}
			})
		}
	}
	// An exact version reaches the response-object validation instead of warning.
	closed := 0
	p := warningFixture(t, nil, func(string) error { t.Fatal("exact version warned"); return nil }, []warningReply{{body: `{"status":true,"version":"2.0","response":42}`}}, &closed)
	if _, err := p.discover(context.Background(), false); err == nil || err.Error() != "malformed API description" {
		t.Fatal("post-version validation changed")
	}
}

func TestVPSAdminWarningErrors(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(map[bool]string{false: "sink-error", true: "sink-success"}[success], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cause := errors.New("synthetic-secret-sink")
			closed := 0
			p := warningFixture(t, warningCredentialPrompt, func(string) error {
				cancel()
				if success {
					return nil
				}
				return cause
			}, []warningReply{{body: warningDescription("2.1")}}, &closed)
			err := p.Authenticate(ctx)
			if success {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("next reached auth request ignored cancellation")
				}
				var w *WarningError
				if errors.As(err, &w) {
					t.Fatal("successful warning became warning error")
				}
			} else {
				warningAssertError(t, err, cause, true)
				if errors.Is(err, context.Canceled) {
					t.Fatal("warning sink cause replaced by cancellation")
				}
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cause := errors.New("synthetic-resource-sink")
	closed := 0
	p := warningFixture(t, warningCredentialPrompt, func(string) error { cancel(); return cause }, []warningReply{{body: providerDescription}, {method: "POST", path: "/auth/request", body: `{"status":true,"response":{"login":{"complete":true,"token":"synthetic-token"}}}`}, {body: warningDescription("2.1")}}, &closed)
	if err := p.Authenticate(ctx); err != nil {
		t.Fatal("auth setup failed")
	}
	_, err := p.ListNetworks(ctx)
	warningAssertError(t, err, cause, false)
}

type warningWriter struct {
	output                  bytes.Buffer
	n                       int
	err                     error
	writes, flushes, closes int
}

func (w *warningWriter) Write(b []byte) (int, error) {
	w.writes++
	n := w.n
	if n < 0 {
		n = len(b)
	}
	if n <= len(b) && n >= 0 {
		w.output.Write(b[:n])
	}
	return n, w.err
}
func (w *warningWriter) Flush() error { w.flushes++; return errors.New("synthetic forbidden flush") }
func (w *warningWriter) Close() error { w.closes++; return errors.New("synthetic forbidden close") }
func TestWarningTo(t *testing.T) {
	cause := errors.New("synthetic writer error")
	body := warningMessage + "\n"
	for _, test := range []struct {
		name      string
		n         int
		err, want error
	}{
		{"full", -1, nil, nil}, {"zero-short", 0, nil, io.ErrShortWrite}, {"partial-short", 3, nil, io.ErrShortWrite},
		{"zero-error", 0, cause, cause}, {"partial-error", 3, cause, cause}, {"full-error", len(body), cause, cause},
	} {
		t.Run(test.name, func(t *testing.T) {
			writer := &warningWriter{n: test.n, err: test.err}
			writer.output.WriteString("prior")
			sink := WarningTo(writer)
			if writer.writes != 0 {
				t.Fatal("warning closure construction wrote")
			}
			err := sink(warningMessage)
			n := test.n
			if n < 0 {
				n = len(body)
			}
			if err != test.want || writer.writes != 1 || writer.flushes != 0 || writer.closes != 0 || writer.output.String() != "prior"+body[:n] {
				t.Fatal("warning single-write/error/residue changed")
			}
		})
	}
	if err := WarningTo(nil)(warningMessage); err == nil || err.Error() != "warning output is unavailable" {
		t.Fatal("nil warning writer skipped")
	}
}

func warningMemoryPrompt(t *testing.T, input string, output *bytes.Buffer, restores *int) *ui.GenDataPrompt {
	t.Helper()
	queue := bufio.NewReader(strings.NewReader(input))
	return ui.NewGenDataPrompt(ui.GenDataPromptIO{Writer: output, Flush: func() error { return nil }, Line: func(context.Context) (string, error) { return queue.ReadString('\n') }, BeginProtected: func(context.Context) (ui.GenDataProtectedInput, error) {
		return ui.GenDataProtectedInput{Character: func(context.Context) (string, error) {
			r, _, err := queue.ReadRune()
			if err != nil {
				return "", err
			}
			return string(r), nil
		}, Restore: func() error { *restores++; return nil }}, nil
	}})
}

func TestVPSAdminWarningPromptComposition(t *testing.T) {
	for _, failWarning := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordered-challenges", true: "warning-stops-auth"}[failWarning], func(t *testing.T) {
			var output bytes.Buffer
			restores, closed := 0, 0
			input := " \t\x00\u00a0synthetic-user\u00a0\t\x00\n\x00\t synthetic-password \t\x00\n +0009007199254740993 \t\nYES\n"
			prompt := warningMemoryPrompt(t, input, &output, &restores)
			description := providerDescription
			if failWarning {
				description = warningDescription("2.1")
			}
			requests := []string{}
			cause := errors.New("synthetic-secret-warning")
			replies := []warningReply{{body: description, before: func(*http.Request) { requests = append(requests, "description") }}}
			if !failWarning {
				replies = append(replies,
					warningReply{method: "POST", path: "/auth/request", body: `{"status":true,"response":{"login":{"complete":false,"token":"synthetic-step","next_action":"device"}}}`, before: func(r *http.Request) {
						requests = append(requests, "request")
						raw, err := io.ReadAll(r.Body)
						var got map[string]map[string]any
						d := json.NewDecoder(bytes.NewReader(raw))
						d.UseNumber()
						decode := d.Decode(&got)
						want := map[string]map[string]any{"credentials": {"user": "\u00a0synthetic-user\u00a0", "password": "synthetic-password", "lifetime": "fixed", "interval": json.Number("60")}}
						if err != nil || decode != nil || !reflect.DeepEqual(got, want) || r.Header.Get("X-Fixture-Token") != "" {
							t.Fatal("prompt raw/provider strip/credential phase differs")
						}
					}},
					warningReply{method: "PUT", path: "/auth/device", body: `{"status":true,"response":{"login":{"complete":true,"token":"synthetic-installed"}}}`, before: func(r *http.Request) {
						requests = append(requests, "device")
						raw, err := io.ReadAll(r.Body)
						var got map[string]map[string]any
						d := json.NewDecoder(bytes.NewReader(raw))
						d.UseNumber()
						decode := d.Decode(&got)
						want := map[string]map[string]any{"challenge": {"token": "synthetic-step", "answer": json.Number("9007199254740993"), "approved": true}}
						if err != nil || decode != nil || !reflect.DeepEqual(got, want) || r.Header.Get("X-Fixture-Token") != "" {
							t.Fatal("ordered prompt/challenge coercion phase differs")
						}
					}})
			}
			p := warningFixture(t, prompt.Ask, func(string) error {
				if !failWarning {
					t.Fatal("exact version warned")
				}
				if closed != 1 || restores != 1 {
					t.Fatal("warning preceded prompt restore/body close")
				}
				return cause
			}, replies, &closed)
			err := p.Authenticate(context.Background())
			if failWarning {
				warningAssertError(t, err, cause, true)
				if output.String() != "User name: Password: \n" || !reflect.DeepEqual(requests, []string{"description"}) || restores != 1 {
					t.Fatal("warning failed to stop later prompt/request")
				}
			} else {
				if err != nil || p.token != "synthetic-installed" || restores != 2 || output.String() != "User name: Password: \nAnswer: \napproved: " || !reflect.DeepEqual(requests, []string{"description", "request", "device"}) {
					t.Fatal("real prompt/provider composition differs")
				}
			}
		})
	}
}

func TestVPSAdminWarningPromptErrorLayers(t *testing.T) {
	for _, kind := range []string{"interrupt", "read", "restore", "canceled-restore"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var output bytes.Buffer
			restores := 0
			readErr := errors.New("synthetic-secret-read")
			restoreErr := errors.New("synthetic-secret-restore")
			ops := ui.GenDataPromptIO{Writer: &output, Flush: func() error { return nil }, Line: func(context.Context) (string, error) { return "synthetic-user\n", nil }, BeginProtected: func(context.Context) (ui.GenDataProtectedInput, error) {
				return ui.GenDataProtectedInput{Character: func(context.Context) (string, error) {
					if kind == "interrupt" {
						return "\x03", nil
					}
					if kind == "canceled-restore" {
						cancel()
					}
					return "synthetic-answer", readErr
				}, Restore: func() error {
					restores++
					if kind == "restore" || kind == "canceled-restore" {
						return restoreErr
					}
					return nil
				}}, nil
			}}
			prompt := ui.NewGenDataPrompt(ops)
			_, err := prompt.Ask(ctx, "Password: ", true)
			want := readErr
			if kind == "interrupt" {
				want = ui.ErrGenDataPromptInterrupted
			}
			if kind == "restore" || kind == "canceled-restore" {
				want = restoreErr
			}
			if err != want || restores != 1 || output.String() != "Password: " {
				t.Fatal("Ask restore/interrupt identity differs")
			}
			// Reuse the actual callback owner; the provider intentionally redacts Ask's
			// cause or chooses its existing context error rather than exposing it.
			output.Reset()
			closed := 0
			p := warningFixture(t, prompt.Ask, func(string) error { t.Fatal("prompt failure reached warning"); return nil }, nil, &closed)
			err = p.Authenticate(ctx)
			var a *AuthenticationError
			wantMessage := "authentication failed: authentication prompt failed"
			if kind == "canceled-restore" {
				wantMessage = "authentication failed: context canceled"
			}
			if !errors.As(err, &a) || err.Error() != wantMessage || errors.Is(err, want) || restores != 2 || output.String() != "User name: Password: " {
				t.Fatal("provider prompt redaction/context layer changed")
			}
			if kind == "canceled-restore" && !errors.Is(err, context.Canceled) {
				t.Fatal("existing auth context cause lost")
			}
		})
	}
}

type warningTreeEntry struct {
	kind       string
	mode       os.FileMode
	body, link string
}

func warningTree(t *testing.T, root string) map[string]warningTreeEntry {
	t.Helper()
	tree := map[string]warningTreeEntry{}
	err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		s, err := os.Lstat(path)
		if err != nil {
			return err
		}
		x := warningTreeEntry{mode: s.Mode()}
		switch {
		case s.Mode()&os.ModeSymlink != 0:
			x.kind = "link"
			x.link, err = os.Readlink(path)
		case s.IsDir():
			x.kind = "directory"
		default:
			x.kind = "file"
			var body []byte
			body, err = os.ReadFile(path)
			x.body = string(body)
		}
		tree[rel] = x
		return err
	})
	if err != nil {
		t.Fatal("disposable tree read failed")
	}
	return tree
}
func TestVPSAdminWarningAllResidue(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "data/vpsadmin/networks"), 0755); err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{containersFile: "old-container", containersFile + ".new": "stale-container", networksFile: "old-network", networksFile + ".new": "stale-network", "sentinel": "unrelated"} {
		path := filepath.Join(root, rel)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("sentinel", filepath.Join(root, "contained-link")); err != nil {
		t.Fatal(err)
	}
	before := warningTree(t, root)
	var output bytes.Buffer
	restores, closed := 0, 0
	cause := errors.New("synthetic-secret-warning-writer")
	prompt := warningMemoryPrompt(t, "synthetic-user\nsynthetic-password\n", &output, &restores)
	writer := &warningWriter{n: 3, err: cause}
	sink := WarningTo(writer)
	p := warningFixture(t, prompt.Ask, func(message string) error {
		if closed != 3 {
			t.Fatal("resource warning preceded body close")
		}
		body, err := os.ReadFile(filepath.Join(root, containersFile))
		if err != nil || string(body) != "{\n}\n" {
			t.Fatal("warning preceded completed first file")
		}
		if _, err := os.Lstat(filepath.Join(root, containersFile+".new")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("first sibling not renamed before warning")
		}
		return sink(message)
	}, []warningReply{{body: providerDescription}, {method: "POST", path: "/auth/request", body: `{"status":true,"response":{"login":{"complete":true,"token":"synthetic-token"}}}`}, {body: warningDescription("2.1")}}, &closed)
	if err := p.Authenticate(context.Background()); err != nil {
		t.Fatal("All authentication failed")
	}
	renders := 0
	g := Generator{Root: root, Source: p, Literal: func(value any) (string, error) {
		renders++
		if !reflect.DeepEqual(value, map[string]any{}) {
			t.Fatal("unexpected empty-container projection")
		}
		return "{\n}", nil
	}}
	err := g.All(context.Background(), nil)
	if err == nil || err.Error() != "list networks: "+warningSafeError {
		t.Fatal("existing Generator network wrapper changed")
	}
	warningAssertError(t, errors.Unwrap(err), cause, false)
	if renders != 1 || writer.writes != 1 || writer.output.String() != warningMessage[:3] || restores != 1 || p.resources != nil {
		t.Fatal("warning All effects/retry/cache differs")
	}
	x := before[containersFile+".new"]
	x.body = "{\n}\n"
	before[containersFile] = x
	delete(before, containersFile+".new")
	if !reflect.DeepEqual(warningTree(t, root), before) {
		t.Fatal("complete All residue tree differs")
	}
}
