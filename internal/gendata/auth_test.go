package gendata

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestVPSAdminRepeatedNonTOTPChallengeAndExpiry(t *testing.T) {
	ctx := context.Background()
	prompts := []string{}
	requests := []string{}
	step := 0
	p := fixtureProvider(doerFunc(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		if r.URL.Path == "/auth/request" || r.URL.Path == "/auth/device" {
			if r.Header.Get("X-Fixture-Token") != "" {
				t.Fatal("auth validity/header re-entry")
			}
		}
		switch r.URL.Path {
		case "/":
			return fixtureResponse(200, providerDescription), nil
		case "/auth/request":
			return fixtureResponse(200, `{"status":true,"response":{"login":{"complete":false,"next_action":"device","token":"step-a"}}}`), nil
		case "/auth/device":
			step++
			body, _ := io.ReadAll(r.Body)
			value, err := jsonValue(body)
			token := "step-a"
			if step%2 == 0 {
				token = "step-b"
			}
			want := map[string]any{"challenge": map[string]any{"token": token, "answer": json.Number("9007199254740993"), "approved": true}}
			if err != nil || !reflect.DeepEqual(value, want) {
				t.Fatal("challenge coercion/token input changed")
			}
			if step%2 == 1 {
				return fixtureResponse(200, `{"status":true,"response":{"login":{"complete":false,"next_action":"device","token":"step-b"}}}`), nil
			}
			return fixtureResponse(200, `{"status":true,"response":{"login":{"complete":true,"token":"installed","valid_to":"2026-10-14T11:59:59Z"}}}`), nil
		case "/alternate/networks":
			if r.Header.Get("X-Fixture-Token") != "installed" {
				t.Fatal("pending resource token missing")
			}
			return fixtureResponse(200, `{"status":true,"response":{"networks":[]}}`), nil
		}
		t.Fatal("unexpected request")
		return nil, errors.New("fixture unexpected request")
	}), func(_ context.Context, label string, protected bool) (string, error) {
		prompts = append(prompts, label)
		switch label {
		case "User name: ":
			return "", nil
		case "Password: ":
			if !protected {
				t.Fatal("password echo")
			}
			return "", nil
		case "Answer: ":
			if !protected {
				t.Fatal("challenge echo")
			}
			return " +9007199254740993 ", nil
		case "approved: ":
			if protected {
				t.Fatal("public challenge hidden")
			}
			return "YeS", nil
		}
		return "", errors.New("unexpected prompt")
	})
	// This fixture permits initial empty credentials (Ruby strings are truthy).
	p.doer = wrapDescription(p.doer, strings.ReplaceAll(providerDescription, `"validators":{"present":{}}`, `"validators":{}`))
	if err := p.Authenticate(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prompts, []string{"User name: ", "Password: ", "Answer: ", "approved: ", "Answer: ", "approved: "}) {
		t.Fatal("prompt descriptor order", prompts)
	}
	before := len(requests)
	if _, err := p.ListNetworks(ctx); err != nil {
		t.Fatal(err)
	}
	// Expired initial token triggers one renewal before description, then one
	// before the separate list request; each new expired token still dispatches.
	if len(requests)-before != 8 || step != 6 {
		t.Fatal("expiry recursion/replay changed", requests, step)
	}
	if len(prompts) != 14 {
		t.Fatal("initial credentials were reprompted", prompts)
	}
}

func wrapDescription(inner httpDoer, description string) httpDoer {
	return doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "OPTIONS" {
			response, err := inner.Do(r)
			if err != nil {
				return response, err
			}
			_ = response.Body.Close()
			return fixtureResponse(200, description), nil
		}
		return inner.Do(r)
	})
}

