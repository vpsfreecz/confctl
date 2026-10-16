package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	cli "github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/gendata"
)

// Finite generated-v7 action replies; every request remains script-confined.
const coreGDLogin = `{"status":true,"response":{"token":{"complete":true,"token":"synthetic-token"}}}`
const coreGDNode = `{"_meta":{"resolved":true,"path_params":"unused"},"id":9007199254740993,"name":"node","domain_name":"n","location":{"_meta":{"resolved":true},"domain":"loc","environment":{"_meta":{"resolved":true},"domain":"test"}}}`
const coreGDContainers = "{\n  \"z.loc.test\" = {\n    node = {\n      domain = \"test\";\n      fqdn = \"n.test\";\n      id = 9007199254740993;\n      location = \"loc\";\n      name = \"node\";\n    };\n  };\n}\n"
const coreGDEmptyContainers = "{\n}\n"
const coreGDEmptyNetworks = "{\n  \"ipv4\" = [\n  ];\n  \"ipv6\" = [\n  ];\n}\n"
const coreGDContainerPath = "data/vpsadmin/containers.nix"
const coreGDNetworkPath = "data/vpsadmin/networks/containers.nix"

func coreGDVPS(node string) string { return `{"status":true,"response":{"vps":{"node":` + node + `}}}` }
func coreGDNetworks(rows string) string {
	return `{"status":true,"response":{"networks":` + rows + `}}`
}
func coreGDMachine(id any) Machine {
	return Machine{Name: "diagnostic", Attributes: map[string]any{"container": map[string]any{"id": id}, "host": map[string]any{"name": "z", "location": "loc", "domain": "test"}}}
}

type coreGDStep struct {
	method, uri, token, body string
	status                   int
	failure                  error
	before                   func(*http.Request)
}
type coreGDBody struct {
	io.Reader
	close func()
}

func (b coreGDBody) Close() error { b.close(); return errors.New("synthetic ignored body-close error") }

type coreGDScript struct {
	t             *testing.T
	steps         []coreGDStep
	at, factories int
	trace         []string
	bindings      genDataBindings
	now           time.Time
}

func coreGDLoginSteps() []coreGDStep {
	return []coreGDStep{{method: "POST", uri: "/_auth/token/tokens", body: coreGDLogin}}
}
func coreGDShowStep(id, node string) coreGDStep {
	return coreGDStep{method: "GET", uri: "/v7.0/vpses/" + id + "?_meta%5Bincludes%5D=node__location__environment", token: "synthetic-token", body: coreGDVPS(node)}
}
func coreGDListStep(rows string) coreGDStep {
	return coreGDStep{method: "GET", uri: "/v7.0/networks", token: "synthetic-token", body: coreGDNetworks(rows)}
}
func coreGDNewScript(t *testing.T, steps []coreGDStep, machines []Machine) *coreGDScript {
	t.Helper()
	s := &coreGDScript{t: t, steps: steps, now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	s.bindings.Prompt = func(_ context.Context, label string, protected bool) (string, error) {
		s.trace = append(s.trace, "prompt "+label)
		switch label {
		case "User name: ":
			if protected {
				t.Fatal("username protection changed")
			}
			return " synthetic-user \t", nil
		case "Password: ":
			if !protected {
				t.Fatal("password protection changed")
			}
			return "synthetic-password", nil
		case "New password: ":
			if !protected {
				t.Fatal("challenge protection changed")
			}
			return "synthetic-answer", nil
		case "Repeat new password: ":
			if !protected {
				t.Fatal("reset confirmation protection changed")
			}
			return "yes", nil
		}
		t.Fatal("undeclared prompt phase")
		return "", errors.New("unexpected prompt")
	}
	s.bindings.Warn = func(string) { s.trace = append(s.trace, "warning") }
	s.bindings.Machines = func(context.Context) ([]Machine, error) { s.trace = append(s.trace, "inventory"); return machines, nil }
	s.bindings.NewClient = func(prompt gendata.Prompt, warn func(string)) (*gendata.VPSAdmin, error) {
		s.factories++
		s.trace = append(s.trace, "factory")
		return gendata.NewVPSAdminWithBindings(prompt, warn, s.do, func() time.Time { s.trace = append(s.trace, "clock"); return s.now })
	}
	return s
}
func (s *coreGDScript) do(r *http.Request) (*http.Response, error) {
	s.t.Helper()
	if s.at >= len(s.steps) {
		s.t.Fatal("request escaped finite script")
	}
	step := s.steps[s.at]
	s.at++
	if r.Method != step.method || r.URL.RequestURI() != step.uri || r.URL.Scheme != "https" || r.URL.Host != "api.vpsfree.cz" || r.Header.Get("X-HaveAPI-Auth-Token") != step.token || r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") != "haveapi-client" {
		s.t.Fatal("request method/origin/query/header phase changed")
	}
	if r.URL.Path == "/_auth/token/tokens" || r.URL.Path == "/_auth/token/tokens/reset_password" {
		raw, err := io.ReadAll(r.Body)
		var value map[string]map[string]any
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.UseNumber()
		decodeErr := decoder.Decode(&value)
		if err != nil || decodeErr != nil {
			s.t.Fatal("auth body unreadable")
		}
		want := map[string]map[string]any{"_meta": {}, "token": {"user": "synthetic-user", "password": "synthetic-password", "lifetime": "fixed", "interval": json.Number("60")}}
		if r.URL.Path == "/_auth/token/tokens/reset_password" {
			want = map[string]map[string]any{"_meta": {}, "token": {"token": "synthetic-step", "new_password1": "synthetic-answer", "new_password2": "yes"}}
		}
		if !reflect.DeepEqual(value, want) {
			s.t.Fatal("auth namespace/fixed-lifetime/coercion changed")
		}
	}
	if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > 60*time.Second {
		s.t.Fatal("request total budget absent")
	}
	s.trace = append(s.trace, step.method+" "+step.uri)
	if step.before != nil {
		step.before(r)
	}
	status := step.status
	if status == 0 {
		status = 200
	}
	response := &http.Response{StatusCode: status, Header: http.Header{}, Body: coreGDBody{strings.NewReader(step.body), func() { s.trace = append(s.trace, "close") }}}
	return response, step.failure
}
func (s *coreGDScript) done() {
	s.t.Helper()
	if s.at != len(s.steps) || s.factories != 1 {
		s.t.Fatal("script/factory phase count changed")
	}
}

