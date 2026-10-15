package gendata

import (
	"context"
	"encoding/json"
	"errors"
	api "github.com/vpsfreecz/vpsadmin-go-client/client"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func testNetworkCall(p *VPSAdmin, ctx context.Context, authenticated bool) (json.RawMessage, json.RawMessage, error) {
	return p.call(ctx, authenticated, "networks", func(c *api.Client) rawAction { return c.Network.Index.Prepare().CallRaw })
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
		_, _, err := testNetworkCall(p, context.Background(), false)
		if err == nil || err.Error() != test.kind || body.closes != 1 || strings.Contains(err.Error(), "synthetic-secret") {
			t.Fatal("error/close/redaction behavior", test.status, err, body.closes)
		}
	}
	called := 0
	p := fixtureProvider(doerFunc(func(*http.Request) (*http.Response, error) {
		called++
		return nil, errors.New("URL contains synthetic-password")
	}), nil)
	if _, _, err := testNetworkCall(p, context.Background(), false); err == nil || strings.Contains(err.Error(), "synthetic-password") {
		t.Fatal("transport error reflection")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := testNetworkCall(p, ctx, false); !errors.Is(err, context.Canceled) || called != 1 {
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
	if _, _, err := testNetworkCall(p, context.Background(), false); !errors.Is(err, context.DeadlineExceeded) || body.closes != 1 {
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

func TestVPSAdminInvalidTokenBeforeDispatch(t *testing.T) {
	p := fixtureProvider(doerFunc(func(*http.Request) (*http.Response, error) { t.Fatal("invalid token dispatched"); return nil, nil }), nil)
	p.authenticated = true
	p.token = "synthetic\r\nsecret"
	if _, _, err := testNetworkCall(p, context.Background(), true); err == nil {
		t.Fatal("invalid header accepted")
	}
}
