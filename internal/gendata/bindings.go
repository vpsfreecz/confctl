package gendata

import (
	"errors"
	"net/http"
	"time"
)

type boundVPSAdminDoer func(*http.Request) (*http.Response, error)

func (do boundVPSAdminDoer) Do(r *http.Request) (*http.Response, error) { return do(r) }

// NewVPSAdminWithBindings borrows invocation-local request and clock callbacks.
// The ordinary constructor still owns the fixed origin and request budget;
// neither callback runs during construction, and no client lifetime is acquired.
func NewVPSAdminWithBindings(prompt Prompt, warn func(string), do func(*http.Request) (*http.Response, error), now func() time.Time) (*VPSAdmin, error) {
	if do == nil {
		return nil, errors.New("vpsAdmin HTTP binding is unavailable")
	}
	if now == nil {
		return nil, errors.New("vpsAdmin clock binding is unavailable")
	}
	p := NewVPSAdmin(prompt, warn)
	p.doer, p.now = boundVPSAdminDoer(do), now
	return p, nil
}
