package gendata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func rubyStrip(value string) string { return strings.Trim(value, " \t\n\v\f\r\x00") }

// AuthenticationError keeps the auth boundary distinct while retaining safe
// transport/envelope/validation causes for the owning CLI adapter.
type AuthenticationError struct{ cause error }

func (e *AuthenticationError) Error() string { return "authentication failed: " + e.cause.Error() }
func (e *AuthenticationError) Unwrap() error { return e.cause }

func authenticationError(err error) error {
	if err == nil {
		return nil
	}
	// Only an existing outer wrapper represents this authentication boundary;
	// an older AuthenticationError inside a trusted cause must not suppress it.
	if _, ok := err.(*AuthenticationError); ok {
		return err
	}
	return &AuthenticationError{cause: err}
}

func scalarText(value any) (string, error) {
	switch v := value.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case json.Number:
		if integralToken(string(v)) {
			integer, _ := new(big.Int).SetString(string(v), 10)
			return integer.String(), nil
		}
		n, err := v.Float64()
		if err != nil {
			return "", errors.New("invalid numeric scalar")
		}
		return rubyFloatText(n), nil
	case float64:
		return rubyFloatText(v), nil
	case time.Time:
		return v.Format(time.RFC3339), nil
	}
	return "", errors.New("unsupported scalar")
}

// Ruby's scalar Float#to_s uses fixed form through exponent14 and retains a
// decimal point, independently of Nix literal rendering and JSON number tokens.
func rubyFloatText(value float64) string {
	if math.IsNaN(value) {
		return "NaN"
	}
	if math.IsInf(value, 1) {
		return "Infinity"
	}
	if math.IsInf(value, -1) {
		return "-Infinity"
	}
	exponential := strconv.FormatFloat(value, 'e', -1, 64)
	pos := strings.IndexByte(exponential, 'e')
	exponent, _ := strconv.Atoi(exponential[pos+1:])
	if exponent >= -4 && exponent < 15 {
		text := strconv.FormatFloat(value, 'f', -1, 64)
		if !strings.Contains(text, ".") {
			text += ".0"
		}
		return text
	}
	mantissa := exponential[:pos]
	if !strings.Contains(mantissa, ".") {
		mantissa += ".0"
	}
	return mantissa + exponential[pos:]
}

func (p *VPSAdmin) answer(ctx context.Context, label string, protected bool) (string, error) {
	if p.prompt == nil {
		return "", errors.New("authentication prompt is unavailable")
	}
	value, err := p.prompt(ctx, label, protected)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("authentication prompt failed")
	}
	return rubyStrip(value), nil
}

func (p *VPSAdmin) Authenticate(ctx context.Context) (err error) {
	defer func() { err = authenticationError(err) }()
	user, err := p.answer(ctx, "User name: ", false)
	if err != nil {
		return err
	}
	password, err := p.answer(ctx, "Password: ", true)
	if err != nil {
		return err
	}
	description, err := p.discover(ctx, false)
	if err != nil {
		return err
	}
	obj, err := rawObject(description)
	if err != nil {
		return err
	}
	auth, err := rawObject(obj["authentication"])
	if err != nil {
		return errors.New("token authentication description is unavailable")
	}
	token, err := rawObject(auth["token"])
	if err != nil {
		return errors.New("token authentication description is unavailable")
	}
	header, err := descriptionString(token, "http_header")
	if err != nil || !validHeaderName(header) {
		return errors.New("invalid authentication HTTP header")
	}
	p.user, p.password, p.authDescription, p.tokenHeader = user, password, auth["token"], header
	if err := p.authenticateCredentials(ctx); err != nil {
		return err
	}
	p.authenticated = true
	return nil
}

