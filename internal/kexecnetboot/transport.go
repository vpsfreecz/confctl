package kexecnetboot

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const inactivity = 60 * time.Second

type transportDeps struct {
	dial  func(context.Context, string, string) (net.Conn, error)
	now   func() time.Time
	roots *x509.CertPool
}
type deadlineConn struct {
	net.Conn
	now func() time.Time
}

func (c *deadlineConn) Read(b []byte) (int, error) {
	if err := c.Conn.SetReadDeadline(c.now().Add(inactivity)); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}
func (c *deadlineConn) Write(b []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(c.now().Add(inactivity)); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}
func endpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid HTTP endpoint")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("endpoint must be absolute HTTP or HTTPS")
	}
	return u, nil
}
func publicURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid URL"
	}
	u.User = nil
	return u.String()
}
func guardedProxy(r *http.Request) (*url.URL, error) {
	u, err := http.ProxyFromEnvironment(r)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy configuration")
	}
	if u != nil && ((u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" || strings.HasSuffix(u.Host, ":")) {
		return nil, fmt.Errorf("proxy must use HTTP or HTTPS")
	}
	return u, nil
}

// Every response uses a fresh HTTP/1 connection. Socket activity refreshes its
// own deadline; dial/TLS limits do not impose a total download deadline.
func newClient(d transportDeps) (*http.Client, error) {
	if d.now == nil {
		d.now = time.Now
	}
	if d.dial == nil {
		dialer := &net.Dialer{Timeout: inactivity, KeepAlive: -1}
		d.dial = dialer.DialContext
	}
	if d.roots == nil {
		var err error
		d.roots, err = x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("load system TLS trust: %w", err)
		}
	}
	t := &http.Transport{Proxy: guardedProxy, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		c, err := d.dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &deadlineConn{c, d.now}, nil
	}, TLSClientConfig: &tls.Config{RootCAs: d.roots}, TLSHandshakeTimeout: inactivity, DisableKeepAlives: true, ForceAttemptHTTP2: false, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}
	return &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

func get(ctx context.Context, c *http.Client, raw string) (*http.Response, error) {
	u, err := endpoint(raw)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("invalid HTTP request")
	}
	resp, err := c.Do(req)
	if err != nil {
		if e, ok := err.(*url.Error); ok {
			err = e.Err
		}
		return nil, fmt.Errorf("request to %s failed: %w", publicURL(raw), err)
	}
	return resp, nil
}
