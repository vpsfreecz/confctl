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

func TestVPSAdminKnownChallengesAndExpiredReplacement(t *testing.T) {
	prompts := []string{}
	steps, resources := 0, 0
	p := fixtureProvider(doerFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, "/_auth/") {
			if r.Header.Get("X-HaveAPI-Auth-Token") != "" {
				t.Fatal("auth attached prior token")
			}
			steps++
			raw, _ := io.ReadAll(r.Body)
			obj, err := rawObject(raw)
			if err != nil {
				t.Fatal(err)
			}
			value, err := jsonValue(obj["token"])
			if err != nil {
				t.Fatal(err)
			}
			var want map[string]any
			result := `{"complete":false,"token":"step-a","next_action":"totp"}`
			switch (steps - 1) % 4 {
			case 0:
				if r.URL.Path != "/_auth/token/tokens" {
					t.Fatal(r.URL)
				}
				want = map[string]any{"user": "user", "password": "password", "lifetime": "fixed", "interval": json.Number("60")}
			case 1, 2:
				if r.URL.Path != "/_auth/token/tokens/totp" {
					t.Fatal(r.URL)
				}
				want = map[string]any{"token": "step-a", "code": "recovery-code"}
				if (steps-1)%4 == 2 {
					result = `{"complete":false,"token":"reset","next_action":"reset_password"}`
				}
			case 3:
				if r.URL.Path != "/_auth/token/tokens/reset_password" {
					t.Fatal(r.URL)
				}
				want = map[string]any{"token": "reset", "new_password1": "new-password", "new_password2": "new-password"}
				result = `{"complete":true,"token":"installed","valid_to":"2026-10-14T11:59:59Z"}`
			}
			if !reflect.DeepEqual(value, want) {
				t.Fatal("generated input differs", value, want)
			}
			return fixtureResponse(200, `{"status":true,"response":{"token":`+result+`}}`), nil
		}
		resources++
		if r.URL.Path != "/v7.0/networks" || r.Header.Get("X-HaveAPI-Auth-Token") != "installed" {
			t.Fatal("unexpected resource")
		}
		return fixtureResponse(200, `{"status":true,"response":{"networks":[]}}`), nil
	}), func(_ context.Context, label string, protected bool) (string, error) {
		prompts = append(prompts, label)
		switch label {
		case "User name: ":
			if protected {
				t.Fatal("user echo")
			}
			return " user \t", nil
		case "Password: ":
			if !protected {
				t.Fatal("password echo")
			}
			return "password", nil
		case "TOTP code: ":
			if protected {
				t.Fatal("TOTP schema echo")
			}
			return " recovery-code ", nil
		case "New password: ", "Repeat new password: ":
			if !protected {
				t.Fatal("reset echo")
			}
			return " new-password ", nil
		}
		t.Fatal("unknown prompt", label)
		return "", nil
	})
	if err := p.Authenticate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListNetworks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if steps != 8 || resources != 1 {
		t.Fatal("expired replacement recursed/replayed", steps, resources)
	}
	if !reflect.DeepEqual(prompts, []string{"User name: ", "Password: ", "TOTP code: ", "TOTP code: ", "New password: ", "Repeat new password: ", "TOTP code: ", "TOTP code: ", "New password: ", "Repeat new password: "}) {
		t.Fatal(prompts)
	}
}

func TestVPSAdminExpiryAbsentEqualPastAndNoReplay(t *testing.T) {
	for _, state := range []string{"absent", "equal", "future", "past"} {
		t.Run(state, func(t *testing.T) {
			p, trace := testAuthenticated(t, func(*http.Request) (*http.Response, error) { return fixtureResponse(403, "secret reflection"), nil })
			if state != "absent" {
				v := p.now()
				if state == "future" {
					v = v.Add(time.Second)
				}
				if state == "past" {
					v = v.Add(-time.Second)
				}
				p.validTo = &v
			}
			before := len(*trace)
			if _, err := p.ListNetworks(context.Background()); err == nil || !strings.Contains(err.Error(), "access forbidden") {
				t.Fatal(err)
			}
			want := 1
			if state == "past" {
				want = 2
			}
			if len(*trace)-before != want {
				t.Fatal("replay/reauth", *trace)
			}
		})
	}
}

func TestVPSAdminAuthFailureRedactionAndNoReprompt(t *testing.T) {
	for _, body := range []string{
		`{"status":false,"message":"synthetic-secret","errors":{"password":"synthetic-secret"}}`,
		`{"status":true,"response":{"token":{"complete":false,"token":"synthetic-secret","next_action":"future_action"}}}`,
		`{"status":true,"response":{"token":{"complete":true,"token":"bad\r\nsecret"}}}`,
		`{"status":true,"response":{"token":{"complete":true,"token":"synthetic-secret","valid_to":42}}}`,
	} {
		calls, prompts := 0, 0
		p := fixtureProvider(doerFunc(func(*http.Request) (*http.Response, error) { calls++; return fixtureResponse(200, body), nil }), func(context.Context, string, bool) (string, error) { prompts++; return "synthetic-secret", nil })
		err := p.Authenticate(context.Background())
		var auth *AuthenticationError
		if !errors.As(err, &auth) || strings.Contains(err.Error(), "synthetic-secret") || calls != 1 || prompts != 2 || p.authenticated {
			t.Fatal("unsafe auth failure", err, calls, prompts)
		}
	}
}

func TestVPSAdminAuthCancellationIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := fixtureProvider(doerFunc(func(r *http.Request) (*http.Response, error) { cancel(); return nil, r.Context().Err() }), func(context.Context, string, bool) (string, error) { return "credential", nil })
	err := p.Authenticate(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