// EnsureToken corrects source recursion deliberately: one reauthentication is
// allowed before this pending dispatch. AuthStep never re-enters this check or
// attaches an installed token, including a newly returned already-expired one.
func (p *VPSAdmin) EnsureToken(ctx context.Context) error {
	if !p.authenticated {
		return errors.New("vpsAdmin provider is not authenticated")
	}
	if p.authenticating {
		return errors.New("authentication re-entry refused")
	}
	if p.validTo != nil && p.validTo.Before(p.now()) {
		return p.authenticateCredentials(ctx)
	}
	return nil
}

type authResult struct {
	complete          bool
	nextAction, token string
	validTo           *time.Time
}

func (p *VPSAdmin) authAction(name string) (apiAction, error) {
	resource, err := resourceAt(p.authDescription, []string{"token"})
	if err != nil {
		return apiAction{}, err
	}
	return actionFrom(resource, name)
}

func (p *VPSAdmin) authenticateCredentials(ctx context.Context) (err error) {
	defer func() { err = authenticationError(err) }()
	if p.authenticating {
		return errors.New("authentication re-entry refused")
	}
	p.authenticating = true
	defer func() { p.authenticating = false }()
	name := "request"
	input := map[string]any{"user": p.user, "password": p.password, "lifetime": "fixed", "interval": json.Number("60")}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := p.AuthStep(ctx, name, input)
		if err != nil {
			return err
		}
		if result.complete {
			if !validHeaderValue(result.token) {
				return errors.New("invalid authentication HTTP header")
			}
			p.token, p.validTo = result.token, result.validTo
			return nil
		}
		name = result.nextAction
		a, err := p.authAction(name)
		if err != nil {
			return err
		}
		if a.input == nil {
			return errors.New("challenge input description is unavailable")
		}
		fields, err := objectMembers(a.input["parameters"])
		if err != nil {
			return errors.New("malformed action parameters")
		}
		input = map[string]any{"token": result.token}
		for _, field := range fields {
			if field.name == "token" {
				continue
			}
			desc, err := rawObject(field.raw)
			if err != nil {
				return errors.New("malformed action parameter")
			}
			label := field.name
			if !absentOrNull(desc["label"]) {
				if label, err = stringScalar(desc["label"]); err != nil {
					return errors.New("malformed authentication label")
				}
			}
			protected := false
			if !absentOrNull(desc["protected"]) && json.Unmarshal(desc["protected"], &protected) != nil {
				return errors.New("malformed authentication echo descriptor")
			}
			answer, err := p.answer(ctx, label+": ", protected)
			if err != nil {
				return err
			}
			input[field.name] = answer
		}
	}
}

func (p *VPSAdmin) AuthStep(ctx context.Context, name string, input map[string]any) (result authResult, err error) {
	defer func() { err = authenticationError(err) }()
	a, err := p.authAction(name)
	if err != nil {
		return authResult{}, err
	}
	if err := actionLayout(a, "object", "hash"); err != nil {
		return authResult{}, err
	}
	if a.input == nil {
		return authResult{}, errors.New("authentication input description is unavailable")
	}
	raw, _, err := p.callAction(ctx, a, nil, input, nil, false)
	if err != nil {
		return authResult{}, err
	}
	obj, err := rawObject(raw)
	if err != nil {
		return authResult{}, errors.New("malformed authentication output")
	}
	if absentOrNull(obj["complete"]) || json.Unmarshal(obj["complete"], &result.complete) != nil {
		return result, errors.New("malformed authentication completion")
	}
	if result.token, err = descriptionString(obj, "token"); err != nil {
		return result, errors.New("malformed authentication token")
	}
	if result.complete {
		if !absentOrNull(obj["valid_to"]) {
			text, err := stringScalar(obj["valid_to"])
			if err != nil {
				return result, errors.New("malformed authentication expiry")
			}
			value, err := parseAuthTime(text)
			if err != nil {
				return result, errors.New("malformed authentication expiry")
			}
			result.validTo = &value
		}
	} else {
		if result.nextAction, err = descriptionString(obj, "next_action"); err != nil || result.nextAction == "" {
			return result, errors.New("malformed next authentication action")
		}
	}
	return result, nil
}