func TestVPSAdminExpiryAbsentEqualPastAndNoReplay(t *testing.T) {
	for _, state := range []string{"absent", "equal", "future", "past"} {
		t.Run(state, func(t *testing.T) {
			p, trace := testAuthenticated(t, func(*http.Request) (*http.Response, error) { return fixtureResponse(403, `secret reflection`), nil })
			p.resources = json.RawMessage(providerDescriptionResponse(t))
			if state != "absent" {
				value := p.now()
				if state == "future" {
					value = value.Add(time.Second)
				}
				if state == "past" {
					value = value.Add(-time.Second)
				}
				p.validTo = &value
			}
			before := len(*trace)
			_, err := p.ListNetworks(context.Background())
			if err == nil || !strings.Contains(err.Error(), "access forbidden") {
				t.Fatal(err)
			}
			want := 1
			if state == "past" {
				want = 2
			}
			if len(*trace)-before != want {
				t.Fatal("failed request replay/expiry behavior", *trace)
			}
		})
	}
}

func providerDescriptionResponse(t *testing.T) string {
	t.Helper()
	obj, err := rawObject(json.RawMessage(providerDescription))
	if err != nil {
		t.Fatal(err)
	}
	return string(obj["response"])
}

func TestVPSAdminAuthCoercionAndValidatorActivation(t *testing.T) {
	for _, test := range []struct {
		kind     string
		raw      any
		nullable bool
		want     any
		bad      bool
	}{
		{"Integer", " +0009007199254740993 ", false, json.Number("9007199254740993"), false},
		{"Integer", "-0002", false, json.Number("-2"), false},
		{"Integer", float64(2), false, json.Number("2"), false},
		{"Integer", float64(1.5), false, nil, true},
		{"Integer", "0x10", false, nil, true},
		{"Integer", "", false, nil, true},
		{"Resource", json.Number("-1"), false, json.Number("-1"), false},
		{"Resource", "00012", false, json.Number("12"), false},
		{"Resource", " 12 ", false, nil, true},
		{"Resource", "-1", false, nil, true},
		{"Resource", " ", true, nil, false},
		{"Boolean", " YES ", false, true, false}, {"Boolean", json.Number("0"), false, false, false},
		{"Boolean", json.Number("1.0"), false, nil, true}, {"Boolean", "no", false, false, false},
		{"Float", "-1.25e2", false, float64(-125), false}, {"Float", "NaN", false, nil, true},
		{"Float", math.Inf(1), false, nil, true},
		{"Datetime", "2026-10-14T12:00:00.75+02:00", false, "2026-10-14T12:00:00+02:00", false},
		// DateTime#to_time retains a numeric offset; Time#iso8601 drops fractions.
		{"Datetime", "20261014", false, "2026-10-14T00:00:00+00:00", false},
		{"Datetime", "2026-W42-3T12:00:00Z", false, "2026-10-14T12:00:00+00:00", false},
		{"Datetime", "2026W423", false, "2026-10-14T00:00:00+00:00", false},
		{"Datetime", "2026-287T12:00:00Z", false, "2026-10-14T12:00:00+00:00", false},
		{"Datetime", "2025-W53-1", false, nil, true},
		{"String", true, false, "true", false}, {"Text", json.Number("9007199254740993"), false, "9007199254740993", false},
		{"String", float64(1), false, "1.0", false},
		{"String", json.Number("1e3"), false, "1000.0", false},
		{"String", []any{}, false, nil, true}, {"String", nil, true, nil, false}, {"String", nil, false, nil, true},
		{"String", "\t ", true, nil, false}, {"String", "\u00a0", true, "\u00a0", false},
	} {
		nullable, _ := json.Marshal(test.nullable)
		kind, _ := json.Marshal(test.kind)
		got, err := coerceAuth(map[string]json.RawMessage{"type": kind, "nullable": nullable}, test.raw)
		if (err != nil) != test.bad || !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%s coercion mismatch: error=%v", test.kind, err)
		}
	}
	for _, validators := range []string{"null", "{}"} {
		input, _ := rawObject(json.RawMessage(`{"parameters":{"field":{"type":"Integer","validators":` + validators + `}}}`))
		got, err := actionParams(input, map[string]any{"field": "bad"})
		if validators == "null" {
			if err != nil || got["field"] != nil {
				t.Fatal("strengthened validator-free coercion")
			}
		} else if err == nil {
			t.Fatal("coercion errors ignored with active validators")
		}
	}
}

