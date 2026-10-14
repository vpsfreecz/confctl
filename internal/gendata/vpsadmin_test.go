package gendata

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// All provider responses/expectations are independent source-derived fixtures,
// never a production capture or output learned from the implementation.
const providerDescription = `{
 "status":true,"version":"2.0","response":{
 "authentication":{"token":{"http_header":"X-Fixture-Token","resources":{"token":{"actions":{
 "request":{"method":"POST","path":"/auth/request","input":{"namespace":"credentials","parameters":{
 "user":{"type":"String","validators":{"present":{}}},"password":{"type":"String","validators":{"present":{}}},
 "lifetime":{"type":"String","validators":null},"interval":{"type":"Integer","validators":{}}}},
 "output":{"namespace":"login","layout":"hash","parameters":{}}},
 "device":{"method":"PUT","path":"/auth/device","input":{"namespace":"challenge","parameters":{
 "token":{"type":"String","validators":{}},"answer":{"type":"Integer","label":"Answer","protected":true,"validators":{"present":{}}},
 "approved":{"type":"Boolean","validators":{}}}},"output":{"namespace":"login","layout":"hash","parameters":{}}}
 }}}}},"resources":{
 "vps":{"actions":{"show":{"method":"GET","path":"/alternate/vps/{id}","input":{"namespace":"vps","parameters":{}},"output":{"namespace":"vps","layout":"object","parameters":{"node":{"type":"Resource","resource":["node"]}}}}}},
 "node":{"actions":{"show":{"method":"GET","path":"/alternate/node/{id}","output":{"namespace":"node","layout":"object","parameters":{
 "id":{"type":"Integer"},"name":{"type":"String"},"domain_name":{"type":"String"},"location":{"type":"Resource","resource":["location"]}}}}}},
 "location":{"actions":{"show":{"method":"GET","path":"/alternate/location/{id}","output":{"namespace":"location","layout":"object","parameters":{
 "domain":{"type":"String"},"environment":{"type":"Resource","resource":["environment"]}}}}}},
 "environment":{"actions":{"show":{"method":"GET","path":"/alternate/environment/{id}","output":{"namespace":"environment","layout":"object","parameters":{"domain":{"type":"String"}}}}}},
 "network":{"actions":{"index":{"aliases":["list"],"method":"GET","path":"/alternate/networks","output":{"namespace":"networks","layout":"object_list","parameters":{
 "ip_version":{"type":"Integer"},"address":{"type":"String"},"prefix":{"type":"Integer"}}}}}}
 }}}`

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func fixtureProvider(doer httpDoer, prompt Prompt) *VPSAdmin {
	origin, _ := url.Parse("https://fixture.invalid")
	return &VPSAdmin{origin: origin, doer: doer, now: func() time.Time { return time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC) }, prompt: prompt, budget: requestBudget}
}

func fixtureResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

func testAuthenticated(t *testing.T, reply func(*http.Request) (*http.Response, error)) (*VPSAdmin, *[]string) {
	t.Helper()
	trace := []string{}
	p := fixtureProvider(doerFunc(func(r *http.Request) (*http.Response, error) {
		trace = append(trace, r.Method+" "+r.URL.RequestURI())
		if r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") != "haveapi-client" {
			t.Fatal("source request identity changed")
		}
		if r.Method == "OPTIONS" {
			if r.Body != nil {
				t.Fatal("description has a body")
			}
			want := ""
			if len(trace) > 3 {
				want = "fixture-token"
			}
			if r.Header.Get("X-Fixture-Token") != want {
				t.Fatal("description authentication timing changed", trace)
			}
			return fixtureResponse(200, providerDescription), nil
		}
		if r.URL.Path == "/auth/request" {
			if r.Header.Get("X-Fixture-Token") != "" {
				t.Fatal("auth step attached installed token")
			}
			body, _ := io.ReadAll(r.Body)
			value, err := jsonValue(body)
			want := map[string]any{"credentials": map[string]any{"user": "fixture-user", "password": "fixture-password", "lifetime": "fixed", "interval": json.Number("60")}}
			if err != nil || !reflect.DeepEqual(value, want) {
				t.Fatal("fixed credential input differs")
			}
			return fixtureResponse(200, `{"status":true,"response":{"login":{"complete":true,"token":"fixture-token"}}}`), nil
		}
		if r.Header.Get("X-Fixture-Token") != "fixture-token" {
			t.Fatal("resource token missing")
		}
		return reply(r)
	}), func(_ context.Context, text string, protected bool) (string, error) {
		trace = append(trace, "prompt "+text)
		if text == "User name: " && !protected {
			return " fixture-user \t", nil
		}
		if text == "Password: " && protected {
			return "fixture-password", nil
		}
		t.Fatal("unexpected prompt")
		return "", errors.New("unexpected fixture prompt")
	})
	if err := p.Authenticate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return p, &trace
}