func actionParams(input map[string]json.RawMessage, supplied map[string]any) (map[string]any, error) {
	ret := map[string]any{}
	if input == nil {
		return ret, nil
	}
	fields, err := objectMembers(input["parameters"])
	if err != nil {
		return nil, errors.New("malformed action parameters")
	}
	coercionErrors := map[string]error{}
	descriptors := map[string]map[string]json.RawMessage{}
	for _, field := range fields {
		desc, err := rawObject(field.raw)
		if err != nil {
			return nil, errors.New("malformed action parameter")
		}
		descriptors[field.name] = desc
		if value, present := supplied[field.name]; present {
			ret[field.name], coercionErrors[field.name] = coerceAuth(desc, value)
		}
	}
	for _, field := range fields {
		desc := descriptors[field.name]
		if absentOrNull(desc["validators"]) {
			continue
		} // Source Params does not enforce coercion errors here.
		validators, err := objectMembers(desc["validators"])
		if err != nil {
			return nil, errors.New("malformed action validators")
		}
		value, supplied := ret[field.name]
		if !supplied {
			for _, v := range validators {
				if (v.name == "presence" || v.name == "present" || v.name == "required") && rawTruthy(v.raw) {
					return nil, errors.New("required action parameter missing")
				}
			}
			continue
		}
		if coercionErrors[field.name] != nil {
			return nil, errors.New("action parameter coercion failed")
		}
		typeName, _ := descriptionString(desc, "type")
		if typeName == "Resource" {
			continue
		} // Resource#valid? checks coercion only.
		for _, v := range validators {
			if err := validateAuth(v, value, ret); err != nil {
				return nil, err
			}
		}
	}
	return ret, nil
}

func coerceAuth(desc map[string]json.RawMessage, raw any) (any, error) {
	nullable := false
	if !absentOrNull(desc["nullable"]) && json.Unmarshal(desc["nullable"], &nullable) != nil {
		return nil, errors.New("malformed nullability")
	}
	if raw == nil {
		if nullable {
			return nil, nil
		}
		return nil, errors.New("non-nullable authentication value")
	}
	if s, ok := raw.(string); ok && rubyStrip(s) == "" && nullable {
		return nil, nil
	}
	kind, err := descriptionString(desc, "type")
	if err != nil {
		return nil, err
	}
	switch kind {
	case "Integer", "Resource":
		var text string
		switch v := raw.(type) {
		case json.Number:
			if integralToken(string(v)) {
				integer, _ := new(big.Int).SetString(string(v), 10)
				return json.Number(integer.String()), nil
			}
			if kind == "Resource" {
				return nil, errors.New("invalid resource ID")
			}
			n, err := v.Float64()
			if err != nil || math.IsInf(n, 0) || n != math.Trunc(n) {
				return nil, errors.New("invalid integer")
			}
			text = strconv.FormatFloat(n, 'f', 0, 64)
		case float64:
			if kind == "Resource" || math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) {
				return nil, errors.New("invalid integer")
			}
			text = strconv.FormatFloat(v, 'f', 0, 64)
		case string:
			text = rubyStrip(v)
			if kind == "Resource" {
				text = v
				if strings.HasPrefix(text, "+") || strings.HasPrefix(text, "-") {
					return nil, errors.New("invalid resource ID")
				}
			}
		default:
			return nil, errors.New("invalid integer")
		}
		if !signedDecimal.MatchString(text) {
			return nil, errors.New("invalid integer")
		}
		integer, ok := new(big.Int).SetString(text, 10)
		if !ok {
			return nil, errors.New("invalid integer")
		}
		return json.Number(integer.String()), nil
	case "Float":
		text, err := scalarText(raw)
		if err != nil {
			return nil, errors.New("invalid float")
		}
		switch raw.(type) {
		case string, json.Number, float64:
		default:
			return nil, errors.New("invalid float")
		}
		n, err := strconv.ParseFloat(rubyStrip(text), 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, errors.New("invalid float")
		}
		return n, nil
	case "Boolean":
		if value, ok := raw.(bool); ok {
			return value, nil
		}
		if value, ok := raw.(json.Number); ok {
			if value == "1" {
				return true, nil
			}
			if value == "0" {
				return false, nil
			}
		}
		if value, ok := raw.(string); ok {
			switch strings.ToLower(rubyStrip(value)) {
			case "true", "t", "yes", "y", "1":
				return true, nil
			case "false", "f", "no", "n", "0":
				return false, nil
			}
		}
		return nil, errors.New("invalid boolean")
	case "Datetime":
		if value, ok := raw.(time.Time); ok {
			return value.Format(time.RFC3339), nil
		}
		text, ok := raw.(string)
		if !ok {
			return nil, errors.New("invalid datetime")
		}
		value, err := parseAuthTime(text)
		if err != nil {
			return nil, err
		}
		return value.Format("2006-01-02T15:04:05-07:00"), nil
	case "String", "Text":
		value, err := scalarText(raw)
		if err != nil {
			return nil, err
		}
		return value, nil
	default:
		return raw, nil
	}
}

