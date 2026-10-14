package gendata

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const productionAPI = "https://api.vpsfree.cz"
const requestBudget = 60 * time.Second

// Prompt is an invocation-owned reader. protected means echo must be hidden;
// terminal handling belongs to the later core adapter, not this HTTP provider.
type Prompt func(context.Context, string, bool) (string, error)

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// VPSAdmin owns only this invocation's consumed description and token state.
// It is sequential, has no persistent config, and performs no network in its
// constructor. The endpoint/Doer/clock/budget fixture seam remains private.
type VPSAdmin struct {
	origin                             *url.URL
	doer                               httpDoer
	now                                func() time.Time
	prompt                             Prompt
	warn                               func(string)
	budget                             time.Duration
	authDescription                    json.RawMessage
	resources                          json.RawMessage
	user, password, token, tokenHeader string
	validTo                            *time.Time
	authenticated                      bool
	authenticating                     bool
}

func NewVPSAdmin(prompt Prompt, warn func(string)) *VPSAdmin {
	origin, _ := url.Parse(productionAPI)
	return &VPSAdmin{origin: origin, doer: trustedHTTPClient(http.ProxyFromEnvironment), now: time.Now, prompt: prompt, warn: warn, budget: requestBudget}
}

func trustedHTTPClient(proxy func(*http.Request) (*url.URL, error)) *http.Client {
	transport := &http.Transport{
		Proxy: func(r *http.Request) (*url.URL, error) {
			u, err := proxy(r)
			if err != nil {
				return nil, errors.New("HTTP proxy selection failed")
			}
			if u != nil && u.Scheme != "http" && u.Scheme != "https" {
				return nil, errors.New("unsupported HTTP proxy scheme")
			}
			return u, nil
		},
		DialContext:         (&net.Dialer{Timeout: requestBudget}).DialContext,
		TLSHandshakeTimeout: requestBudget,
		DisableKeepAlives:   true,
		ForceAttemptHTTP2:   false,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// apiFailure deliberately does not format server text, bodies, URLs, headers or
// underlying transport errors. Structured API errors stay in invocation memory.
type apiFailure struct {
	kind            string
	status          int
	message, fields json.RawMessage
}

func (e *apiFailure) Error() string { return e.kind }

func effectivePort(u *url.URL) string {
	if u.Port() != "" {
		return u.Port()
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

func (p *VPSAdmin) destination(path string) (*url.URL, error) {
	u, err := url.Parse(path)
	if err != nil || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("invalid API destination")
	}
	u = p.origin.ResolveReference(u)
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != p.origin.Scheme || !strings.EqualFold(u.Hostname(), p.origin.Hostname()) || effectivePort(u) != effectivePort(p.origin) || u.User != nil {
		return nil, errors.New("API destination is not same-origin")
	}
	return u, nil
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := range name {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			continue
		}
		return false
	}
	return true
}

func validHeaderValue(value string) bool {
	for i := range value {
		if value[i] < 32 && value[i] != '\t' || value[i] == 127 {
			return false
		}
	}
	return true
}

func (p *VPSAdmin) request(ctx context.Context, method string, destination *url.URL, body []byte, authenticated bool, description bool) (map[string]json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if authenticated {
		if err := p.EnsureToken(ctx); err != nil {
			return nil, err
		}
		if !validHeaderName(p.tokenHeader) || !validHeaderValue(p.token) {
			return nil, errors.New("invalid authentication HTTP header")
		}
	}
	requestCtx, cancel := context.WithTimeout(ctx, p.budget)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	r, err := http.NewRequestWithContext(requestCtx, method, destination.String(), reader)
	if err != nil {
		return nil, errors.New("invalid API request")
	}
	r.Header.Set("Accept", "application/json")
	r.Header.Set("User-Agent", "haveapi-client")
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if authenticated {
		r.Header.Set(p.tokenHeader, p.token)
	}
	response, err := p.doer.Do(r)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if requestCtx.Err() != nil {
			return nil, requestCtx.Err()
		}
		return nil, &apiFailure{kind: "API transport failed"}
	}
	if response == nil || response.Body == nil {
		return nil, &apiFailure{kind: "missing API response"}
	}
	defer response.Body.Close()
	status := response.StatusCode
	if status >= 300 && status < 400 {
		return nil, &apiFailure{kind: "API redirect refused", status: status}
	}
	if !description && status == http.StatusForbidden {
		return nil, &apiFailure{kind: "API access forbidden", status: status}
	}
	if status < 200 || status >= 300 {
		if description || status != 400 && status != 404 {
			return nil, &apiFailure{kind: "API HTTP request failed", status: status}
		}
	}
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		if requestCtx.Err() != nil {
			return nil, requestCtx.Err()
		}
		return nil, &apiFailure{kind: "API response body failed", status: status}
	}
	obj, err := rawObject(raw)
	if err != nil {
		return nil, &apiFailure{kind: "invalid API JSON", status: status}
	}
	var success bool
	if absentOrNull(obj["status"]) || json.Unmarshal(obj["status"], &success) != nil {
		return nil, &apiFailure{kind: "malformed API envelope", status: status}
	}
	if !success {
		return nil, &apiFailure{kind: "API action failed", status: status, message: obj["message"], fields: obj["errors"]}
	}
	if absentOrNull(obj["response"]) {
		return nil, &apiFailure{kind: "missing API response payload", status: status}
	}
	return obj, nil
}