func TestVPSAdminLazyAssociationsAndOrdering(t *testing.T) {
	ctx := context.Background()
	p, trace := testAuthenticated(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/alternate/vps/9007199254740993":
			if r.URL.Query().Get("_meta[includes]") != "node__location__environment" || len(r.URL.Query()) != 1 {
				t.Fatal("includes/filter changed")
			}
			return fixtureResponse(200, `{"status":true,"response":{"vps":{"node":{"_meta":{"resolved":false,"path_params":["node id/+?"]}}}}}`), nil
		case "/alternate/node/node id/+?":
			if r.URL.EscapedPath() != "/alternate/node/node%20id%2F%2B%3F" {
				t.Fatal("CGI path encoding changed", r.URL.EscapedPath())
			}
			return fixtureResponse(200, `{"status":true,"response":{"node":{"id":9007199254740993,"name":null,"domain_name":"n","location":{"_meta":{"resolved":false,"path_params":[2]}}}}}`), nil
		case "/alternate/location/2":
			return fixtureResponse(200, `{"status":true,"response":{"location":{"domain":"loc","environment":{"domain":"env","_meta":{"resolved":true}}}}}`), nil
		default:
			t.Fatal("unexpected resource request", r.URL.Path)
		}
		return nil, errors.New("fixture unexpected request")
	})
	m := MachineInput{Container: some(ContainerRef{ID: some(json.Number("9007199254740993"))}), HostName: some("ct"), HostDomain: some("example")}
	vps, err := p.ShowVPS(ctx, m.Container.Value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(*trace) != 6 {
		t.Fatal("association resolved before projection", *trace)
	}
	key, row, err := ProjectContainer(m, vps)
	want := map[string]any{"node": map[string]any{"id": json.Number("9007199254740993"), "name": nil, "location": "loc", "domain": "env", "fqdn": "n.env"}}
	if err != nil || key != "ct.example" || !reflect.DeepEqual(row, want) {
		t.Fatal(key, row, err)
	}
	if _, _, err := ProjectContainer(m, vps); err != nil {
		t.Fatal(err)
	}
	if len(*trace) != 8 {
		t.Fatal("association instance resolved twice", *trace)
	}
	if _, err := Containers(ctx, p, []MachineInput{m, m}); err != nil {
		t.Fatal(err)
	}
	if len(*trace) != 14 {
		t.Fatal("repeated VPS IDs were cached", *trace)
	}
	broken := m
	broken.HostName = Field[string]{Value: m.HostName.Value}
	vps, err = p.ShowVPS(ctx, m.Container.Value.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := len(*trace)
	if _, _, err = ProjectContainer(broken, vps); err == nil || len(*trace) != before {
		t.Fatal("host failure did not precede association", err, *trace)
	}
}

func TestVPSAdminOneNetworkResponseLazyUnusedFields(t *testing.T) {
	p, trace := testAuthenticated(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/alternate/networks" || r.URL.RawQuery != "" {
			t.Fatal("network route/filters changed")
		}
		return fixtureResponse(200, `{"status":true,"response":{"networks":[
		{"ip_version":6.0,"address":"six-a","prefix":64},
		{"ip_version":"4","address":{},"prefix":"bad-unused"},
		{"ip_version":4e0,"address":"four","prefix":9007199254740993},
		{"ip_version":true,"address":{},"prefix":"unused"},
		{"ip_version":6,"address":"six-b","prefix":null},
		{"address":{},"prefix":[]},
		{"ip_version":4,"address":"four","prefix":0}]}}`), nil
	})
	got, err := Networks(context.Background(), p)
	want := map[string]any{"ipv4": []any{map[string]any{"address": "four", "prefix": json.Number("9007199254740993")}, map[string]any{"address": "four", "prefix": json.Number("0")}}, "ipv6": []any{map[string]any{"address": "six-a", "prefix": json.Number("64")}, map[string]any{"address": "six-b", "prefix": nil}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal(got, err)
	}
	if len(*trace) != 6 {
		t.Fatal("network pagination/additional request", *trace)
	}
}

