package kexecnetboot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type nomination struct {
	ID          string            `json:"id"`
	Argv        []string          `json:"argv"`
	Environment map[string]string `json:"environment"`
	Stdin       *string           `json:"stdin_bytes"`
	Literal     struct {
		FQDN    string `json:"compiled_machine_fqdn"`
		Cmdline string `json:"cmdline_bytes"`
		Index   struct {
			Status  int    `json:"http_status"`
			Fixture string `json:"fixture"`
		} `json:"index_response"`
		Downloads []struct {
			Status int    `json:"http_status"`
			Body   string `json:"body_bytes"`
		} `json:"download_responses"`
		Exit           int            `json:"kexec_exit"`
		Signal         map[string]any `json:"kexec_status"`
		UnlinkError    string         `json:"temp_unlink_error"`
		TransportError string         `json:"index_transport_error"`
	} `json:"literal_inputs"`
}

func nominations(t *testing.T) []nomination {
	t.Helper()
	b, err := os.ReadFile("testdata/kn-nominations.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Cases []nomination `json:"cases"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Cases) != 45 {
		t.Fatal(len(d.Cases))
	}
	return d.Cases
}

type fakeRun struct {
	d                          Dependencies
	out, err                   bytes.Buffer
	requests, removed, created []string
	argv                       [][]string
	bodyCloses                 int
	payloadChecks              int
	root                       string
}

func mockRun(t *testing.T, n nomination) *fakeRun {
	t.Helper()
	f := &fakeRun{root: t.TempDir()}
	input := ""
	if n.Stdin != nil {
		input = *n.Stdin
	}
	idx := 0
	f.d = Dependencies{ReadCmdline: func() ([]byte, error) { return []byte(n.Literal.Cmdline), nil }, In: strings.NewReader(input), Out: &f.out, Err: &f.err, Request: func(_ context.Context, raw string) (*http.Response, error) {
		f.requests = append(f.requests, raw)
		if len(f.requests) == 1 {
			if n.Literal.TransportError != "" {
				return nil, errors.New(n.Literal.TransportError)
			}
			b, err := os.ReadFile("testdata/" + n.Literal.Index.Fixture)
			if err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: n.Literal.Index.Status, Body: &countBody{Reader: bytes.NewReader(b), close: func() { f.bodyCloses++ }}}, nil
		}
		if idx >= len(n.Literal.Downloads) {
			t.Fatal("unexpected content request", raw)
		}
		r := n.Literal.Downloads[idx]
		idx++
		return &http.Response{StatusCode: r.Status, Body: &countBody{Reader: strings.NewReader(r.Body), close: func() { f.bodyCloses++ }}}, nil
	}, CreateTemp: func(prefix string) (Temp, error) {
		p := filepath.Join(f.root, fmt.Sprintf("%s-%d", prefix, len(f.created)))
		file, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return Temp{}, err
		}
		f.created = append(f.created, p)
		return Temp{p, file, file.Close}, nil
	}, Remove: func(p string) error {
		f.removed = append(f.removed, p)
		if n.Literal.UnlinkError != "" {
			return errors.New(n.Literal.UnlinkError)
		}
		return os.Remove(p)
	}, Child: func(_ context.Context, a []string) error {
		f.argv = append(f.argv, append([]string(nil), a...))
		if len(a) > 1 && a[1] == "--load" && n.Literal.Exit == 0 && n.Literal.Signal == nil {
			if len(a) != 6 || !strings.HasPrefix(a[3], "--initrd=") {
				t.Fatalf("load argv %q", a)
			}
			paths := []string{a[2], strings.TrimPrefix(a[3], "--initrd=")}
			if paths[0] == paths[1] || len(n.Literal.Downloads) != 2 {
				t.Fatal("distinct kernel/initrd files required", paths)
			}
			for i, path := range paths {
				b, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(b, []byte(n.Literal.Downloads[i].Body)) {
					t.Fatalf("load payload %d at %s: got%q want%q err%v", i, path, b, n.Literal.Downloads[i].Body, err)
				}
				f.payloadChecks++
			}
		}
		if n.Literal.Exit != 0 || n.Literal.Signal != nil {
			return &exec.ExitError{}
		}
		return nil
	}}
	return f
}