var signedDecimal = regexp.MustCompile(`^[+-]?[0-9]+$`)

func parseAuthTime(text string) (time.Time, error) {
	// DateTime.iso8601 accepts the date-only/basic forms as well as extended
	// timestamps. Values serialize to whole-second ISO8601, as Time#iso8601 does.
	text = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(text, ",", "."), "t", "T"), "z", "Z")
	date, clock, hasClock := strings.Cut(text, "T")
	if match := authWeekDate.FindStringSubmatch(date); match != nil {
		year, _ := strconv.Atoi(match[1])
		week, _ := strconv.Atoi(match[2])
		day := 1
		if match[3] != "" {
			day, _ = strconv.Atoi(match[3])
		}
		jan4 := time.Date(year, 1, 4, 0, 0, 0, 0, time.UTC)
		weekday := (int(jan4.Weekday()) + 6) % 7
		value := jan4.AddDate(0, 0, -weekday+(week-1)*7+day-1)
		actualYear, actualWeek := value.ISOWeek()
		if week < 1 || day < 1 || day > 7 || actualYear != year || actualWeek != week {
			return time.Time{}, errors.New("invalid ISO8601 authentication week date")
		}
		date = fmt.Sprintf("%04d-%02d-%02d", value.Year(), value.Month(), value.Day())
		text = date
		if hasClock {
			text += "T" + clock
		}
	}
	for _, layout := range []string{
		time.RFC3339Nano, "2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05Z07", "2006-01-02T15:04:05",
		"2006-01-02T15:04Z07:00", "2006-01-02T15:04Z0700", "2006-01-02T15:04", "20060102T150405Z0700", "20060102T150405",
		"2006-002T15:04:05Z07:00", "2006-002T15:04:05", "2006002T150405Z0700", "2006002T150405",
		"2006-01-02", "20060102", "2006-002", "2006002",
	} {
		if value, err := time.Parse(layout, text); err == nil {
			return value, nil
		}
	}
	return time.Time{}, errors.New("invalid ISO8601 authentication datetime")
}

var authWeekDate = regexp.MustCompile(`^([0-9]{4})-?W([0-9]{2})(?:-?([0-9]))?$`)