func TestVPSAdminNullableIDsDuplicateRowsAndAllPartialSuccess(t *testing.T) {
	ctx := context.Background()
	shows := 0
	p, trace := testAuthenticated(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/alternate/vps/" {
			shows++
			name := "first"
			if shows > 1 {
				name = "last"
			}
			return fixtureResponse(200, `{"status":true,"response":{"vps":{"node":{"id":1,"name":"`+name+`","domain_name":"node","location":{"domain":"loc","environment":{"domain":"env","_meta":{"resolved":true}},"_meta":{"resolved":true}},"_meta":{"resolved":true}}}}}`), nil
		}
		if r.URL.Path == "/alternate/networks" {
			return fixtureResponse(500, "synthetic-secret"), nil
		}
		t.Fatal("unexpected nullable ID route")
		return nil, errors.New("unexpected fixture route")
	})
	machines := []MachineInput{
		{Container: some(ContainerRef{}), HostName: some("same")},
		{Container: some(ContainerRef{ID: null[json.Number]()}), HostName: some("same")},
	}
	projection, err := Containers(ctx, p, machines)
	if err != nil || shows != 2 || projection["same"].(map[string]any)["node"].(map[string]any)["name"] != "last" {
		t.Fatal("nullable IDs/duplicate request replacement changed", err)
	}
	root := t.TempDir()
	container := filepath.Join(root, "data/vpsadmin/containers.nix")
	network := filepath.Join(root, "data/vpsadmin/networks/containers.nix")
	if err := os.MkdirAll(filepath.Dir(network), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(network, []byte("OLD-N\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(network+".new", []byte("STALE-N\n"), 0600); err != nil {
		t.Fatal(err)
	}
	g := Generator{Root: root, Source: p, Literal: func(value any) (string, error) {
		if !reflect.DeepEqual(value, map[string]any{}) {
			t.Fatal("empty container projection changed")
		}
		return "{\n}", nil
	}}
	before := len(*trace)
	if err := g.All(ctx, nil); err == nil || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatal("network error missing/reflected")
	}
	if len(*trace) != before+1 {
		t.Fatal("empty containers caused request/network preflight")
	}
	if body, err := os.ReadFile(container); err != nil || string(body) != "{\n}\n" {
		t.Fatal("first file rolled back", err)
	}
	if body, err := os.ReadFile(network); err != nil || string(body) != "OLD-N\n" {
		t.Fatal("network destination modified", err)
	}
	if body, err := os.ReadFile(network + ".new"); err != nil || string(body) != "STALE-N\n" {
		t.Fatal("network sibling opened before API success", err)
	}
}