type coreGDEntry struct {
	kind       string
	mode       os.FileMode
	body, link string
}

func coreGDTree(t *testing.T, root string) map[string]coreGDEntry {
	t.Helper()
	out := map[string]coreGDEntry{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		x := coreGDEntry{mode: info.Mode()}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			x.kind = "link"
			x.link, err = os.Readlink(p)
		case info.IsDir():
			x.kind = "directory"
		default:
			x.kind = "file"
			var b []byte
			b, err = os.ReadFile(p)
			x.body = string(b)
		}
		out[rel] = x
		return err
	})
	if err != nil {
		t.Fatal("fixture tree read failed")
	}
	return out
}
func coreGDCheckTree(t *testing.T, root string, want map[string]coreGDEntry) {
	t.Helper()
	if !reflect.DeepEqual(coreGDTree(t, root), want) {
		t.Fatal("complete disposable file/link/mode tree differs")
	}
}
func coreGDWrite(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal("fixture write failed")
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal("fixture mode failed")
	}
}
func coreGDSeed(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "data/vpsadmin/networks"), 0755); err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{coreGDContainerPath: "old-container", coreGDContainerPath + ".new": "stale-container", coreGDNetworkPath: "old-network", coreGDNetworkPath + ".new": "stale-network", "sentinel": "unrelated"} {
		coreGDWrite(t, filepath.Join(root, rel), body, 0640)
	}
	return root
}
func coreGDReplace(want map[string]coreGDEntry, rel, body string) {
	x := want[rel+".new"]
	x.kind = "file"
	x.body = body
	x.link = ""
	want[rel] = x
	delete(want, rel+".new")
}
func coreGDAuthTrace() []string {
	return []string{"factory", "prompt User name: ", "prompt Password: ", "POST /_auth/token/tokens", "close"}
}

