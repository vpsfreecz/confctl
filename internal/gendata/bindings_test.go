package gendata

import (
	"context"
	"errors"
	"io"
	"net/http"
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
		if client.authenticated || client.token != "" || client.user != "" || client.password != "" || client.validTo != nil {
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

// Exercise the exported callback seam through generated actions and raw output.
func TestVPSAdminBindingsActualUse(t *testing.T) {
	calls, clocks, closed := 0, 0, 0
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	prompt := func(_ context.Context, label string, _ bool) (string, error) { return "synthetic-credential", nil }
	client, err := NewVPSAdminWithBindings(prompt, func(string) { t.Fatal("unexpected schema warning") }, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Scheme != "https" || r.URL.Host != "api.vpsfree.cz" {
			t.Fatal("fixed origin changed")
		}
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > requestBudget {
			t.Fatal("budget missing")
		}
		body := `{"status":true,"response":{"token":{"complete":true,"token":"bound","valid_to":"2026-10-14T12:00:00Z"}}}`
		switch calls {
		case 1:
			if r.Method != "POST" || r.URL.Path != "/_auth/token/tokens" || r.Header.Get("X-HaveAPI-Auth-Token") != "" {
				t.Fatal("auth binding")
			}
		case 2:
			if r.Method != "GET" || r.URL.RequestURI() != "/v7.0/networks" || r.Header.Get("X-HaveAPI-Auth-Token") != "bound" {
				t.Fatal("resource binding")
			}
			body = `{"status":true,"response":{"networks":[]}}`
		default:
			t.Fatal("extra request")
		}
		response := fixtureResponse(200, body)
		response.Body = &bindingsBody{ReadCloser: response.Body, closed: &closed}
		return response, nil
	}, func() time.Time { clocks++; return now })
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Authenticate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = Networks(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || clocks != 1 || closed != 2 {
		t.Fatal(calls, clocks, closed)
	}
}

type bindingsBody struct {
	io.ReadCloser
	closed *int
}

func (b *bindingsBody) Close() error { *b.closed++; return b.ReadCloser.Close() }