func (p *VPSAdmin) discover(ctx context.Context, authenticated bool) (json.RawMessage, error) {
	u, err := p.destination("/?describe=default")
	if err != nil {
		return nil, err
	}
	obj, err := p.request(ctx, "OPTIONS", u, nil, authenticated, true)
	if err != nil {
		return nil, err
	}
	if err := p.descriptionVersion(obj); err != nil {
		return nil, err
	}
	if _, err := rawObject(obj["response"]); err != nil {
		return nil, errors.New("malformed API description")
	}
	return obj["response"], nil
}

func (p *VPSAdmin) resourceAction(ctx context.Context, path []string, name string) (apiAction, error) {
	if p.resources == nil {
		desc, err := p.discover(ctx, true)
		if err != nil {
			return apiAction{}, err
		}
		p.resources = desc
	}
	resource, err := resourceAt(p.resources, path)
	if err != nil {
		return apiAction{}, err
	}
	return actionFrom(resource, name)
}

var pathPlaceholder = regexp.MustCompile(`\{[a-zA-Z0-9_-]+\}`)

func preparedPath(path string, args []any) (string, error) {
	for _, arg := range args {
		loc := pathPlaceholder.FindStringIndex(path)
		if loc == nil {
			break
		}
		text, err := scalarText(arg)
		if err != nil {
			return "", errors.New("invalid API path argument")
		}
		encoded := strings.ReplaceAll(url.QueryEscape(text), "+", "%20")
		path = path[:loc[0]] + encoded + path[loc[1]:]
	}
	if pathPlaceholder.MatchString(path) {
		return "", errors.New("unresolved API path argument")
	}
	return path, nil
}