// Go's regex engine is used only for a deliberately bounded equivalent subset.
// Ruby line anchors, lookaround, backreferences, encoding/classes and modes are
// refused before auth dispatch, never silently skipped or delegated to Ruby.
func authRegexp(pattern string) (*regexp.Regexp, error) {
	unsupported := func() (*regexp.Regexp, error) { return nil, errors.New("unsupported authentication format descriptor") }
	var b strings.Builder
	// Ruby nested classes and && are operators, not Go class literals. Escaped
	// brackets and a leading literal ] must not change the class boundary.
	var inClass, classFirst bool
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c >= 128 || c == '^' || c == '$' || c < 32 || strings.Contains(pattern, "(?") || strings.Contains(pattern, "[:") {
			return unsupported()
		}
		if c == '\\' {
			if i+1 >= len(pattern) {
				return unsupported()
			}
			i++
			next := pattern[i]
			if next == 'A' && i == 1 {
				b.WriteByte('^')
				continue
			}
			if next == 'z' && i == len(pattern)-1 {
				b.WriteByte('$')
				continue
			}
			if !strings.ContainsRune(`.\+*?()[]{}|^-$/`, rune(next)) {
				return unsupported()
			}
			b.WriteByte('\\')
			b.WriteByte(next)
			if inClass {
				classFirst = false
			}
		} else {
			if c == '[' {
				if inClass {
					return unsupported()
				}
				inClass, classFirst = true, true
			} else if inClass {
				if c == '&' && i+1 < len(pattern) && pattern[i+1] == '&' {
					return unsupported()
				}
				if c == ']' && !classFirst {
					inClass = false
				}
				classFirst = false
			}
			b.WriteByte(c)
		}
	}
	rx, err := regexp.Compile(b.String())
	if err != nil {
		return unsupported()
	}
	return rx, nil
}

func validatorBool(opts map[string]json.RawMessage, key string) bool {
	return rawTruthy(opts[key])
}

func rawTruthy(raw json.RawMessage) bool {
	if absentOrNull(raw) {
		return false
	}
	value, err := jsonValue(raw)
	return err == nil && value != nil && value != false
}

