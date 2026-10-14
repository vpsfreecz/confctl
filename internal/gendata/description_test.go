package gendata

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestVPSAdminDescriptionProtocolAliasesAndOrderedMembers(t *testing.T) {
	for _, version := range []string{"2.0", "2.1", "2.other", "3.0", ""} {
		warnings := 0
		p := fixtureProvider(nil, nil)
		p.warn = func(string) { warnings++ }
		encoded, _ := json.Marshal(version)
		err := p.descriptionVersion(map[string]json.RawMessage{"version": encoded})
		bad := version == "" || version == "3.0"
		if (err != nil) != bad {
			t.Fatal("major protocol behavior", version, err)
		}
		want := 0
		if !bad && version != "2.0" {
			want = 1
		}
		if warnings != want {
			t.Fatal("minor warning behavior")
		}
	}
	obj, err := objectMembers(json.RawMessage(`{"b":1,"a":2,"b":3}`))
	if err != nil || len(obj) != 2 || obj[0].name != "b" || string(obj[0].raw) != "3" || obj[1].name != "a" {
		t.Fatal("source object insertion/duplicate behavior")
	}
	resource, err := resourceAt(json.RawMessage(providerDescriptionResponse(t)), []string{"network"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := actionFrom(resource, "list")
	if err != nil || a.path != "/alternate/networks" || a.method != "GET" {
		t.Fatal("alias/default-version route lost", err)
	}
	for _, raw := range []string{`null`, `[]`, `{}`, `{"actions":{"show":{"method":"PATCH","path":"/","output":{}}}}`} {
		if _, err := actionFrom(json.RawMessage(raw), "show"); err == nil {
			t.Fatal("malformed consumed action accepted")
		}
	}
}

func TestVPSAdminPathNullExactNumbersAndNamespaceMethods(t *testing.T) {
	for _, test := range []struct {
		args []any
		want string
	}{
		{[]any{nil}, "/v/"},
		{[]any{json.Number("9007199254740993")}, "/v/9007199254740993"},
		{[]any{"a /+?&"}, "/v/a%20%2F%2B%3F%26"},
	} {
		got, err := preparedPath("/v/{id}", test.args)
		if err != nil || got != test.want {
			t.Fatal("path conversion differs", got, err)
		}
	}
	if _, err := preparedPath("/v/{id}", nil); err == nil {
		t.Fatal("unresolved placeholder accepted")
	}
	for _, method := range []string{"GET", "DELETE", "POST", "PUT"} {
		input, _ := rawObject(json.RawMessage(`{"namespace":"step","parameters":{"z":{"type":"String"},"a":{"type":"String"}}}`))
		output, _ := rawObject(json.RawMessage(`{"namespace":"result","layout":"hash"}`))
		p := fixtureProvider(doerFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != method {
				t.Fatal("method changed")
			}
			if method == "GET" || method == "DELETE" {
				if r.Body != nil || r.URL.RawQuery != "step%5Bz%5D=&step%5Ba%5D=a+b&_meta%5Bincludes%5D=node" {
					t.Fatal("query namespace/order/nil differs", r.URL.RawQuery)
				}
			} else {
				body, _ := io.ReadAll(r.Body)
				value, err := jsonValue(body)
				want := map[string]any{"step": map[string]any{"z": nil, "a": "a b"}, "_meta": map[string]any{"includes": "node"}}
				if err != nil || !reflect.DeepEqual(value, want) {
					t.Fatal("body namespace differs")
				}
			}
			return fixtureResponse(200, `{"status":true,"response":{"result":{},"_meta":{"count":1}}}`), nil
		}), nil)
		_, meta, err := p.callAction(context.Background(), apiAction{method, "/route", input, output}, nil, map[string]any{"z": nil, "a": "a b"}, map[string]any{"includes": "node"}, false)
		if err != nil || string(meta) != `{"count":1}` {
			t.Fatal("response metadata lost", err)
		}
	}
}

type countedBody struct {
	io.Reader
	closes int
}

func (b *countedBody) Close() error { b.closes++; return nil }

func TestVPSAdminResponseFailuresCloseAndRedact(t *testing.T) {
	for _, test := range []struct {
		status     int
		body, kind string
	}{
		{403, "private-body", "API access forbidden"},
		{302, "", "API redirect refused"},
		{500, "private-body", "API HTTP request failed"},
		{400, `{"status":false,"message":"synthetic-secret"}`, "API action failed"},
		{404, `{"status":false,"errors":{"field":"synthetic-secret"}}`, "API action failed"},
		{200, `not-json`, "invalid API JSON"},
		{200, `null`, "invalid API JSON"},
		{200, `{}`, "malformed API envelope"},
		{200, `{"status":"true","response":{}}`, "malformed API envelope"},
		{200, `{"status":true}`, "missing API response payload"},
		{200, `{"status":true,"response":{}} trailing`, "invalid API JSON"},
	} {
		body := &countedBody{Reader: strings.NewReader(test.body)}
		p := fixtureProvider(doerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: test.status, Body: body}, nil
		}), nil)
		u, _ := p.destination("/")
		_, err := p.request(context.Background(), "GET", u, nil, false, false)
		if err == nil || err.Error() != test.kind || body.closes != 1 || strings.Contains(err.Error(), "synthetic-secret") {
			t.Fatal("error/close/redaction behavior", test.status, err, body.closes)
		}
	}
	called := 0
	p := fixtureProvider(doerFunc(func(*http.Request) (*http.Response, error) {
		called++
		return nil, errors.New("URL contains synthetic-password")
	}), nil)
	u, _ := p.destination("/")
	if _, err := p.request(context.Background(), "GET", u, nil, false, false); err == nil || strings.Contains(err.Error(), "synthetic-password") {
		t.Fatal("transport error reflection")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.request(ctx, "GET", u, nil, false, false); !errors.Is(err, context.Canceled) || called != 1 {
		t.Fatal("canceled request dispatched", err)
	}
}