func TestVPSAdminActualValidatorsAndBoundedRubyRegex(t *testing.T) {
	for _, test := range []struct {
		name, opts string
		value      any
		params     map[string]any
		bad        bool
	}{
		{"present", `{}`, false, nil, false}, {"present", `{}`, " ", nil, true}, {"present", `{"empty":true}`, "", nil, false},
		{"present", `{"empty":0}`, "", nil, false},
		{"accept", `{"value":"yes"}`, "yes", nil, false},
		{"accept", `{"value":1}`, float64(1), nil, false},
		{"accept", `{"value":1}`, "1", nil, true},
		{"confirm", `{"parameter":"other","equal":true}`, "a", map[string]any{"other": "a"}, false},
		{"confirm", `{"parameter":"other","equal":false}`, nil, nil, true},
		{"include", `{"values":{"12":"label"}}`, json.Number("12"), nil, false},
		{"exclude", `{"values":["a","b"]}`, "a", nil, true},
		{"length", `{"equals":2}`, "é😀", nil, false}, {"length", `{"min":1,"max":2}`, "abc", nil, true},
		{"number", `{"min":1,"max":9,"step":2,"odd":true}`, json.Number("5"), nil, false},
		{"number", `{"mod":2,"even":true}`, json.Number("3"), nil, true},
		{"number", `{"mod":0.1}`, float64(0.3), nil, true},
		{"number", `{"odd":true}`, float64(5), nil, true},
		{"number", `{}`, "-1", nil, true},
		{"format", `{"rx":"\\A[0-9]{6}\\z","match":true}`, "123456", nil, false},
		{"format", `{"rx":"\\A[0-9]{6}\\z","match":true}`, "123456\n", nil, true},
		{"custom", `{}`, "anything", nil, false}, {"unknown", `{}`, "a", nil, true},
		{"format", `{"rx":"^a$","match":true}`, "a", nil, true},
		{"format", `{"rx":"(?<=a)b","match":true}`, "ab", nil, true},
		{"format", `{"rx":"\\p{Word}","match":true}`, "a", nil, true},
	} {
		err := validateAuth(namedJSON{test.name, json.RawMessage(test.opts)}, test.value, test.params)
		if (err != nil) != test.bad {
			t.Fatal("validator behavior differs", test.name, err)
		}
	}
	for _, name := range []string{"present", "presence", "required"} {
		input, _ := rawObject(json.RawMessage(`{"parameters":{"field":{"type":"String","validators":{"` + name + `":{}}}}}`))
		if _, err := actionParams(input, nil); err == nil {
			t.Fatal("missing presence obligation ignored")
		}
	}
	input, _ := rawObject(json.RawMessage(`{"parameters":{"field":{"type":"String","validators":{"present":false}}}}`))
	if _, err := actionParams(input, nil); err != nil {
		t.Fatal("false presence validator strengthened missing-field handling", err)
	}
	for _, pattern := range []string{`\A[[:digit:]]+\z`, `(a)\1`, `(?m:a)`, `\A\w+\z`, `a$`, `é`} {
		if _, err := authRegexp(pattern); err == nil {
			t.Fatal("non-equivalent Ruby pattern accepted")
		}
	}
}

func TestVPSAdminAuthFailureRedactionAndNoReprompt(t *testing.T) {
	prompts, requests := 0, 0
	p := fixtureProvider(doerFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method == "OPTIONS" {
			return fixtureResponse(200, providerDescription), nil
		}
		return fixtureResponse(400, `{"status":false,"message":"synthetic-password synthetic-token","errors":{"user":"synthetic-user"}}`), nil
	}), func(context.Context, string, bool) (string, error) { prompts++; return "synthetic-password", nil })
	err := p.Authenticate(context.Background())
	if err == nil || prompts != 2 || requests != 2 || strings.Contains(err.Error(), "synthetic-") || p.authenticated {
		t.Fatal("auth failure leaked/retried")
	}
	var failure *apiFailure
	if !errors.As(err, &failure) || len(failure.message) == 0 || len(failure.fields) == 0 {
		t.Fatal("API envelope error fields lost")
	}
	var authFailure *AuthenticationError
	if !errors.As(err, &authFailure) {
		t.Fatal("authentication boundary lost")
	}
}