func TestGenDataCoreConstructionAndAuth(t *testing.T) {
	sentinel := errors.New("synthetic binding failure")
	for _, test := range []struct {
		name string
		leaf genDataLeaf
		kind string
		want string
	}{
		{"unknown", 0, "unknown", "unknown gen-data leaf"}, {"missing-client", genDataAll, "missing-client", "gen-data client binding is unavailable"},
		{"factory-error", genDataAll, "factory-error", "synthetic binding failure"}, {"nil-client", genDataAll, "nil-client", "gen-data client binding returned no provider"},
		{"nil-prompt", genDataAll, "nil-prompt", "authentication failed: authentication prompt is unavailable"},
		{"username", genDataAll, "username", "authentication failed: authentication prompt failed"}, {"password", genDataAll, "password", "authentication failed: authentication prompt failed"},
		{"token", genDataAll, "token", "authentication failed: API action failed"},
		{"missing-inventory", genDataAll, "missing-inventory", "gen-data machine binding is unavailable"}, {"inventory-error", genDataContainers, "inventory-error", "synthetic binding failure"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := coreGDSeed(t)
			before := coreGDTree(t, root)
			steps := coreGDLoginSteps()
			switch test.kind {
			case "unknown", "missing-client", "factory-error", "nil-client", "nil-prompt", "username", "password":
				steps = nil
			case "token":
				steps[0].body = `{"status":false,"message":"synthetic-password synthetic-token"}`
			}
			s := coreGDNewScript(t, steps, nil)
			switch test.kind {
			case "unknown":
				s.bindings.NewClient = func(gendata.Prompt, func(string)) (*gendata.VPSAdmin, error) {
					t.Fatal("unknown leaf reached factory")
					return nil, nil
				}
			case "missing-client":
				s.bindings.NewClient = nil
			case "factory-error":
				s.bindings.NewClient = func(gendata.Prompt, func(string)) (*gendata.VPSAdmin, error) { return nil, sentinel }
			case "nil-client":
				s.bindings.NewClient = func(gendata.Prompt, func(string)) (*gendata.VPSAdmin, error) { return nil, nil }
			case "nil-prompt":
				s.bindings.Prompt = nil
			case "username", "password":
				old := s.bindings.Prompt
				s.bindings.Prompt = func(ctx context.Context, label string, p bool) (string, error) {
					if (test.kind == "username" && label == "User name: ") || (test.kind == "password" && label == "Password: ") {
						s.trace = append(s.trace, "prompt refusal")
						return "synthetic-answer", errors.New("synthetic-password")
					}
					return old(ctx, label, p)
				}
			case "missing-inventory":
				s.bindings.Machines = nil
			case "inventory-error":
				s.bindings.Machines = func(context.Context) ([]Machine, error) { return nil, sentinel }
			}
			err := runGenData(context.Background(), root, test.leaf, s.bindings)
			if err == nil || err.Error() != test.want {
				t.Fatal("construction/auth reached error differs")
			}
			if test.kind == "factory-error" || test.kind == "inventory-error" {
				if err != sentinel {
					t.Fatal("binding error identity lost")
				}
			}
			if strings.HasPrefix(test.want, "authentication failed:") {
				var a *gendata.AuthenticationError
				if !errors.As(err, &a) {
					t.Fatal("auth boundary lost")
				}
			}
			if s.at != len(steps) {
				t.Fatal("construction/auth request count changed")
			}
			coreGDCheckTree(t, root, before)
		})
	}
}