type deadlineBody struct {
	ctx    context.Context
	closes int
}

func (b *deadlineBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b *deadlineBody) Close() error             { b.closes++; return nil }

func TestVPSAdminRequestBudgetAndCancellationCloseBody(t *testing.T) {
	var body *deadlineBody
	p := fixtureProvider(doerFunc(func(r *http.Request) (*http.Response, error) {
		deadline, exists := r.Context().Deadline()
		if !exists || time.Until(deadline) > 25*time.Millisecond {
			t.Fatal("total request budget not attached before dispatch")
		}
		body = &deadlineBody{ctx: r.Context()}
		return &http.Response{StatusCode: 200, Body: body}, nil
	}), nil)
	p.budget = 25 * time.Millisecond
	u, _ := p.destination("/")
	if _, err := p.request(context.Background(), "GET", u, nil, false, false); !errors.Is(err, context.DeadlineExceeded) || body.closes != 1 {
		t.Fatal("body deadline/close changed", err)
	}
}

func TestVPSAdminLazySchemaAndSelectedFieldErrorOrder(t *testing.T) {
	for _, body := range []string{
		`{"status":true,"response":{"networks":[{"ip_version":6,"address":{}},{"ip_version":4,"prefix":[]}]}}`,
		`{"status":true,"response":{"networks":[{"ip_version":6,"address":{}},{"ip_version":4,"prefix":0,"address":"ok"}]}}`,
	} {
		p, _ := testAuthenticated(t, func(*http.Request) (*http.Response, error) { return fixtureResponse(200, body), nil })
		_, err := Networks(context.Background(), p)
		if err == nil {
			t.Fatal("selected malformed field accepted")
		}
		if strings.Contains(body, `"prefix":[]`) && !strings.Contains(err.Error(), "network 1") {
			t.Fatal("IPv6 field error preempted IPv4", err)
		}
	}
	for _, node := range []string{"null", "{}", `{"_meta":{"resolved":true},"id":[],"location":{"_meta":{"resolved":false,"path_params":[2]}}}`} {
		calls := 0
		p, _ := testAuthenticated(t, func(r *http.Request) (*http.Response, error) {
			calls++
			if strings.Contains(r.URL.Path, "vps") {
				return fixtureResponse(200, `{"status":true,"response":{"vps":{"node":`+node+`}}}`), nil
			}
			return fixtureResponse(500, ""), nil
		})
		_, err := Containers(context.Background(), p, []MachineInput{{Container: some(ContainerRef{ID: some(json.Number("1"))})}})
		if err == nil {
			t.Fatal("bad association/scalar accepted")
		}
		want := 1
		if node == "{}" {
			want = 1
		}
		if calls != want {
			t.Fatal("unused downstream association requested", calls, err)
		}
	}
}

func TestVPSAdminAuthenticationHeaderAndUnsupportedDescriptorBeforeDispatch(t *testing.T) {
	for _, desc := range []string{
		strings.Replace(providerDescription, `"X-Fixture-Token"`, `"Bad\r\nHeader"`, 1),
		strings.Replace(providerDescription, `"validators":{"present":{}}`, `"validators":{"format":{"rx":"^bad$","match":true}}`, 1),
	} {
		calls := 0
		p := fixtureProvider(doerFunc(func(*http.Request) (*http.Response, error) { calls++; return fixtureResponse(200, desc), nil }), func(context.Context, string, bool) (string, error) { return "synthetic-secret", nil })
		err := p.Authenticate(context.Background())
		if err == nil || calls != 1 || strings.Contains(err.Error(), "synthetic-secret") || strings.Contains(err.Error(), "bad$") {
			t.Fatal("unsupported descriptor/header dispatched or leaked", err, calls)
		}
	}
	p := fixtureProvider(nil, nil)
	p.authenticated = true
	p.tokenHeader = "X-Token"
	p.token = "synthetic\r\nsecret"
	u, _ := p.destination("/")
	if _, err := p.request(context.Background(), "GET", u, nil, true, false); err == nil {
		t.Fatal("invalid token header dispatched")
	}
}

func TestVPSAdminReadActionRequiredInputBeforeHTTP(t *testing.T) {
	p, trace := testAuthenticated(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid read input dispatched")
		return nil, errors.New("unexpected request")
	})
	root, _ := rawObject(json.RawMessage(providerDescriptionResponse(t)))
	resources, _ := rawObject(root["resources"])
	network, _ := rawObject(resources["network"])
	actions, _ := rawObject(network["actions"])
	index, _ := rawObject(actions["index"])
	index["input"] = json.RawMessage(`{"namespace":"filters","parameters":{"required":{"type":"String","validators":{"present":{}}}}}`)
	actions["index"], _ = json.Marshal(index)
	network["actions"], _ = json.Marshal(actions)
	resources["network"], _ = json.Marshal(network)
	root["resources"], _ = json.Marshal(resources)
	p.resources, _ = json.Marshal(root)
	before := len(*trace)
	if _, err := p.ListNetworks(context.Background()); err == nil || !strings.Contains(err.Error(), "required action parameter") || len(*trace) != before {
		t.Fatal("read action validation order changed", err)
	}
}
