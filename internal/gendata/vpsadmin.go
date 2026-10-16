package gendata

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	api "github.com/vpsfreecz/vpsadmin-go-client/client"
)

const productionAPI = "https://api.vpsfree.cz"
const requestBudget = 60 * time.Second

// Prompt is an invocation-owned reader. protected means echo must be hidden;
// terminal handling belongs to the later core adapter, not this HTTP provider.
type Prompt func(context.Context, string, bool) (string, error)

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// VPSAdmin owns only this invocation's generated-schema actions and token state.
// It is sequential, has no persistent config, and performs no network in its
// constructor. The endpoint/Doer/clock/budget fixture seam remains private.
type VPSAdmin struct {
	origin                        *url.URL
	doer                          httpDoer
	now                           func() time.Time
	prompt                        Prompt
	warn                          func(string)
	warnError                     func(string) error
	budget                        time.Duration
	user, password, token         string
	validTo                       *time.Time
	authenticated, authenticating bool
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

func validHeaderValue(value string) bool {
	for i := range value {
		if value[i] < 32 && value[i] != '\t' || value[i] == 127 {
			return false
		}
	}
	return true
}

// rawAction is the generated action's context-aware dispatch, not a local
// route/parameter interpreter. Raw mode avoids decoding unused typed fields.
type rawAction func(context.Context) (*api.RawResponse, error)

type providerHTTP struct{ provider *VPSAdmin }

func (t providerHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	p := t.provider
	if _, err := p.destination(r.URL.String()); err != nil {
		return nil, err
	}
	r.Header.Set("Accept", "application/json")
	r.Header.Set("User-Agent", "haveapi-client")
	response, err := p.doer.Do(r)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, err
	}
	return response, nil
}

func (p *VPSAdmin) call(ctx context.Context, authenticated bool, namespace string, prepare func(*api.Client) rawAction) (json.RawMessage, json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if authenticated {
		if err := p.EnsureToken(ctx); err != nil {
			return nil, nil, err
		}
		if !validHeaderValue(p.token) {
			return nil, nil, errors.New("invalid authentication HTTP header")
		}
	}
	c := api.New(p.origin.String())
	c.SetHTTPClient(&http.Client{Transport: providerHTTP{p}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})
	if authenticated {
		c.SetExistingTokenAuth(p.token)
	}
	requestCtx, cancel := context.WithTimeout(ctx, p.budget)
	defer cancel()
	raw, err := prepare(c)(requestCtx)
	if err != nil {
		if requestCtx.Err() != nil {
			return nil, nil, requestCtx.Err()
		}
		return nil, nil, &apiFailure{kind: "API request failed"}
	}
	if raw == nil {
		return nil, nil, &apiFailure{kind: "missing API response"}
	}
	return decodeResponse(raw, namespace)
}

func decodeResponse(raw *api.RawResponse, namespace string) (json.RawMessage, json.RawMessage, error) {
	status := raw.StatusCode
	if status >= 300 && status < 400 {
		return nil, nil, &apiFailure{kind: "API redirect refused", status: status}
	}
	if status == 403 {
		return nil, nil, &apiFailure{kind: "API access forbidden", status: status}
	}
	if (status < 200 || status >= 300) && status != 400 && status != 404 {
		return nil, nil, &apiFailure{kind: "API HTTP request failed", status: status}
	}
	obj, err := rawObject(raw.Body)
	if err != nil {
		return nil, nil, &apiFailure{kind: "invalid API JSON", status: status}
	}
	var success bool
	if absentOrNull(obj["status"]) || json.Unmarshal(obj["status"], &success) != nil {
		return nil, nil, &apiFailure{kind: "malformed API envelope", status: status}
	}
	if !success {
		return nil, nil, &apiFailure{kind: "API action failed", status: status, message: obj["message"], fields: obj["errors"]}
	}
	if absentOrNull(obj["response"]) {
		return nil, nil, &apiFailure{kind: "missing API response payload", status: status}
	}
	response, err := rawObject(obj["response"])
	if err != nil {
		return nil, nil, errors.New("malformed API action response")
	}
	if absentOrNull(response[namespace]) {
		return nil, nil, errors.New("missing API output namespace")
	}
	return response[namespace], response["_meta"], nil
}

type consumedObject struct {
	provider *VPSAdmin
	data     json.RawMessage
}

func (o *consumedObject) getter(name string) (json.RawMessage, error) {
	data, err := rawObject(o.data)
	if err != nil {
		return nil, errors.New("malformed consumed resource object")
	}
	return data[name], nil
}

// Only the known node -> location -> environment chain can trigger a Show.
// Included resolved objects never dispatch; unresolved metadata is read lazily.
func (o *consumedObject) association(ctx context.Context, name string) (*consumedObject, error) {
	value, err := o.getter(name)
	if err != nil || absentOrNull(value) {
		return nil, err
	}
	data, err := rawObject(value)
	if err != nil {
		return nil, errors.New("malformed consumed association")
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
		return &consumedObject{o.provider, value}, nil
	}
	if len(args) != 1 {
		return nil, errors.New("missing association path parameter")
	}
	id, err := scalarText(args[0])
	if err != nil {
		return nil, errors.New("invalid association path parameter")
	}
	raw, _, err := o.provider.call(ctx, true, name, func(c *api.Client) rawAction {
		switch name {
		case "node":
			return c.Node.Show.Prepare().SetPathParamString("node_id", id).CallRaw
		case "location":
			return c.Location.Show.Prepare().SetPathParamString("location_id", id).CallRaw
		case "environment":
			return c.Environment.Show.Prepare().SetPathParamString("environment_id", id).CallRaw
		}
		panic("unknown internal association")
	})
	if err != nil {
		return nil, err
	}
	return &consumedObject{o.provider, raw}, nil
}

func scalarField[T any](o *consumedObject, name string, decode func(json.RawMessage) (T, error)) Field[T] {
	return deferredField(func() (Field[T], error) {
		value, err := o.getter(name)
		if err != nil {
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

func projectedScalar[T any](o *consumedObject, name string) Field[T] {
	var done bool
	var value any
	var err error
	return Field[T]{scalar: func() (any, error) {
		if done {
			return value, err
		}
		done = true
		var raw json.RawMessage
		raw, err = o.getter(name)
		if err != nil || absentOrNull(raw) {
			return nil, err
		}
		value, err = jsonValue(raw)
		if err == nil {
			value, err = sourcePrimitive(value)
		}
		return value, err
	}}
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
	text, err := scalarText(value)
	if err != nil {
		return VPS{}, err
	}
	raw, _, err := p.call(ctx, true, "vps", func(c *api.Client) rawAction {
		inv := c.Vps.Show.Prepare().SetPathParamString("vps_id", text)
		inv.NewMetaInput().SetIncludes("node__location__environment")
		return inv.CallRaw
	})
	if err != nil {
		return VPS{}, err
	}
	ct := &consumedObject{provider: p, data: raw}
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
	raw, _, err := p.call(ctx, true, "networks", func(c *api.Client) rawAction { return c.Network.Index.Prepare().CallRaw })
	if err != nil {
		return nil, err
	}
	var rows []json.RawMessage
	if json.Unmarshal(raw, &rows) != nil || absentOrNull(raw) {
		return nil, errors.New("malformed network response list")
	}
	networks := make([]Network, 0, len(rows))
	for _, row := range rows {
		o := &consumedObject{provider: p, data: row}
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