func TestGenDataCoreLeafOrderAndInventory(t *testing.T) {
	for _, leaf := range []genDataLeaf{genDataContainers, genDataNetwork, genDataAll} {
		for _, populated := range []bool{false, true} {
			t.Run(string(rune('0'+leaf))+map[bool]string{false: "empty", true: "populated"}[populated], func(t *testing.T) {
				root := coreGDSeed(t)
				want := coreGDTree(t, root)
				steps := coreGDLoginSteps()
				machines := []Machine{}
				containerBody := coreGDEmptyContainers
				networkBody := coreGDEmptyNetworks
				if populated && leaf != genDataNetwork {
					carrier := "carrier"
					first := coreGDMachine(json.Number("9007199254740993"))
					first.Managed = false
					first.CarrierName = &carrier
					duplicate := coreGDMachine(json.Number("9007199254740993"))
					empty := coreGDMachine(nil)
					empty.Attributes["container"] = map[string]any{}
					machines = []Machine{{Name: "missing"}, {Name: "null", Attributes: map[string]any{"container": nil}}, first, empty, duplicate}
					steps = append(steps, coreGDShowStep("9007199254740993", strings.Replace(coreGDNode, `"name":"node"`, `"name":"first"`, 1)), coreGDShowStep("", coreGDNode), coreGDShowStep("9007199254740993", coreGDNode))
					containerBody = coreGDContainers
				}
				if leaf != genDataContainers {
					if !populated || leaf == genDataNetwork {
					}
					rows := `[]`
					if populated {
						rows = `[{"ip_version":6.0,"address":"2001:db8:2::","prefix":64},{"ip_version":"4","address":{},"prefix":[]},{"ip_version":true,"address":[]},{"ip_version":4e0,"address":"192.0.2.0","prefix":9007199254740993},{"ip_version":6,"address":null,"prefix":null},{"ip_version":4,"address":"192.0.2.0","prefix":24}]`
						networkBody = "{\n  \"ipv4\" = [\n    {\n      address = \"192.0.2.0\";\n      prefix = 9007199254740993;\n    }\n    {\n      address = \"192.0.2.0\";\n      prefix = 24;\n    }\n  ];\n  \"ipv6\" = [\n    {\n      address = \"2001:db8:2::\";\n      prefix = 64;\n    }\n    {\n      address = null;\n      prefix = null;\n    }\n  ];\n}\n"
					}
					list := coreGDListStep(rows)
					if leaf == genDataAll {
						list.before = func(*http.Request) {
							x := coreGDTree(t, root)
							if x[coreGDContainerPath].body != containerBody {
								t.Fatal("network discovery before containers save")
							}
							if _, ok := x[coreGDContainerPath+".new"]; ok {
								t.Fatal("container sibling not renamed")
							}
						}
					}
					steps = append(steps, list)
				}
				s := coreGDNewScript(t, steps, machines)
				beforeMachines, _ := json.Marshal(machines)
				e := &Engine{Inventory: machines, SettingsCache: map[string]any{"sentinel": "unchanged"}}
				s.bindings.Machines = func(ctx context.Context) ([]Machine, error) {
					s.trace = append(s.trace, "inventory")
					return e.Machines(false)
				}
				if leaf == genDataNetwork {
					s.bindings.Machines = func(context.Context) ([]Machine, error) {
						t.Fatal("network reached inventory")
						return nil, errors.New("unreached")
					}
				}
				renders := 0
				s.bindings.Literal = func(value any) (string, error) {
					renders++
					rel := coreGDContainerPath
					if leaf == genDataNetwork || renders == 2 {
						rel = coreGDNetworkPath
					}
					if b, err := os.ReadFile(filepath.Join(root, rel+".new")); err != nil || len(b) != 0 {
						t.Fatal("render before open/truncate")
					}
					s.trace = append(s.trace, "render")
					return genDataNixLiteral(value)
				}
				if err := runGenData(context.Background(), root, leaf, s.bindings); err != nil {
					t.Fatal("leaf composition failed")
				}
				s.done()
				if !reflect.DeepEqual(s.trace[:len(coreGDAuthTrace())], coreGDAuthTrace()) {
					t.Fatal("auth sequence changed")
				}
				if leaf != genDataNetwork {
					if s.trace[len(coreGDAuthTrace())] != "inventory" {
						t.Fatal("inventory was not after auth")
					}
					coreGDReplace(want, coreGDContainerPath, containerBody)
				}
				if leaf != genDataContainers {
					coreGDReplace(want, coreGDNetworkPath, networkBody)
				}
				afterMachines, _ := json.Marshal(machines)
				if string(beforeMachines) != string(afterMachines) || e.SettingsCache["sentinel"] != "unchanged" {
					t.Fatal("borrowed snapshot/settings mutated")
				}
				coreGDCheckTree(t, root, want)
			})
		}
	}
}

