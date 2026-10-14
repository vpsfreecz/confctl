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

func TestVPSAdminBindingsConstruction(t *testing.T) {
	called := func() { t.Fatal("constructor reached a callback") }
	do := func(*http.Request) (*http.Response, error) { called(); return nil, nil }
	now := func() time.Time { called(); return time.Time{} }
	prompt := func(context.Context, string, bool) (string, error) { called(); return "", nil }
	warn := func(string) { called() }
	for _, test := range []struct {
		do   func(*http.Request) (*http.Response, error)
		now  func() time.Time
		want string
	}{
		{nil, nil, "vpsAdmin HTTP binding is unavailable"},
		{nil, now, "vpsAdmin HTTP binding is unavailable"},
		{do, nil, "vpsAdmin clock binding is unavailable"},
	} {
		p, err := NewVPSAdminWithBindings(prompt, warn, test.do, test.now)
		if p != nil || err == nil || err.Error() != test.want {
			t.Fatal("nil binding precedence changed")
		}
	}
	p, err := NewVPSAdminWithBindings(prompt, warn, do, now)
	q, qerr := NewVPSAdminWithBindings(nil, nil, do, now)
	if err != nil || qerr != nil || p == q || p.origin.String() != productionAPI || p.budget != 60*time.Second {
		t.Fatal("constructor policy changed")
	}
	for _, client := range []*VPSAdmin{p, q} {
		if client.authenticated || client.token != "" || client.user != "" || client.password != "" || client.authDescription != nil || client.resources != nil || client.validTo != nil {
			t.Fatal("constructor reused auth state")
		}
	}
	ordinary := NewVPSAdmin(nil, nil)
	client, ok := ordinary.doer.(*http.Client)
	if !ok || ordinary.origin.String() != productionAPI || ordinary.budget != requestBudget || client.CheckRedirect == nil {
		t.Fatal("ordinary constructor changed")
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || !transport.DisableKeepAlives || transport.ForceAttemptHTTP2 || transport.TLSNextProto == nil {
		t.Fatal("ordinary transport changed")
	}
	err = q.Authenticate(context.Background())
	var authErr *AuthenticationError
	if !errors.As(err, &authErr) || err.Error() != "authentication failed: authentication prompt is unavailable" {
		t.Fatal("nil prompt was prevalidated or reclassified")
	}
}

type bindingsBody struct {
	io.Reader
	closed *int
}

func (b bindingsBody) Close() error { *b.closed++; return nil }

func TestVPSAdminBindingsActualUse(t *testing.T) {
	ctx := context.Background()
	trace := []string{}
	closed, clocks, requests := 0, 0, 0
	instant := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	var client *VPSAdmin
	do := func(r *http.Request) (*http.Response, error) {
		requests++
		trace = append(trace, r.Method+" "+r.URL.RequestURI())
		if r.URL.Scheme != "https" || r.URL.Host != "api.vpsfree.cz" || r.Header.Get("User-Agent") != "haveapi-client" {
			t.Fatal("fixed request identity changed")
		}
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > requestBudget {
			t.Fatal("request budget absent")
		}
		body := ""
		switch requests {
		case 1, 3:
			if r.Method != "OPTIONS" || r.URL.RequestURI() != "/?describe=default" || r.Body != nil {
				t.Fatal("description sequence changed")
			}
			want := ""
			if requests == 3 {
				want = "synthetic-token"
			}
			if r.Header.Get("X-Fixture-Token") != want {
				t.Fatal("description auth timing changed")
			}
			body = providerDescription
		case 2:
			if r.Method != "POST" || r.URL.Path != "/auth/request" || r.Header.Get("X-Fixture-Token") != "" {
				t.Fatal("token request changed")
			}
			raw, err := io.ReadAll(r.Body)
			value, decodeErr := jsonValue(raw)
			want := map[string]any{"credentials": map[string]any{"user": "synthetic-user", "password": "synthetic-password", "lifetime": "fixed", "interval": json.Number("60")}}
			if err != nil || decodeErr != nil || !reflect.DeepEqual(value, want) {
				t.Fatal("credential body changed")
			}
			body = `{"status":true,"response":{"login":{"complete":true,"token":"synthetic-token","valid_to":"2026-10-14T12:00:00Z"}}}`
		case 4:
			if r.Method != "GET" || r.URL.RequestURI() != "/alternate/networks" || r.Header.Get("X-Fixture-Token") != "synthetic-token" {
				t.Fatal("resource binding not reached")
			}
			body = `{"status":true,"response":{"networks":[{"ip_version":4,"address":"192.0.2.0","prefix":24}]}}`
		default:
			t.Fatal("request escaped finite script")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: bindingsBody{strings.NewReader(body), &closed}}, nil
	}
	prompt := func(c context.Context, label string, protected bool) (string, error) {
		if c != ctx {
			t.Fatal("prompt context changed")
		}
		trace = append(trace, "prompt "+label)
		if label == "User name: " && !protected {
			return " synthetic-user \t", nil
		}
		if label == "Password: " && protected {
			return "synthetic-password", nil
		}
		t.Fatal("unexpected prompt")
		return "", errors.New("unexpected fixture prompt")
	}
	var err error
	client, err = NewVPSAdminWithBindings(prompt, func(string) { t.Fatal("unexpected warning") }, do, func() time.Time { clocks++; trace = append(trace, "clock"); return instant })
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Authenticate(ctx); err != nil {
		t.Fatal("auth binding failed")
	}
	networks, err := client.ListNetworks(ctx)
	if err != nil || len(networks) != 1 {
		t.Fatal("resource binding failed")
	}
	projected, err := ProjectNetworks(networks)
	want := map[string]any{"ipv4": []any{map[string]any{"address": "192.0.2.0", "prefix": json.Number("24")}}, "ipv6": []any{}}
	if err != nil || !reflect.DeepEqual(projected, want) {
		t.Fatal("actual projection changed")
	}
	wantTrace := []string{"prompt User name: ", "prompt Password: ", "OPTIONS /?describe=default", "POST /auth/request", "clock", "OPTIONS /?describe=default", "clock", "GET /alternate/networks"}
	if !reflect.DeepEqual(trace, wantTrace) || clocks != 2 || closed != 4 {
		t.Fatal("callback/close order changed")
	}
	other, err := NewVPSAdminWithBindings(prompt, nil, do, func() time.Time { return instant })
	if err != nil || other.authenticated || other.resources != nil || other.token != "" {
		t.Fatal("new invocation retained client state")
	}
}