func TestVPSAdminDecodedScalarDomainsAndMachineAccessOrder(t *testing.T) {
	requests := 0
	p, _ := testAuthenticated(t, func(r *http.Request) (*http.Response, error) {
		requests++
		if strings.Contains(r.URL.Path, "networks") {
			return fixtureResponse(200, `{"status":true,"response":{"networks":[{"ip_version":4,"address":false,"prefix":"32"}]}}`), nil
		}
		return fixtureResponse(200, `{"status":true,"response":{"vps":{"node":{"id":"source-id","name":false,"domain_name":7,"location":{"domain":null,"environment":{"domain":false,"_meta":{"resolved":true,"path_params":"unused"}},"_meta":{"resolved":true}},"_meta":{"resolved":true}}}}}`), nil
	})
	ctx := context.Background()
	machine := MachineInputFromAttributes("logical", map[string]any{
		"container": map[string]any{"id": json.Number("9007199254740993")},
		"host":      map[string]any{"name": false, "location": "", "domain": json.Number("0")},
	})
	got, err := Containers(ctx, p, []MachineInput{machine})
	want := map[string]any{"false..0": map[string]any{"node": map[string]any{"id": "source-id", "name": false, "location": nil, "domain": false, "fqdn": "7.false"}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("source primitive domains were coerced/narrowed", err)
	}
	networks, err := Networks(ctx, p)
	if err != nil || !reflect.DeepEqual(networks, map[string]any{"ipv4": []any{map[string]any{"address": false, "prefix": "32"}}, "ipv6": []any{}}) {
		t.Fatal("network primitive scalars were coerced/narrowed", err)
	}
	badContainer := MachineInputFromAttributes("bad", map[string]any{"container": true})
	before := requests
	if _, err := Containers(ctx, p, []MachineInput{badContainer}); err == nil || requests != before {
		t.Fatal("malformed container ID traversal dispatched HTTP")
	}
	badHost := MachineInputFromAttributes("bad", map[string]any{"container": map[string]any{}, "host": true})
	if _, err := Containers(ctx, p, []MachineInput{badHost}); err == nil || requests != before+1 {
		t.Fatal("host metadata failure was eager or association-dependent")
	}
}

func TestVPSAdminProductionTransportAndOriginRefusal(t *testing.T) {
	p := NewVPSAdmin(nil, nil)
	if p.origin.String() != productionAPI || p.resources != nil || p.authDescription != nil {
		t.Fatal("constructor performs discovery or changes production origin")
	}
	client := p.doer.(*http.Client)
	transport := client.Transport.(*http.Transport)
	if !transport.DisableKeepAlives || transport.ForceAttemptHTTP2 || transport.TLSClientConfig != nil || transport.TLSNextProto == nil || p.budget != 60*time.Second {
		t.Fatal("production transport policy changed")
	}
	for _, path := range []string{"https://other.invalid/", "http://api.vpsfree.cz/", "//other.invalid/", "https://user:password@api.vpsfree.cz/", "https://api.vpsfree.cz:444/"} {
		if _, err := p.destination(path); err == nil {
			t.Fatal("accepted foreign/credential destination")
		}
	}
	if _, err := p.destination("https://api.vpsfree.cz:443/alternate"); err != nil {
		t.Fatal("effective same origin rejected")
	}
	r, _ := http.NewRequest("GET", productionAPI, nil)
	for _, scheme := range []string{"http", "https", "socks5"} {
		c := trustedHTTPClient(func(*http.Request) (*url.URL, error) { return url.Parse(scheme + "://proxy.invalid:8080") })
		proxy, err := c.Transport.(*http.Transport).Proxy(r)
		if scheme == "socks5" {
			if err == nil || proxy != nil {
				t.Fatal("unsupported proxy accepted")
			}
		} else if err != nil || proxy.Scheme != scheme {
			t.Fatal("proxy selection changed")
		}
	}
}

func TestVPSAdminTLSAndRedirectNoCredentialForwarding(t *testing.T) {
	received := 0
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received++
		t.Fatal("redirect forwarded to receiving site")
	}))
	defer foreign.Close()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, foreign.URL, 302)
			return
		}
		_, _ = io.WriteString(w, `{"status":true,"response":{}}`)
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	p := fixtureProvider(trustedHTTPClient(func(*http.Request) (*url.URL, error) { return nil, nil }), nil)
	p.origin, _ = url.Parse(server.URL)
	u, _ := p.destination("/")
	if _, err := p.request(context.Background(), "OPTIONS", u, nil, false, true); err == nil {
		t.Fatal("untrusted TLS accepted")
	}
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	p.doer.(*http.Client).Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool}
	if _, err := p.request(context.Background(), "OPTIONS", u, nil, false, true); err != nil {
		t.Fatal(err)
	}
	p.authenticated = true
	p.tokenHeader = "X-Fixture-Token"
	p.token = "synthetic-secret"
	u, _ = p.destination("/redirect")
	if _, err := p.request(context.Background(), "GET", u, nil, true, false); err == nil || received != 0 {
		t.Fatal("redirect succeeded/forwarded")
	}
	p.origin, _ = url.Parse(strings.Replace(server.URL, "127.0.0.1", "localhost", 1))
	u, _ = p.destination("/")
	if _, err := p.request(context.Background(), "OPTIONS", u, nil, false, true); err == nil {
		t.Fatal("hostname mismatch accepted")
	}
}

func TestVPSAdminTotalBudgetAcrossTricklingBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":true,"response":`)
		w.(http.Flusher).Flush()
		for i := 0; i < 20; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
				_, _ = io.WriteString(w, " ")
				w.(http.Flusher).Flush()
			}
		}
		_, _ = io.WriteString(w, `{}}`)
	}))
	defer server.Close()
	p := fixtureProvider(trustedHTTPClient(func(*http.Request) (*url.URL, error) { return nil, nil }), nil)
	p.origin, _ = url.Parse(server.URL)
	p.budget = 20 * time.Millisecond
	u, _ := p.destination("/")
	if _, err := p.request(context.Background(), "GET", u, nil, false, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("body escaped total request deadline", err)
	}
}