func TestGenDataCoreLazyAccess(t *testing.T) {
	for _, test := range []struct {
		name, node string
		machine    func() Machine
		afterFirst bool
		want       string
	}{
		{"container-type", coreGDNode, func() Machine { m := coreGDMachine(1); m.Attributes["container"] = false; return m }, false, "container: expected structured object"},
		{"host-after-show", `{"_meta":{"resolved":false,"path_params":[1]}}`, func() Machine { m := coreGDMachine(json.Number("1")); m.Attributes["host"] = false; return m }, false, "host: expected structured object"},
		{"missing-node", `null`, func() Machine { return coreGDMachine(json.Number("1")) }, false, "node: missing association"},
		{"missing-location", `{"_meta":{"resolved":true},"id":null,"name":null,"location":null}`, func() Machine { return coreGDMachine(json.Number("1")) }, false, "node.location: missing association"},
		{"missing-environment", `{"_meta":{"resolved":true},"location":{"_meta":{"resolved":true},"domain":null,"environment":null}}`, func() Machine { return coreGDMachine(json.Number("1")) }, false, "node.location.environment: missing association"},
		{"second-row", `null`, func() Machine { return coreGDMachine(json.Number("1")) }, true, "node: missing association"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := coreGDSeed(t)
			before := coreGDTree(t, root)
			steps := coreGDLoginSteps()
			machines := []Machine{}
			if test.afterFirst {
				machines = append(machines, coreGDMachine(json.Number("1")))
				steps = append(steps, coreGDShowStep("1", coreGDNode))
			}
			machines = append(machines, test.machine())
			if test.name != "container-type" {
				if !test.afterFirst {
				}
				steps = append(steps, coreGDShowStep("1", test.node))
			}
			s := coreGDNewScript(t, steps, machines)
			s.bindings.Literal = func(any) (string, error) { t.Fatal("failed projection reached render"); return "", nil }
			err := runGenData(context.Background(), root, genDataAll, s.bindings)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatal("lazy reached failure changed")
			}
			s.done()
			coreGDCheckTree(t, root, before)
		})
	}
	// Resolved nullable/raw scalars remain nullable, without eager typed narrowing.
	root := coreGDSeed(t)
	want := coreGDTree(t, root)
	node := `{"_meta":{"resolved":true},"id":true,"name":null,"domain_name":null,"location":{"_meta":{"resolved":true},"domain":false,"environment":{"_meta":{"resolved":true},"domain":null}}}`
	steps := append(coreGDLoginSteps(), coreGDShowStep("1", node))
	s := coreGDNewScript(t, steps, []Machine{coreGDMachine(json.Number("1"))})
	if err := runGenData(context.Background(), root, genDataContainers, s.bindings); err != nil {
		t.Fatal("raw nullable scalar projection failed")
	}
	s.done()
	body := "{\n  \"z.loc.test\" = {\n    node = {\n      domain = null;\n      fqdn = \".\";\n      id = true;\n      location = false;\n      name = null;\n    };\n  };\n}\n"
	coreGDReplace(want, coreGDContainerPath, body)
	coreGDCheckTree(t, root, want)
}