func TestVPSAdminAuthRegexpClassAdmission(t *testing.T) {
	// Ruby nesting/intersection semantics are outside the admitted Go subset.
	for _, pattern := range []string{
		`\A[a-z&&[def]]\z`,
		`\A[a-z[0-9]]\z`,
		`\A[a&&b]\z`,
		`\A[[ab]c]\z`,
		`\A[]a[b]]\z`,
		`\A[\]a[b]]\z`,
		`\A[\[a&&b]\z`,
		`\A[\\[ab]]\z`,
	} {
		t.Run(pattern, func(t *testing.T) {
			rx, err := authRegexp(pattern)
			if rx != nil || err == nil || err.Error() != "unsupported authentication format descriptor" {
				t.Fatalf("unsupported class admitted: regexp=%v error=%v", rx, err)
			}
		})
	}
	for _, test := range []struct{ pattern, match, miss string }{
		{`\A[a-z]{2}\z`, "ab", "a]"},
		{`\A[0-9]+\z`, "123", "a"},
		{`\A[a-cd-f]+\z`, "bed", "g"},
		{`\A[\[\]]+\z`, "[][", "a"},
		{`\A[]a]+\z`, "]a]", "b"},
		{`\A[a\]]+\z`, "a]", "b"},
		{`\A[\\]+\z`, `\`, "a"},
		{`\A[a&b]+\z`, "a&b", "c"},
		{`\Aa&&b\z`, "a&&b", "a&b"},
		{`\A\[\]\z`, "[]", "]["},
		{`\A[]a][b-c]\z`, "]b", "]d"},
		{`\A\[[ab]\]\z`, "[a]", "[c]"},
	} {
		t.Run(test.pattern, func(t *testing.T) {
			rx, err := authRegexp(test.pattern)
			if err != nil || !rx.MatchString(test.match) || rx.MatchString(test.miss) {
				t.Fatalf("supported class changed: regexp=%v error=%v", rx, err)
			}
			for _, match := range []bool{true, false} {
				opts, err := json.Marshal(map[string]any{"rx": test.pattern, "match": match})
				if err != nil {
					t.Fatal(err)
				}
				for _, value := range []string{test.match, test.miss} {
					err := validateAuth(namedJSON{"format", opts}, value, nil)
					wantPass := (value == test.match) == match
					if (err == nil) != wantPass {
						t.Fatal("supported match/inverse validation changed", err)
					}
				}
			}
		})
	}
}

func TestVPSAdminAuthClassValidationBeforeDispatch(t *testing.T) {
	for _, test := range []struct {
		name, pattern, match, miss string
		unsupported                bool
	}{
		{"reviewer-intersection", `\A[a-z&&[def]]\z`, "d", "a]", true},
		{"nested", `\A[a-z[0-9]]\z`, "0", "a]", true},
		{"intersection-without-nesting", `\A[a&&b]\z`, "a", "&", true},
		{"literal-first-close", `\A[]a[b]]\z`, "]", "b]", true},
		{"escaped-close", `\A[\]a[b]]\z`, "]", "b]", true},
		{"escaped-open-intersection", `\A[\[a&&b]\z`, "[", "&", true},
		{"escaped-backslash-nesting", `\A[\\[ab]]\z`, `\`, "a]", true},
		{"ordinary-range", `\A[a-z]{2}\z`, "ab", "a]", false},
		{"escaped-brackets", `\A[\[\]]+\z`, "[]", "a", false},
		{"literal-first-close-supported", `\A[]a]+\z`, "]a", "b", false},
		{"outside-ampersands", `\Aa&&b\z`, "a&&b", "ab", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, stage := range []string{"request", "device"} {
				t.Run(stage, func(t *testing.T) {
					for _, match := range []bool{true, false} {
						for _, answer := range []string{test.match, test.miss, "synthetic-answer"} {
							want := ""
							if test.unsupported {
								want = "unsupported authentication format descriptor"
							} else if (answer == test.match) != match {
								want = "action parameter validation failed"
							}
							checkClassAuthenticate(t, test.pattern, match, answer, stage, want)
						}
					}
				})
			}
		})
	}
}

func checkClassAuthenticate(t *testing.T, pattern string, match bool, answer, stage, want string) {
	t.Helper()
	opts, err := json.Marshal(map[string]any{"rx": pattern, "match": match})
	if err != nil {
		t.Fatal(err)
	}
	old := `"password":{"type":"String","validators":{"present":{}}}`
	replacement := `"password":{"type":"String","validators":{"format":` + string(opts) + `}}`
	if stage == "device" {
		old = `"answer":{"type":"Integer","label":"Answer","protected":true,"validators":{"present":{}}}`
		replacement = `"answer":{"type":"String","label":"Answer","protected":true,"validators":{"format":` + string(opts) + `}}`
	}
	if strings.Count(providerDescription, old) != 1 {
		t.Fatal("fixture descriptor target changed")
	}
	description := strings.Replace(providerDescription, old, replacement, 1)
	trace := []string{}
	p := fixtureProvider(doerFunc(func(r *http.Request) (*http.Response, error) {
		trace = append(trace, r.Method+" "+r.URL.RequestURI())
		switch r.Method + " " + r.URL.Path {
		case "OPTIONS /":
			return fixtureResponse(200, description), nil
		case "POST /auth/request":
			if stage == "device" {
				return fixtureResponse(200, `{"status":true,"response":{"login":{"complete":false,"next_action":"device","token":"synthetic-step-token"}}}`), nil
			}
			return fixtureResponse(200, `{"status":true,"response":{"login":{"complete":true,"token":"synthetic-installed-token"}}}`), nil
		case "PUT /auth/device":
			return fixtureResponse(200, `{"status":true,"response":{"login":{"complete":true,"token":"synthetic-installed-token"}}}`), nil
		default:
			t.Fatal("unexpected fixture request")
			return nil, errors.New("unexpected fixture request")
		}
	}), func(_ context.Context, label string, protected bool) (string, error) {
		trace = append(trace, "prompt "+label)
		switch label {
		case "User name: ":
			return "synthetic-user", nil
		case "Password: ":
			if !protected {
				t.Fatal("password protection changed")
			}
			if stage == "request" {
				return answer, nil
			}
			return "synthetic-password", nil
		case "Answer: ":
			if !protected {
				t.Fatal("answer protection changed")
			}
			return answer, nil
		case "approved: ":
			return "yes", nil
		}
		t.Fatal("unexpected fixture prompt")
		return "", errors.New("unexpected fixture prompt")
	})
	warnings := []string{}
	p.warn = func(s string) { warnings = append(warnings, s) }
	err = p.Authenticate(context.Background())
	wantTrace := []string{"prompt User name: ", "prompt Password: ", "OPTIONS /?describe=default"}
	if stage == "device" {
		wantTrace = append(wantTrace, "POST /auth/request", "prompt Answer: ", "prompt approved: ")
	}
	if want == "" {
		method := "POST /auth/request"
		if stage == "device" {
			method = "PUT /auth/device"
		}
		wantTrace = append(wantTrace, method)
		if err != nil || !p.authenticated {
			t.Fatalf("supported auth validation failed: error=%v", err)
		}
	} else {
		var authFailure *AuthenticationError
		if !errors.As(err, &authFailure) || err.Error() != "authentication failed: "+want {
			t.Fatalf("auth validation boundary changed: error=%v", err)
		}
		if errors.Unwrap(authFailure).Error() != want || p.authenticated || p.token != "" {
			t.Fatal("auth refusal installed a token or changed the redacted cause")
		}
	}
	if p.authenticating || !reflect.DeepEqual(trace, wantTrace) || len(warnings) != 0 {
		t.Fatal("auth request/prompt/refusal order changed", trace)
	}
}