func TestDownloadPayloadBytesAtChild(t *testing.T) {
	for _, name := range []string{"nominated", "chunked", "empty-kernel", "empty-initrd", "both-empty"} {
		t.Run(name, func(t *testing.T) {
			n := nominations(t)[0]
			if name == "empty-kernel" || name == "both-empty" {
				n.Literal.Downloads[0].Body = ""
			}
			if name == "empty-initrd" || name == "both-empty" {
				n.Literal.Downloads[1].Body = ""
			}
			f := mockRun(t, n)
			if name == "chunked" {
				request := f.d.Request
				f.d.Request = func(ctx context.Context, raw string) (*http.Response, error) {
					r, err := request(ctx, raw)
					if err != nil || len(f.requests) == 1 {
						return r, err
					}
					_ = r.Body.Close()
					body := n.Literal.Downloads[len(f.requests)-2].Body
					r.Body = &countBody{Reader: io.MultiReader(
						strings.NewReader(body[:3]), strings.NewReader(body[3:9]), strings.NewReader(body[9:]),
					), close: func() { f.bodyCloses++ }}
					return r, nil
				}
			}
			status, err := Run(context.Background(), Config{"node.test", "/fixture/kexec"}, Options{}, f.d)
			if status != 0 || err != nil || f.payloadChecks != 2 || len(f.argv) != 1 {
				t.Fatal(status, err, f.payloadChecks, f.argv)
			}
			if !reflect.DeepEqual(f.created, f.removed) {
				t.Fatal("cleanup tracking", f.created, f.removed)
			}
			for _, path := range f.created {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("owned file survived cleanup", path, err)
				}
			}
		})
	}
}

type countBody struct {
	io.Reader
	close func()
}

func (b *countBody) Close() error { b.close(); return nil }

func TestAll45SourceNominationsWithApprovedDifferences(t *testing.T) {
	failure := map[string]bool{"KN04": true, "KN08": true, "KN09": true, "KN14": true, "KN17": true, "KN18": true, "KN19": true, "KN23": true, "KN24": true, "KN25": true, "KN26": true, "KN32": true, "KN33": true, "KN34": true, "KN35": true, "KN38": true, "KN39": true, "KN40": true, "KN42": true, "KN43": true}
	for _, n := range nominations(t) {
		t.Run(n.ID, func(t *testing.T) {
			f := mockRun(t, n)
			o, p := Parse("/literal/kexec-netboot", n.Argv, false)
			status := 0
			var err error
			if p != nil {
				status = p.Status
				f.out.WriteString(p.Stdout)
				f.err.WriteString(p.Stderr)
			} else {
				status, err = Run(context.Background(), Config{n.Literal.FQDN, "/fixture/kexec"}, o, f.d)
			}
			id := n.ID[:4]
			want := 0
			if failure[id] {
				want = 1
			}
			if status != want {
				t.Fatalf("status=%d want%d err=%v stdout=%q stderr=%q", status, want, err, f.out.String(), f.err.String())
			}
			for _, path := range f.created {
				if n.Literal.UnlinkError == "" {
					if _, e := os.Stat(path); !os.IsNotExist(e) {
						t.Fatal("owned file residue", path, e)
					}
				}
			}
			if !reflect.DeepEqual(f.created, f.removed) {
				t.Fatalf("tracking created=%v removed=%v", f.created, f.removed)
			}
			if id == "KN02" || id == "KN03" || id == "KN05" {
				if len(f.requests) != 0 || len(f.created) != 0 || len(f.argv) != 1 || f.argv[0][len(f.argv[0])-1] != "--no-sync" {
					t.Fatal("dispatch effects", f.requests, f.argv)
				}
			}
			if id == "KN13" && f.requests[0] != "https://netboot.test:8443/prefix/machines.json" {
				t.Fatal("HTTPS derivation changed", f.requests)
			}
			if id == "KN15" || id == "KN16" || id == "KN20" || id == "KN22" {
				if len(f.requests) < 2 || !strings.Contains(f.requests[1], "/6/bzImage") {
					t.Fatal("selection", f.requests)
				}
			}
			if id == "KN29" {
				if !strings.Contains(f.err.String(), "Requested variant 'missing' not found") || strings.Contains(f.argv[0][4], "runlevel=") {
					t.Fatal("unknown variant did not continue without variant", f.err.String(), f.argv)
				}
			}
			if id == "KN36" || id == "KN37" || id == "KN01" {
				a := f.argv[0]
				if len(a) != 6 || a[4] != "--append=httproot=http://netboot.test/node.test/7/initrd init=/fixture/store/system-7/init console=ttyS0 "+o.Append {
					t.Fatalf("actual argv %q", a)
				}
			}
			if id == "KN38" && len(f.created) != 2 {
				t.Fatal("failed second request not tracked", f.created)
			}
			if id == "KN25" && len(f.requests) != 1 {
				t.Fatal("redirect followed")
			}
			// KN39/40 inject a synthetic child ExitError; actual exit/signal state is
			// tested independently with an owned fake executable, never real kexec.
		})
	}
}