func TestGenDataCoreFilesAndAllPartialEffects(t *testing.T) {
	for _, phase := range []string{"containers-projection", "containers-open", "containers-render", "containers-rename", "network-empty-list", "network-list", "network-projection", "network-open", "network-render", "network-rename", "alias-render", "nul-render", "invalid-utf8-render"} {
		t.Run(phase, func(t *testing.T) {
			root := coreGDSeed(t)
			machine := coreGDMachine(json.Number("1"))
			steps := coreGDLoginSteps()
			wantCause := errors.New("synthetic renderer failure")
			switch phase {
			case "containers-open":
				if err := os.Remove(filepath.Join(root, coreGDContainerPath+".new")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(root, coreGDContainerPath+".new"), 0755); err != nil {
					t.Fatal(err)
				}
			case "network-open":
				if err := os.RemoveAll(filepath.Join(root, "data/vpsadmin/networks")); err != nil {
					t.Fatal(err)
				}
			case "containers-rename", "network-rename":
				rel := coreGDContainerPath
				if phase == "network-rename" {
					rel = coreGDNetworkPath
				}
				if err := os.Remove(filepath.Join(root, rel)); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(root, rel), 0755); err != nil {
					t.Fatal(err)
				}
				coreGDWrite(t, filepath.Join(root, rel, "occupied"), "unrelated", 0600)
			case "alias-render":
				if err := os.Remove(filepath.Join(root, coreGDContainerPath+".new")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("containers.nix", filepath.Join(root, coreGDContainerPath+".new")); err != nil {
					t.Fatal(err)
				}
			case "nul-render":
				machine.Attributes["host"].(map[string]any)["name"] = "bad\x00key"
			case "invalid-utf8-render":
				machine.Attributes["host"].(map[string]any)["name"] = "bad" + string([]byte{0xff})
			}
			want := coreGDTree(t, root)
			node := coreGDNode
			if phase == "containers-projection" {
				node = `null`
			}
			steps = append(steps, coreGDShowStep("1", node))
			containerStops := strings.HasPrefix(phase, "containers-") || phase == "alias-render" || phase == "nul-render" || phase == "invalid-utf8-render"
			if !containerStops {
				rows := `[]`
				if phase == "network-projection" {
					rows = `[{"ip_version":6,"address":{}},{"ip_version":4,"prefix":[]}]`
				}
				list := coreGDListStep(rows)
				list.before = func(*http.Request) {
					if b, err := os.ReadFile(filepath.Join(root, coreGDContainerPath)); err != nil || string(b) != coreGDContainers {
						t.Fatal("network before completed first replacement")
					}
				}
				if phase == "network-list" {
					list.status = 403
				}
				if phase == "network-empty-list" {
					// Empty inventory is the source path that defers resource discovery until network.
					machine = Machine{Name: "skipped"}
					steps = coreGDLoginSteps()
					discovery := coreGDListStep(`[]`)
					discovery.status = 403
					discovery.before = func(*http.Request) {
						if b, err := os.ReadFile(filepath.Join(root, coreGDContainerPath)); err != nil || string(b) != coreGDEmptyContainers {
							t.Fatal("discovery before first save")
						}
					}
					steps = append(steps, discovery)
				} else {
					steps = append(steps, list)
				}
			}
			s := coreGDNewScript(t, steps, []Machine{machine})
			renders := 0
			s.bindings.Literal = func(value any) (string, error) {
				renders++
				rel := coreGDContainerPath
				if renders == 2 {
					rel = coreGDNetworkPath
				}
				if b, err := os.ReadFile(filepath.Join(root, rel+".new")); err != nil || len(b) != 0 {
					t.Fatal("renderer did not see opened/truncated sibling")
				}
				if phase == "containers-render" || phase == "alias-render" || phase == "network-render" && renders == 2 {
					return "", wantCause
				}
				return genDataNixLiteral(value)
			}
			err := runGenData(context.Background(), root, genDataAll, s.bindings)
			if err == nil {
				t.Fatal("file/API failure became success")
			}
			s.done()
			switch phase {
			case "containers-projection":
				if renders != 0 {
					t.Fatal("projection error rendered")
				}
			case "containers-open":
				if renders != 0 {
					t.Fatal("open error rendered")
				}
			case "containers-render", "nul-render", "invalid-utf8-render":
				x := want[coreGDContainerPath+".new"]
				x.body = ""
				want[coreGDContainerPath+".new"] = x
			case "alias-render":
				x := want[coreGDContainerPath]
				x.body = ""
				want[coreGDContainerPath] = x
			case "containers-rename":
				x := want[coreGDContainerPath+".new"]
				x.body = coreGDContainers
				want[coreGDContainerPath+".new"] = x
			default:
				body := coreGDContainers
				if phase == "network-empty-list" {
					body = coreGDEmptyContainers
				}
				coreGDReplace(want, coreGDContainerPath, body)
				if phase == "network-render" {
					x := want[coreGDNetworkPath+".new"]
					x.body = ""
					want[coreGDNetworkPath+".new"] = x
				}
				if phase == "network-rename" {
					x := want[coreGDNetworkPath+".new"]
					x.body = coreGDEmptyNetworks
					want[coreGDNetworkPath+".new"] = x
				}
			}
			if phase == "containers-render" || phase == "alias-render" || phase == "network-render" {
				if !errors.Is(err, wantCause) {
					t.Fatal("render sentinel identity lost")
				}
			}
			if phase == "network-projection" && !strings.Contains(err.Error(), "network 1:") {
				t.Fatal("network family4 error did not precede family6")
			}
			if phase == "nul-render" && !strings.Contains(err.Error(), "NUL") {
				t.Fatal("NUL formatter boundary not reached")
			}
			if phase == "invalid-utf8-render" && !strings.Contains(err.Error(), "UTF-8") {
				t.Fatal("UTF8 formatter boundary not reached")
			}
			coreGDCheckTree(t, root, want)
		})
	}
	// A genuinely absent parent stays absent, and open failure precedes rendering.
	root := t.TempDir()
	coreGDWrite(t, filepath.Join(root, "sentinel"), "untouched", 0640)
	before := coreGDTree(t, root)
	s := coreGDNewScript(t, coreGDLoginSteps(), nil)
	s.bindings.Literal = func(any) (string, error) { t.Fatal("missing parent rendered"); return "", nil }
	if err := runGenData(context.Background(), root, genDataContainers, s.bindings); err == nil || !strings.Contains(err.Error(), ": open:") {
		t.Fatal("missing parent accepted")
	}
	s.done()
	coreGDCheckTree(t, root, before)
}