func (p *VPSAdmin) callAction(ctx context.Context, a apiAction, args []any, params map[string]any, meta map[string]any, authenticated bool) (json.RawMessage, json.RawMessage, error) {
	path, err := preparedPath(a.path, args)
	if err != nil {
		return nil, nil, err
	}
	u, err := p.destination(path)
	if err != nil {
		return nil, nil, err
	}
	var body []byte
	// The same source Params rules cover the consumed read and token actions.
	// Validate only at the reached action, before token validity/HTTP dispatch.
	params, err = actionParams(a.input, params)
	if err != nil {
		return nil, nil, err
	}
	if a.method == "POST" || a.method == "PUT" {
		payload := map[string]any{}
		if a.input != nil {
			if params == nil {
				params = map[string]any{}
			}
			ns, err := descriptionString(a.input, "namespace")
			if err != nil {
				return nil, nil, err
			}
			payload[ns] = params
		}
		if meta != nil {
			payload["_meta"] = meta
		}
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, nil, errors.New("invalid API input encoding")
		}
	} else {
		query := u.RawQuery
		appendParam := func(key string, value any) error {
			text, err := scalarText(value)
			if err != nil {
				return errors.New("invalid API query value")
			}
			if query != "" {
				query += "&"
			}
			query += url.QueryEscape(key) + "=" + url.QueryEscape(text)
			return nil
		}
		if a.input != nil {
			ns, err := descriptionString(a.input, "namespace")
			if err != nil {
				return nil, nil, err
			}
			fields, err := objectMembers(a.input["parameters"])
			if err != nil {
				return nil, nil, err
			}
			for _, f := range fields {
				if value, ok := params[f.name]; ok {
					if err := appendParam(ns+"["+f.name+"]", value); err != nil {
						return nil, nil, err
					}
				}
			}
		}
		if value, ok := meta["includes"]; ok {
			if err := appendParam("_meta[includes]", value); err != nil {
				return nil, nil, err
			}
		}
		u.RawQuery = query
	}
	obj, err := p.request(ctx, a.method, u, body, authenticated, false)
	if err != nil {
		return nil, nil, err
	}
	response, err := rawObject(obj["response"])
	if err != nil {
		return nil, nil, errors.New("malformed API action response")
	}
	ns, err := descriptionString(a.output, "namespace")
	if err != nil {
		return nil, nil, err
	}
	if absentOrNull(response[ns]) {
		return nil, nil, errors.New("missing API output namespace")
	}
	return response[ns], response["_meta"], nil
}

type consumedObject struct {
	provider *VPSAdmin
	action   apiAction
	data     json.RawMessage
	meta     json.RawMessage
}

func (o *consumedObject) getter(name string) (json.RawMessage, json.RawMessage, error) {
	params, err := rawObject(o.action.output["parameters"])
	if err != nil || len(params[name]) == 0 {
		return nil, nil, errors.New("consumed getter is unavailable")
	}
	data, err := rawObject(o.data)
	if err != nil {
		return nil, nil, errors.New("malformed consumed resource object")
	}
	return params[name], data[name], nil
}

func (o *consumedObject) association(ctx context.Context, name string) (*consumedObject, error) {
	desc, value, err := o.getter(name)
	if err != nil {
		return nil, err
	}
	param, err := rawObject(desc)
	if err != nil {
		return nil, err
	}
	typeName, err := descriptionString(param, "type")
	if err != nil || typeName != "Resource" {
		return nil, errors.New("consumed association has wrong description type")
	}
	if absentOrNull(value) {
		return nil, nil
	}
	data, err := rawObject(value)
	if err != nil {
		return nil, errors.New("malformed consumed association")
	}
	var path []string
	if json.Unmarshal(param["resource"], &path) != nil || len(path) == 0 {
		return nil, errors.New("malformed association resource path")
	}
	a, err := o.provider.resourceAction(ctx, path, "show")
	if err != nil {
		return nil, err
	}
	if err := actionLayout(a, "object"); err != nil {
		return nil, err
	}
	meta := data["_meta"]
	var resolved bool
	var args []any
	if !absentOrNull(meta) {
		m, err := rawObject(meta)
		if err != nil {
			return nil, errors.New("malformed association metadata")
		}
		if !absentOrNull(m["resolved"]) && json.Unmarshal(m["resolved"], &resolved) != nil {
			return nil, errors.New("malformed association resolved state")
		}
		if !resolved && !absentOrNull(m["path_params"]) {
			value, err := jsonValue(m["path_params"])
			var ok bool
			args, ok = value.([]any)
			if err != nil || !ok {
				return nil, errors.New("malformed association path parameters")
			}
		}
	}
	if resolved {
		return &consumedObject{o.provider, a, value, meta}, nil
	}
	dataRaw, responseMeta, err := o.provider.callAction(ctx, a, args, nil, nil, true)
	if err != nil {
		return nil, err
	}
	return &consumedObject{o.provider, a, dataRaw, responseMeta}, nil
}

