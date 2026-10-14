package gendata

import (
	"errors"
	"io"
	"net/http"
	"time"
)

// WarningError preserves a trusted sink cause without exposing its contents in
// diagnostics. Authentication retains its existing outer error classification.
type WarningError struct{ cause error }

func (e *WarningError) Error() string { return "HaveAPI protocol warning output failed" }
func (e *WarningError) Unwrap() error { return e.cause }

// NewVPSAdminWithWarning borrows a required synchronous error-returning sink.
// Construction performs no I/O; a reached warning failure stops description
// processing immediately, before auth/resource state or later dispatch.
func NewVPSAdminWithWarning(prompt Prompt, warn func(string) error) (*VPSAdmin, error) {
	if warn == nil {
		return nil, errors.New("vpsAdmin warning binding is unavailable")
	}
	p := NewVPSAdmin(prompt, nil)
	p.warnError = warn
	return p, nil
}

// NewVPSAdminWithWarningBindings retains the reviewed fixed-origin request and
// clock binding constructor. The required warning sink is checked first.
func NewVPSAdminWithWarningBindings(prompt Prompt, warn func(string) error, do func(*http.Request) (*http.Response, error), now func() time.Time) (*VPSAdmin, error) {
	if warn == nil {
		return nil, errors.New("vpsAdmin warning binding is unavailable")
	}
	p, err := NewVPSAdminWithBindings(prompt, nil, do, now)
	if err != nil {
		return nil, err
	}
	p.warnError = warn
	return p, nil
}

// WarningTo borrows a writer and makes one message-plus-LF Write when reached.
// Production must supply unbuffered original stderr: this closure adds no
// Flush/Close, retry, context policy or delayed error reporting.
func WarningTo(w io.Writer) func(string) error {
	return func(message string) error {
		if w == nil {
			return errors.New("warning output is unavailable")
		}
		body := []byte(message + "\n")
		n, err := w.Write(body)
		if err != nil {
			return err
		}
		if n != len(body) {
			return io.ErrShortWrite
		}
		return nil
	}
}
