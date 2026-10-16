package gendata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	api "github.com/vpsfreecz/vpsadmin-go-client/client"
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

// Authenticate prompts once. Reauthentication reuses only this invocation's
// credentials and never attaches an old installed token to an auth action.
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
	p.user, p.password = user, password
	if err := p.authenticateCredentials(ctx); err != nil {
		return err
	}
	p.authenticated = true
	return nil
}

func (p *VPSAdmin) EnsureToken(ctx context.Context) error {
	if !p.authenticated {
		return errors.New("vpsAdmin provider is not authenticated")
	}
	if p.authenticating {
		return errors.New("authentication re-entry refused")
	}
	// Equality is unexpired. A replacement already expired is still used once by
	// this pending request, without recursively authenticating or replaying it.
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

func (p *VPSAdmin) authenticateCredentials(ctx context.Context) (err error) {
	defer func() { err = authenticationError(err) }()
	if p.authenticating {
		return errors.New("authentication re-entry refused")
	}
	p.authenticating = true
	defer func() { p.authenticating = false }()
	name := "request"
	input := map[string]string{"user": p.user, "password": p.password}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := p.authStep(ctx, name, input)
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
		input = map[string]string{"token": result.token}
		switch name {
		case "totp":
			input["code"], err = p.answer(ctx, "TOTP code: ", false)
		case "reset_password":
			input["new_password1"], err = p.answer(ctx, "New password: ", true)
			if err == nil {
				input["new_password2"], err = p.answer(ctx, "Repeat new password: ", true)
			}
		default:
			return errors.New("unsupported authentication action")
		}
		if err != nil {
			return err
		}
	}
}

func (p *VPSAdmin) authStep(ctx context.Context, name string, input map[string]string) (result authResult, err error) {
	var call rawAction
	// This finite schema binding uses selected input setters. In particular scope
	// is omitted, so the server's normal default applies.
	build := func(c *api.Client) rawAction {
		token := api.NewAuthTokenResourceToken(c)
		switch name {
		case "request":
			inv := token.Request.Prepare()
			inv.NewInput().SetUser(input["user"]).SetPassword(input["password"]).SetLifetime("fixed").SetInterval(60)
			return inv.CallRaw
		case "totp":
			inv := token.Totp.Prepare()
			inv.NewInput().SetToken(input["token"]).SetCode(input["code"])
			return inv.CallRaw
		case "reset_password":
			inv := token.ResetPassword.Prepare()
			inv.NewInput().SetToken(input["token"]).SetNewPassword1(input["new_password1"]).SetNewPassword2(input["new_password2"])
			return inv.CallRaw
		}
		return call
	}
	if name != "request" && name != "totp" && name != "reset_password" {
		return result, errors.New("unsupported authentication action")
	}
	raw, _, err := p.call(ctx, false, "token", build)
	if err != nil {
		return result, err
	}
	obj, err := rawObject(raw)
	if err != nil {
		return result, errors.New("malformed authentication output")
	}
	if absentOrNull(obj["complete"]) || json.Unmarshal(obj["complete"], &result.complete) != nil {
		return result, errors.New("malformed authentication completion")
	}
	if result.token, err = responseString(obj, "token"); err != nil {
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
	} else if result.nextAction, err = responseString(obj, "next_action"); err != nil || result.nextAction == "" {
		return result, errors.New("malformed next authentication action")
	}
	return result, nil
}

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