func TestGenDataCoreCancellationAndErrors(t *testing.T) {
	for _, phase := range []string{"prompt-error", "prompt-success", "do-error", "inventory-error", "inventory-success", "render-success", "all-after-render"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			root := coreGDSeed(t)
			want := coreGDTree(t, root)
			steps := coreGDLoginSteps()
			leaf := genDataContainers
			if phase == "prompt-error" || phase == "prompt-success" {
				steps = nil
				cancel()
			}
			if phase == "do-error" {
				steps = steps[:1]
				steps[0].before = func(*http.Request) { cancel() }
				steps[0].failure = errors.New("synthetic transport secret")
			}
			s := coreGDNewScript(t, steps, nil)
			prompt := s.bindings.Prompt
			s.bindings.Prompt = func(c context.Context, label string, p bool) (string, error) {
				if c != ctx {
					t.Fatal("prompt context not borrowed")
				}
				if phase == "prompt-error" {
					s.trace = append(s.trace, "prompt canceled")
					return "synthetic-answer", errors.New("synthetic prompt secret")
				}
				return prompt(c, label, p)
			}
			s.bindings.Machines = func(c context.Context) ([]Machine, error) {
				if c != ctx {
					t.Fatal("inventory context changed")
				}
				s.trace = append(s.trace, "inventory")
				if phase == "inventory-error" {
					cancel()
					return nil, context.Canceled
				}
				if phase == "inventory-success" {
					cancel()
				}
				return nil, nil
			}
			if phase == "render-success" || phase == "all-after-render" {
				s.bindings.Literal = func(value any) (string, error) { cancel(); return genDataNixLiteral(value) }
			}
			if phase == "all-after-render" {
				leaf = genDataAll
			}
			err := runGenData(ctx, root, leaf, s.bindings)
			if phase == "inventory-success" || phase == "render-success" {
				if err != nil {
					t.Fatal("added eager cancellation gate")
				}
				coreGDReplace(want, coreGDContainerPath, coreGDEmptyContainers)
			} else {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("reached cancellation classification changed")
				}
				if phase == "all-after-render" {
					coreGDReplace(want, coreGDContainerPath, coreGDEmptyContainers)
				}
			}
			if s.at != len(steps) || s.factories != 1 {
				t.Fatal("cancellation dispatch count changed")
			}
			if phase == "prompt-error" && len(s.trace) != 2 {
				t.Fatal("canceled prompt error continued")
			}
			if phase == "prompt-success" && !reflect.DeepEqual(s.trace, []string{"factory", "prompt User name: ", "prompt Password: "}) {
				t.Fatal("successful canceled prompts were preempted")
			}
			coreGDCheckTree(t, root, want)
		})
	}
}

func TestGenDataCoreAuthExpiryAndKnownChallenges(t *testing.T) {
	for _, expiry := range []string{"absent", "equal", "past"} {
		t.Run(expiry, func(t *testing.T) {
			root := coreGDSeed(t)
			want := coreGDTree(t, root)
			steps := coreGDLoginSteps()
			if expiry != "absent" {
				date := "2026-10-14T12:00:00Z"
				if expiry == "past" {
					date = "2026-10-14T11:59:59Z"
				}
				steps[0].body = strings.Replace(coreGDLogin, `"synthetic-token"`, `"synthetic-token","valid_to":"`+date+`"`, 1)
			}
			renewal := coreGDStep{method: "POST", uri: "/_auth/token/tokens", body: steps[0].body}
			if expiry == "past" {
				steps = append(steps, renewal)
			}
			list := coreGDListStep(`[]`)
			list.status = 403
			steps = append(steps, list)
			s := coreGDNewScript(t, steps, nil)
			s.bindings.Machines = nil
			err := runGenData(context.Background(), root, genDataNetwork, s.bindings)
			if err == nil || !strings.Contains(err.Error(), "access forbidden") {
				t.Fatal("resource refusal changed")
			}
			s.done()
			coreGDCheckTree(t, root, want)
			prompts, clocks := 0, 0
			for _, event := range s.trace {
				if strings.HasPrefix(event, "prompt ") {
					prompts++
				}
				if event == "clock" {
					clocks++
				}
			}
			if prompts != 2 || clocks != map[bool]int{false: 1, true: 0}[expiry == "absent"] {
				t.Fatal("expiry reprompt/clock schedule changed")
			}
		})
	}
	// Repeated known reset steps preserve protected prompts and effect ordering.
	root := coreGDSeed(t)
	want := coreGDTree(t, root)
	steps := coreGDLoginSteps()
	steps[0].body = `{"status":true,"response":{"token":{"complete":false,"token":"synthetic-step","next_action":"reset_password"}}}`
	steps = append(steps, coreGDStep{method: "POST", uri: "/_auth/token/tokens/reset_password", body: steps[0].body}, coreGDStep{method: "POST", uri: "/_auth/token/tokens/reset_password", body: coreGDLogin})
	script := coreGDNewScript(t, steps, nil)
	if err := runGenData(context.Background(), root, genDataContainers, script.bindings); err != nil {
		t.Fatal(err)
	}
	script.done()
	coreGDReplace(want, coreGDContainerPath, coreGDEmptyContainers)
	coreGDCheckTree(t, root, want)
	expected := append(coreGDAuthTrace(), "prompt New password: ", "prompt Repeat new password: ", "POST /_auth/token/tokens/reset_password", "close", "prompt New password: ", "prompt Repeat new password: ", "POST /_auth/token/tokens/reset_password", "close", "inventory")
	if !reflect.DeepEqual(script.trace, expected) {
		t.Fatal("known challenge order", script.trace)
	}
}