func validateAuth(validator namedJSON, value any, params map[string]any) error {
	opts, err := rawObject(validator.raw)
	if err != nil {
		return errors.New("malformed authentication validator")
	}
	pass := false
	decode := func(key string) any { v, _ := jsonValue(opts[key]); return v }
	switch validator.name {
	case "present":
		pass = value != nil
		if s, ok := value.(string); ok && !validatorBool(opts, "empty") {
			pass = rubyStrip(s) != ""
		}
	case "accept":
		pass = authEqual(value, decode("value"))
	case "confirm":
		name, err := descriptionString(opts, "parameter")
		if err != nil {
			return errors.New("malformed confirmation descriptor")
		}
		other, exists := params[name]
		if validatorBool(opts, "equal") {
			pass = exists && authEqual(value, other)
		} else {
			pass = !authEqual(value, other)
		}
	case "include", "exclude":
		members := decode("values")
		switch list := members.(type) {
		case []any:
			for _, member := range list {
				if authEqual(value, member) {
					pass = true
					break
				}
			}
		case map[string]any:
			if validator.name == "include" {
				text, err := scalarText(value)
				if err == nil {
					_, pass = list[text]
				}
			} else {
				return errors.New("unsupported exclusion descriptor")
			}
		default:
			return errors.New("malformed inclusion descriptor")
		}
		if validator.name == "exclude" {
			pass = !pass
		}
	case "length":
		length := -1
		switch v := value.(type) {
		case string:
			length = utf8.RuneCountInString(v)
		case []any:
			length = len(v)
		case map[string]any:
			length = len(v)
		}
		if length < 0 {
			return errors.New("invalid length validator value")
		}
		n := new(big.Rat).SetInt64(int64(length))
		if !absentOrNull(opts["equals"]) {
			eq, ok := numberRat(decode("equals"))
			pass = ok && n.Cmp(eq) == 0
		} else {
			pass, err = numberBounds(n, opts)
		}
	case "number":
		if s, ok := value.(string); ok && !regexp.MustCompile(`^[0-9]+$`).MatchString(s) {
			return errors.New("authentication numerical validation failed")
		}
		n, ok := numberRat(value)
		if !ok {
			return errors.New("invalid numerical validator value")
		}
		pass, err = numberBounds(n, opts)
		for _, key := range []string{"step", "mod"} {
			if absentOrNull(opts[key]) {
				continue
			}
			divisor, ok := numberRat(decode(key))
			if !ok || divisor.Sign() == 0 {
				return errors.New("invalid numerical validator descriptor")
			}
			x := new(big.Rat).Set(n)
			if key == "step" && !absentOrNull(opts["min"]) {
				min, ok := numberRat(decode("min"))
				if !ok {
					return errors.New("invalid numerical minimum")
				}
				x.Sub(x, min)
			}
			floatArithmetic := floatingNumber(value) || floatingNumber(decode(key)) || key == "step" && floatingNumber(decode("min"))
			if floatArithmetic {
				v, _ := n.Float64()
				if key == "step" && !absentOrNull(opts["min"]) {
					min, _ := numberRat(decode("min"))
					m, _ := min.Float64()
					v -= m
				}
				d, _ := divisor.Float64()
				if math.Mod(v, d) != 0 {
					pass = false
				}
			} else if !new(big.Rat).Quo(x, divisor).IsInt() {
				pass = false
			}
		}
		if validatorBool(opts, "odd") || validatorBool(opts, "even") {
			if validatorBool(opts, "odd") && floatingNumber(value) {
				return errors.New("invalid parity validator value")
			}
			if !n.IsInt() {
				return errors.New("invalid parity validator value")
			}
			even := n.Num().Bit(0) == 0
			if validatorBool(opts, "odd") && even || validatorBool(opts, "even") && !even {
				pass = false
			}
		}
	case "format":
		pattern, err := descriptionString(opts, "rx")
		if err != nil {
			return errors.New("malformed authentication format descriptor")
		}
		rx, err := authRegexp(pattern)
		if err != nil {
			return err
		}
		s, ok := value.(string)
		if !ok {
			return errors.New("invalid format validator value")
		}
		pass = rx.MatchString(s) == validatorBool(opts, "match")
	case "custom":
		pass = true
	default:
		return errors.New("unsupported authentication validator")
	}
	if err != nil {
		return err
	}
	if !pass {
		return errors.New("action parameter validation failed")
	}
	return nil
}

func numberRat(value any) (*big.Rat, bool) {
	if floatingNumber(value) {
		var n float64
		switch v := value.(type) {
		case float64:
			n = v
		case json.Number:
			var err error
			n, err = v.Float64()
			if err != nil {
				return nil, false
			}
		}
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, false
		}
		return new(big.Rat).SetFloat64(n), true
	}
	text, err := scalarText(value)
	if err != nil {
		return nil, false
	}
	return new(big.Rat).SetString(text)
}

func floatingNumber(value any) bool {
	switch v := value.(type) {
	case float64:
		return true
	case json.Number:
		return !integralToken(string(v))
	}
	return false
}

func authEqual(a, b any) bool {
	isNumeric := func(value any) bool {
		switch value.(type) {
		case json.Number, float64:
			return true
		}
		return false
	}
	if isNumeric(a) && isNumeric(b) {
		x, xok := numberRat(a)
		y, yok := numberRat(b)
		return xok && yok && x.Cmp(y) == 0
	}
	return reflect.DeepEqual(a, b)
}

func numberBounds(value *big.Rat, opts map[string]json.RawMessage) (bool, error) {
	pass := true
	for _, key := range []string{"min", "max"} {
		if absentOrNull(opts[key]) {
			continue
		}
		raw, err := jsonValue(opts[key])
		if err != nil {
			return false, errors.New("malformed validator bound")
		}
		bound, ok := numberRat(raw)
		if !ok {
			return false, errors.New("invalid validator bound")
		}
		if key == "min" && value.Cmp(bound) < 0 || key == "max" && value.Cmp(bound) > 0 {
			pass = false
		}
	}
	return pass, nil
}