func scalarField[T any](o *consumedObject, name string, decode func(json.RawMessage) (T, error)) Field[T] {
	return deferredField(func() (Field[T], error) {
		desc, value, err := o.getter(name)
		if err != nil {
			return Field[T]{}, err
		}
		if err := scalarDescription(desc); err != nil {
			return Field[T]{}, err
		}
		f := Field[T]{Present: len(value) != 0}
		if absentOrNull(value) {
			return f, nil
		}
		decoded, err := decode(value)
		if err != nil {
			return f, err
		}
		f.Value = &decoded
		return f, nil
	})
}

// ResourceInstance scalar getters return decoded values, not Typed input
// coercions. Read only this scalar when projection reaches it, preserving null,
// bool/string/numeric identity and exact numbers even in a nullable typed DTO.
func projectedScalar[T any](o *consumedObject, name string) Field[T] {
	var done bool
	var value any
	var err error
	return Field[T]{scalar: func() (any, error) {
		if done {
			return value, err
		}
		done = true
		var desc, raw json.RawMessage
		desc, raw, err = o.getter(name)
		if err != nil {
			return nil, err
		}
		if err = scalarDescription(desc); err != nil {
			return nil, err
		}
		if absentOrNull(raw) {
			return nil, nil
		}
		value, err = jsonValue(raw)
		if err == nil {
			value, err = sourcePrimitive(value)
		}
		return value, err
	}}
}

func scalarDescription(raw json.RawMessage) error {
	param, err := rawObject(raw)
	if err != nil {
		return errors.New("malformed consumed scalar descriptor")
	}
	kind, err := descriptionString(param, "type")
	if err != nil {
		return err
	}
	switch kind {
	case "Integer", "Float", "String", "Text", "Boolean", "Datetime":
		return nil
	}
	return errors.New("unsupported consumed scalar descriptor")
}

// This single-use closure belongs to one association/getter instance; it is not
// shared across rows or invocations. No eager decode or cross-VPS cache exists.
func deferredField[T any](load func() (Field[T], error)) Field[T] {
	var done bool
	var value *T
	var err error
	return Field[T]{read: func() (*T, error) {
		if !done {
			done = true
			var field Field[T]
			field, err = load()
			if err == nil {
				value, err = fieldValue(field, "deferred field")
			}
		}
		return value, err
	}}
}

func stringScalar(raw json.RawMessage) (string, error) {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", errors.New("malformed consumed string")
	}
	return value, nil
}

// MachineInputFromAttributes consumes the caller's ordered immutable snapshot,
// never Machine.Attr's display values. Container/ID access is before ShowVPS;
// host traversal stays lazy until after that request and before associations.
func MachineInputFromAttributes(name string, attributes map[string]any) MachineInput {
	machine := MachineInput{Name: name}
	machine.Container = deferredField(func() (Field[ContainerRef], error) {
		raw, present := attributes["container"]
		field := Field[ContainerRef]{Present: present}
		if raw == nil {
			return field, nil
		}
		object, ok := raw.(map[string]any)
		if !ok {
			return field, errors.New("container: expected structured object")
		}
		id, exists := object["id"]
		ref := ContainerRef{ID: Field[json.Number]{Present: exists}}
		if id != nil {
			if number, ok := id.(json.Number); ok && integralToken(string(number)) {
				ref.ID.Value = &number
			} else {
				ref.ID.scalar = func() (any, error) { return sourcePrimitive(id) }
			}
		}
		field.Value = &ref
		return field, nil
	})
	host := func(key string) Field[string] {
		return Field[string]{scalar: func() (any, error) {
			raw := attributes["host"]
			if raw == nil {
				return nil, nil
			}
			object, ok := raw.(map[string]any)
			if !ok {
				return nil, errors.New("host: expected structured object")
			}
			return sourcePrimitive(object[key])
		}}
	}
	machine.HostName, machine.HostLocation, machine.HostDomain = host("name"), host("location"), host("domain")
	return machine
}