func TestGenDataCorePublicBoundary(t *testing.T) {
	r := cli.BuiltinRegistry()
	for _, leaf := range []string{"containers", "network", "all"} {
		path := []string{"gen-data", "vpsadmin", leaf}
		inv, err := r.Parse(path)
		if err != nil || r.CheckAvailability(inv) != nil || inv.Command.Handler != cli.GenDataHandler {
			t.Fatal("public gen-data dispatch is unavailable")
		}
		command, ok := r.Lookup(path)
		if !ok || command.Availability.Mode != cli.Available {
			t.Fatal("public declaration changed")
		}
	}
}

func TestGenDataCoreCreationAndUnresolvedAssociations(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "data/vpsadmin/networks"), 0755); err != nil {
		t.Fatal(err)
	}
	coreGDWrite(t, filepath.Join(root, "sentinel"), "unrelated", 0600)
	control := filepath.Join(root, "umask-control")
	f, err := os.OpenFile(control, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(control)
	if err != nil {
		t.Fatal(err)
	}
	want := coreGDTree(t, root)
	steps := append(coreGDLoginSteps(), coreGDShowStep("1", `{"_meta":{"resolved":false,"path_params":["a b"]}}`))
	node := `{"id":9007199254740993,"name":"node","domain_name":"n","location":{"_meta":{"resolved":false,"path_params":[2]}}}`
	location := `{"domain":"loc","environment":{"_meta":{"resolved":false,"path_params":[3]}}}`
	steps = append(steps,
		coreGDStep{method: "GET", uri: "/v7.0/nodes/a%20b", token: "synthetic-token", body: `{"status":true,"response":{"node":` + node + `}}`},
		coreGDStep{method: "GET", uri: "/v7.0/locations/2", token: "synthetic-token", body: `{"status":true,"response":{"location":` + location + `}}`},
		coreGDStep{method: "GET", uri: "/v7.0/environments/3", token: "synthetic-token", body: `{"status":true,"response":{"environment":{"domain":"test"}}}`},
		coreGDListStep(`[]`))
	s := coreGDNewScript(t, steps, []Machine{coreGDMachine(json.Number("1"))})
	if err := runGenData(context.Background(), root, genDataAll, s.bindings); err != nil {
		t.Fatal("unresolved association composition failed")
	}
	s.done()
	want[coreGDContainerPath] = coreGDEntry{kind: "file", mode: info.Mode(), body: coreGDContainers}
	want[coreGDNetworkPath] = coreGDEntry{kind: "file", mode: info.Mode(), body: coreGDEmptyNetworks}
	coreGDCheckTree(t, root, want)
}

func TestGenDataCoreAllLaterAuthFailure(t *testing.T) {
	root := coreGDSeed(t)
	want := coreGDTree(t, root)
	steps := coreGDLoginSteps()
	steps[0].body = strings.Replace(coreGDLogin, `"synthetic-token"`, `"synthetic-token","valid_to":"2026-10-14T12:00:00Z"`, 1)
	steps = append(steps, coreGDShowStep("1", coreGDNode), coreGDStep{method: "POST", uri: "/_auth/token/tokens", body: `{"status":false,"message":"synthetic-password synthetic-token"}`})
	s := coreGDNewScript(t, steps, []Machine{coreGDMachine(json.Number("1"))})
	s.bindings.Literal = func(value any) (string, error) { s.now = s.now.Add(time.Second); return genDataNixLiteral(value) }
	err := runGenData(context.Background(), root, genDataAll, s.bindings)
	var authErr *gendata.AuthenticationError
	if !errors.As(err, &authErr) || strings.Contains(err.Error(), "synthetic-") {
		t.Fatal("later auth error classification/redaction changed")
	}
	s.done()
	coreGDReplace(want, coreGDContainerPath, coreGDContainers)
	coreGDCheckTree(t, root, want)
}