func TestDiscoveryLiteralPathsAndScheme(t *testing.T) {
	for _, tc := range []struct{ cmdline, override, want string }{{"x httproot=http://host/prefix/node/7/initrd?x#f", "", "http://host/prefix/machines.json"}, {"httproot=https://host:8443/a/b", "", "https://host:8443/a/b/machines.json"}, {"httproot=http://u:p@host/a%2Fb/n/7/initrd", "", "http://host/a%2Fb/machines.json"}, {"httproot=http://host///a///b/", "", "http://host/a/b/machines.json"}, {"httproot=http://host/n/7/initrd", "http://other/pre?literal=x", "http://other/pre?literal=x/machines.json"}, {"prefixhttproot=no httproot=http://host/n/7/initrd\vhttproot=http://ignored/x", "", "http://host/machines.json"}} {
		var override *string
		if tc.override != "" {
			override = &tc.override
		}
		got, err := machinesURL([]byte(tc.cmdline), override)
		if err != nil || got != tc.want {
			t.Fatalf("%q %q got%q want%q err%v", tc.cmdline, tc.override, got, tc.want, err)
		}
	}
	if _, err := machinesURL([]byte("none"), strptr("http://override")); err == nil {
		t.Fatal("override bypassed required proc parameter")
	}
}

func TestDownloadFailuresOwnershipAndErrorPrecedence(t *testing.T) {
	base := nominations(t)[0]
	for _, phase := range []string{"create", "request1", "request2", "read", "write", "short-write", "close", "child-start", "child-wait", "cancel"} {
		t.Run(phase, func(t *testing.T) {
			f := mockRun(t, base)
			sentinel := filepath.Join(f.root, "caller-owned")
			if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			boom := errors.New("synthetic " + phase)
			origCreate, origRequest := f.d.CreateTemp, f.d.Request
			contents := 0
			f.d.Request = func(ctx context.Context, raw string) (*http.Response, error) {
				if strings.HasSuffix(raw, "machines.json") {
					return origRequest(ctx, raw)
				}
				contents++
				if (phase == "request1" && contents == 1) || (phase == "request2" && contents == 2) {
					return nil, boom
				}
				r, err := origRequest(ctx, raw)
				if err != nil {
					return nil, err
				}
				if phase == "read" {
					r.Body = &countBody{Reader: &failReader{err: boom}, close: func() { f.bodyCloses++ }}
				}
				return r, nil
			}
			f.d.CreateTemp = func(prefix string) (Temp, error) {
				if phase == "create" {
					return Temp{}, boom
				}
				v, err := origCreate(prefix)
				if err != nil {
					return v, err
				}
				if phase == "write" {
					v.Writer = failWriter{err: boom}
				}
				if phase == "short-write" {
					v.Writer = shortWriter{}
				}
				if phase == "close" {
					close := v.Close
					v.Close = func() error { _ = close(); return boom }
				}
				return v, nil
			}
			if phase == "child-start" || phase == "child-wait" || phase == "cancel" {
				f.d.Child = func(context.Context, []string) error {
					if phase == "cancel" {
						return context.Canceled
					}
					return boom
				}
			}
			remove := f.d.Remove
			f.d.Remove = func(p string) error { _ = remove(p); return errors.New("secondary cleanup") }
			status, err := Run(context.Background(), Config{"node.test", "/fixture/kexec"}, Options{}, f.d)
			if status != 1 || err == nil {
				t.Fatal(status, err)
			}
			if phase == "short-write" {
				if !errors.Is(err, io.ErrShortWrite) {
					t.Fatal(err)
				}
			} else if phase == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, boom) {
				t.Fatal("primary error lost", err)
			}
			if !reflect.DeepEqual(f.created, f.removed) {
				t.Fatal("cleanup abandoned owned file", f.created, f.removed)
			}
			b, e := os.ReadFile(sentinel)
			if e != nil || string(b) != "keep" {
				t.Fatal("caller sentinel removed")
			}
		})
	}
}

type failReader struct {
	err  error
	done bool
}

func (r *failReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		copy(p, "part")
		return 4, nil
	}
	return 0, r.err
}

type failWriter struct{ err error }