func sourcePrimitive(value any) (any, error) {
	switch v := value.(type) {
	case nil, string, bool:
		return value, nil
	case json.Number:
		if string(v) == "" {
			return nil, errors.New("empty scalar JSON number")
		}
		if _, err := json.Marshal(v); err != nil {
			return nil, errors.New("invalid scalar JSON number")
		}
		return v, nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, errors.New("non-finite scalar primitive")
		}
		return v, nil
	}
	return nil, errors.New("malformed consumed scalar primitive")
}

func (p *VPSAdmin) ShowVPS(ctx context.Context, id Field[json.Number]) (VPS, error) {
	value, err := numberValue(id, "container.id")
	if err != nil {
		return VPS{}, err
	}
	a, err := p.resourceAction(ctx, []string{"vps"}, "show")
	if err != nil {
		return VPS{}, err
	}
	if err := actionLayout(a, "object"); err != nil {
		return VPS{}, err
	}
	raw, meta, err := p.callAction(ctx, a, []any{value}, nil, map[string]any{"includes": "node__location__environment"}, true)
	if err != nil {
		return VPS{}, err
	}
	ct := &consumedObject{p, a, raw, meta}
	return VPS{Node: deferredField(func() (Field[Node], error) {
		node, err := ct.association(ctx, "node")
		if err != nil || node == nil {
			return Field[Node]{}, err
		}
		v := Node{ID: projectedScalar[json.Number](node, "id"), Name: projectedScalar[string](node, "name"), DomainName: projectedScalar[string](node, "domain_name")}
		v.Location = deferredField(func() (Field[Location], error) {
			location, err := node.association(ctx, "location")
			if err != nil || location == nil {
				return Field[Location]{}, err
			}
			v := Location{Domain: projectedScalar[string](location, "domain")}
			v.Environment = deferredField(func() (Field[Environment], error) {
				environment, err := location.association(ctx, "environment")
				if err != nil || environment == nil {
					return Field[Environment]{}, err
				}
				v := Environment{Domain: projectedScalar[string](environment, "domain")}
				return Field[Environment]{Present: true, Value: &v}, nil
			})
			return Field[Location]{Present: true, Value: &v}, nil
		})
		return Field[Node]{Present: true, Value: &v}, nil
	})}, nil
}

func (p *VPSAdmin) ListNetworks(ctx context.Context) ([]Network, error) {
	a, err := p.resourceAction(ctx, []string{"network"}, "list")
	if err != nil {
		return nil, err
	}
	if err := actionLayout(a, "object_list"); err != nil {
		return nil, err
	}
	raw, _, err := p.callAction(ctx, a, nil, nil, nil, true)
	if err != nil {
		return nil, err
	}
	var rows []json.RawMessage
	if json.Unmarshal(raw, &rows) != nil || absentOrNull(raw) {
		return nil, errors.New("malformed network response list")
	}
	networks := make([]Network, 0, len(rows))
	for _, row := range rows {
		o := &consumedObject{p, a, row, nil}
		version := scalarField(o, "ip_version", func(raw json.RawMessage) (any, error) {
			value, err := jsonValue(raw)
			if err != nil {
				return nil, err
			}
			switch v := value.(type) {
			case json.Number:
				if integralToken(string(v)) {
					return v, nil
				}
				n, err := v.Float64()
				if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
					return nil, errors.New("non-finite network version")
				}
				return n, nil
			case string, bool:
				return value, nil
			}
			return nil, errors.New("unsupported network version primitive")
		})
		networks = append(networks, Network{IPVersion: version, Address: projectedScalar[string](o, "address"), Prefix: projectedScalar[json.Number](o, "prefix")})
	}
	return networks, nil
}