func (w failWriter) Write([]byte) (int, error) { return 0, w.err }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestNativeTempExclusivityAndTMPDIR(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	d := OSDependencies()
	file, err := d.CreateTemp("bzImage")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(file.Path)
	defer file.Close()
	i, err := os.Stat(file.Path)
	if err != nil || i.Mode().Perm() != 0600 || filepath.Dir(file.Path) != root {
		t.Fatal(file.Path, i, err)
	}
	if _, err := os.OpenFile(file.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600); !os.IsExist(err) {
		t.Fatal("not exclusively created", err)
	}
}

func TestActionContextBoundaryAndCleanup(t *testing.T) {
	for _, name := range []string{"load", "exec", "unload", "load-child-fails", "exec-child-fails", "unload-child-fails", "request-fails", "create-fails", "conflict", "index-fails", "machine-missing", "machine-prompt-eof", "final-prompt-eof", "abort"} {
		t.Run(name, func(t *testing.T) {
			f := mockRun(t, nominations(t)[0])
			options := Options{}
			switch name {
			case "exec", "exec-child-fails":
				options.Exec = true
			case "unload", "unload-child-fails":
				options.Unload = true
			case "conflict":
				options.Exec, options.Unload = true, true
			case "machine-missing":
				options.Machine = strptr("missing.test")
			case "machine-prompt-eof":
				options.Interactive = true
			case "final-prompt-eof", "abort":
				options.Interactive, options.Machine, options.Generation = true, strptr("node.test"), strptr("7")
				if name == "abort" {
					f.d.In = strings.NewReader("n\n")
				}
			}
			type contextKey struct{}
			parent := context.WithValue(context.Background(), contextKey{}, "parent")
			starts, stops := 0, 0
			var trace []string
			f.d.BeginActions = func(ctx context.Context) (context.Context, context.CancelFunc) {
				starts++
				if ctx != parent {
					t.Error("external parent context changed")
				}
				trace = append(trace, "start")
				return context.WithValue(ctx, contextKey{}, "action"), func() {
					stops++
					if !reflect.DeepEqual(f.created, f.removed) {
						t.Error("action context stopped before owned cleanup", f.created, f.removed)
					}
					trace = append(trace, "stop")
				}
			}
			request, create, child, remove := f.d.Request, f.d.CreateTemp, f.d.Child, f.d.Remove
			boom := errors.New("synthetic action boundary failure")
			f.d.Request = func(ctx context.Context, raw string) (*http.Response, error) {
				if strings.HasSuffix(raw, "machines.json") {
					if ctx != parent || starts != 0 {
						t.Error("action context installed before index selection")
					}
					if name == "index-fails" {
						return nil, boom
					}
				} else {
					if ctx.Value(contextKey{}) != "action" || starts != 1 || stops != 0 {
						t.Error("download outside action context")
					}
					trace = append(trace, "request")
					if name == "request-fails" {
						return nil, boom
					}
				}
				return request(ctx, raw)
			}
			f.d.CreateTemp = func(prefix string) (Temp, error) {
				if starts != 1 || stops != 0 {
					t.Error("temporary creation outside action context")
				}
				trace = append(trace, "create")
				if name == "create-fails" {
					return Temp{}, boom
				}
				return create(prefix)
			}
			f.d.Child = func(ctx context.Context, argv []string) error {
				if ctx.Value(contextKey{}) != "action" || starts != 1 || stops != 0 {
					t.Error("child outside action context")
				}
				trace = append(trace, "child")
				if strings.HasSuffix(name, "child-fails") {
					return boom
				}
				return child(ctx, argv)
			}
			f.d.Remove = func(path string) error {
				if starts != 1 || stops != 0 {
					t.Error("owned cleanup outside action context")
				}
				trace = append(trace, "remove")
				return remove(path)
			}
			status, err := Run(parent, Config{"node.test", "/fixture/kexec"}, options, f.d)
			wantStart := 1
			switch name {
			case "conflict", "index-fails", "machine-missing", "machine-prompt-eof", "final-prompt-eof", "abort":
				wantStart = 0
			}
			wantStatus := 1
			if name == "load" || name == "exec" || name == "unload" {
				wantStatus = 0
			}
			if starts != wantStart || stops != wantStart || status != wantStatus {
				t.Fatal(starts, stops, status, err, trace)
			}
			if wantStart == 1 && (trace[0] != "start" || trace[len(trace)-1] != "stop") {
				t.Fatal("incorrect action context lifetime", trace)
			}
			if strings.HasSuffix(name, "-fails") && !errors.Is(err, boom) {
				t.Fatal("original failure lost", err)
			}
		})
	}
}
