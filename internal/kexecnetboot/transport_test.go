package kexecnetboot

import (
	"compress/gzip"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLocalHTTPFreshConnectionsGzipAndRedirectRefusal(t *testing.T) {
	var connections, requests atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Proto != "HTTP/1.1" {
			t.Errorf("protocol %s", r.Proto)
		}
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/followed", 302)
		case "/gzip":
			w.Header().Set("Content-Encoding", "gzip")
			z := gzip.NewWriter(w)
			_, _ = z.Write([]byte("decoded\x00payload"))
			_ = z.Close()
		default:
			_, _ = io.WriteString(w, "literal\x00payload")
		}
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	c, err := newClient(transportDeps{roots: x509.NewCertPool()})
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout != 0 {
		t.Fatal("total timeout added")
	}
	tr := c.Transport.(*http.Transport)
	if tr.TLSHandshakeTimeout != inactivity || !tr.DisableKeepAlives || tr.ForceAttemptHTTP2 {
		t.Fatal("transport contract")
	}
	for _, path := range []string{"/literal", "/gzip", "/redirect"} {
		r, err := get(context.Background(), c, srv.URL+path)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if path == "/gzip" && string(b) != "decoded\x00payload" {
			t.Fatal(string(b))
		}
		if path == "/redirect" && r.StatusCode != 302 {
			t.Fatal(r.StatusCode)
		}
	}
	if requests.Load() != 3 || connections.Load() != 3 {
		t.Fatal("retry/redirect/pool used", requests.Load(), connections.Load())
	}
}

func TestLocalTLSVerificationAndHostname(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "verified") }))
	defer srv.Close()
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	c, err := newClient(transportDeps{roots: roots})
	if err != nil {
		t.Fatal(err)
	}
	r, err := get(context.Background(), c, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if string(b) != "verified" {
		t.Fatal(string(b))
	}
	bad, err := newClient(transportDeps{roots: x509.NewCertPool()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := get(context.Background(), bad, srv.URL); err == nil {
		t.Fatal("untrusted certificate accepted")
	}
	u, _ := url.Parse(srv.URL)
	dialer := &net.Dialer{}
	wrong, err := newClient(transportDeps{roots: roots, dial: func(ctx context.Context, n, a string) (net.Conn, error) { return dialer.DialContext(ctx, n, u.Host) }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := get(context.Background(), wrong, "https://wrong.invalid/"); err == nil {
		t.Fatal("hostname not checked")
	}
}

type recordedConn struct {
	reads, writes []time.Time
	err           error
}

func (c *recordedConn) Read(p []byte) (int, error)        { p[0] = 'x'; return 1, nil }
func (c *recordedConn) Write(p []byte) (int, error)       { return len(p), nil }
func (c *recordedConn) Close() error                      { return nil }
func (c *recordedConn) LocalAddr() net.Addr               { return &net.TCPAddr{} }
func (c *recordedConn) RemoteAddr() net.Addr              { return &net.TCPAddr{} }
func (c *recordedConn) SetDeadline(time.Time) error       { return nil }
func (c *recordedConn) SetReadDeadline(t time.Time) error { c.reads = append(c.reads, t); return c.err }
func (c *recordedConn) SetWriteDeadline(t time.Time) error {
	c.writes = append(c.writes, t)
	return c.err
}
func TestRefreshedInactivityDeadlinesNoTotalCap(t *testing.T) {
	now := time.Unix(0, 0)
	raw := &recordedConn{}
	c := &deadlineConn{raw, func() time.Time { return now }}
	buf := make([]byte, 1)
	if _, err := c.Read(buf); err != nil {
		t.Fatal(err)
	}
	now = now.Add(65 * time.Second)
	if _, err := c.Read(buf); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(buf); err != nil {
		t.Fatal(err)
	}
	if len(raw.reads) != 2 || raw.reads[0] != time.Unix(60, 0) || raw.reads[1] != time.Unix(125, 0) || raw.writes[0] != time.Unix(125, 0) {
		t.Fatal(raw.reads, raw.writes)
	}
}

func TestNoReconnectAfterPartialBodyAndCancellation(t *testing.T) {
	var requests atomic.Int32
	ready := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/partial" {
			h := w.(http.Hijacker)
			conn, rw, err := h.Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\npart")
			_ = rw.Flush()
			_ = conn.Close()
			return
		}
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(ready)
		<-r.Context().Done()
	}))
	defer srv.Close()
	c, err := newClient(transportDeps{roots: x509.NewCertPool()})
	if err != nil {
		t.Fatal(err)
	}
	r, err := get(context.Background(), c, srv.URL+"/partial")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if string(b) != "part" || err != io.ErrUnexpectedEOF || requests.Load() != 1 {
		t.Fatal(string(b), err, requests.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	r, err = get(ctx, c, srv.URL+"/cancel")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	<-ready
	cancel()
	_, err = io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err == nil {
		t.Fatal("canceled body did not fail")
	}
}

// ProxyFromEnvironment caches its snapshot. Each environment case gets a fresh
// owned test child; the child performs only a symbolic proxy choice, no dialing.
func TestProxyEnvironmentChild(t *testing.T) {
	if os.Getenv("CONFCTL_KEXEC_PROXY_CHILD") != "yes" {
		return
	}
	req, err := http.NewRequest("GET", os.Getenv("CONFCTL_KEXEC_TARGET"), nil)
	if err != nil {
		os.Exit(30)
	}
	u, err := guardedProxy(req)
	result := map[string]string{}
	if err != nil {
		result["error"] = err.Error()
	} else if u != nil {
		result["proxy"] = u.String()
	}
	b, _ := json.Marshal(result)
	_, _ = os.Stdout.Write(b)
	os.Exit(0)
}
func proxyChild(t *testing.T, target string, vars map[string]string) map[string]string {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test.run=^TestProxyEnvironmentChild$")
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy", "REQUEST_METHOD":
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, "CONFCTL_KEXEC_PROXY_CHILD=yes", "CONFCTL_KEXEC_TARGET="+target)
	for name, value := range vars {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	b, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("child JSON %q: %v", b, err)
	}
	return out
}
func TestStandardProxyEnvironmentSelection(t *testing.T) {
	for _, tc := range []struct {
		target  string
		env     map[string]string
		want    string
		failure bool
	}{{"http://origin.test/x", map[string]string{"HTTP_PROXY": "http://upper.test:80", "http_proxy": "http://lower.test:80"}, "http://upper.test:80", false}, {"https://origin.test/x", map[string]string{"HTTPS_PROXY": "https://secure.test:443", "HTTP_PROXY": "http://other.test"}, "https://secure.test:443", false}, {"http://origin.test/x", map[string]string{"HTTP_PROXY": "http://proxy.test", "NO_PROXY": "origin.test"}, "", false}, {"http://origin.test/x", map[string]string{"HTTP_PROXY": "http://proxy.test", "no_proxy": "origin.test"}, "", false}, {"http://127.0.0.1/x", map[string]string{"HTTP_PROXY": "http://proxy.test"}, "", false}, {"http://origin.test/x", map[string]string{"HTTP_PROXY": "http://proxy.test", "REQUEST_METHOD": "GET"}, "", true}, {"http://origin.test/x", map[string]string{"HTTP_PROXY": "socks5://proxy.test"}, "", true}, {"http://origin.test/x", map[string]string{"HTTP_PROXY": "http://user:password@host:bad"}, "", true}} {
		out := proxyChild(t, tc.target, tc.env)
		if tc.failure {
			if out["error"] == "" || strings.Contains(out["error"], "password") {
				t.Fatal(out)
			}
		} else if out["error"] != "" || out["proxy"] != tc.want {
			t.Fatal(tc, out)
		}
	}
}

func TestUnsupportedEndpointsAndCredentialsInErrors(t *testing.T) {
	for _, raw := range []string{"file:///fixture/kernel", "ftp://host/kernel", "/relative", "http://"} {
		if _, err := endpoint(raw); err == nil {
			t.Fatal(raw)
		}
	}
	c, err := newClient(transportDeps{roots: x509.NewCertPool(), dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, fmt.Errorf("synthetic dial failure")
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = get(context.Background(), c, "http://fixture-user:fixture-password@origin.test/x")
	if err == nil || strings.Contains(err.Error(), "fixture-user") || strings.Contains(err.Error(), "fixture-password") {
		t.Fatal(err)
	}
}

func TestProxyNetworkChild(t *testing.T) {
	if os.Getenv("CONFCTL_KEXEC_NETWORK_CHILD") != "yes" {
		return
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(os.Getenv("CONFCTL_KEXEC_CA")))
	allowed := os.Getenv("CONFCTL_KEXEC_DIAL")
	dialer := &net.Dialer{}
	c, err := newClient(transportDeps{roots: roots, dial: func(ctx context.Context, n, a string) (net.Conn, error) {
		if a != allowed {
			return nil, fmt.Errorf("unselected address")
		}
		return dialer.DialContext(ctx, n, a)
	}})
	if err != nil {
		os.Exit(30)
	}
	r, err := get(context.Background(), c, os.Getenv("CONFCTL_KEXEC_TARGET"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(31)
	}
	b, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		os.Exit(32)
	}
	_, _ = os.Stdout.Write(b)
	os.Exit(0)
}
func TestLoopbackHTTPProxyAndTLSConnectAuthentication(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "through CONNECT") }))
	defer origin.Close()
	originURL, _ := url.Parse(origin.URL)
	cert := origin.Certificate()
	if len(cert.DNSNames) == 0 {
		t.Fatal("fixture certificate needs symbolic SAN")
	}
	targetHost := cert.DNSNames[0]
	if strings.HasPrefix(targetHost, "*.") {
		targetHost = "fixture" + strings.TrimPrefix(targetHost, "*")
	}
	if err := cert.VerifyHostname(targetHost); err != nil {
		t.Fatal(err)
	}
	var direct, connect atomic.Int32
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("fixture-user:fixture-password"))
	proxyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != auth {
			http.Error(w, "fixture auth", 407)
			return
		}
		if r.Method != "CONNECT" {
			direct.Add(1)
			if r.URL.Host != "origin.test" {
				t.Errorf("proxy got host %s", r.URL.Host)
			}
			_, _ = io.WriteString(w, "through HTTP proxy")
			return
		}
		connect.Add(1)
		upstream, err := net.Dial("tcp", originURL.Host)
		if err != nil {
			t.Error(err)
			return
		}
		defer upstream.Close()
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		done := make(chan struct{})
		go func() { _, _ = io.Copy(conn, upstream); close(done) }()
		_, _ = io.Copy(upstream, rw)
		_ = upstream.Close()
		<-done
	})
	plainProxy := httptest.NewServer(proxyHandler)
	defer plainProxy.Close()
	tlsProxy := httptest.NewTLSServer(proxyHandler)
	defer tlsProxy.Close()
	for _, proxy := range []*httptest.Server{plainProxy, tlsProxy} {
		proxyURL, _ := url.Parse(proxy.URL)
		proxyURL.User = url.UserPassword("fixture-user", "fixture-password")
		ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
		if proxy == tlsProxy {
			ca = append(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsProxy.Certificate().Raw})...)
		}
		for _, tc := range []struct{ target, key, want string }{{"http://origin.test/fixture", "HTTP_PROXY", "through HTTP proxy"}, {"https://" + targetHost + "/fixture", "HTTPS_PROXY", "through CONNECT"}} {
			bin, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bin, "-test.run=^TestProxyNetworkChild$")
			for _, entry := range os.Environ() {
				name, _, _ := strings.Cut(entry, "=")
				switch name {
				case "HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy", "REQUEST_METHOD":
					continue
				}
				cmd.Env = append(cmd.Env, entry)
			}
			cmd.Env = append(cmd.Env, "CONFCTL_KEXEC_NETWORK_CHILD=yes", "CONFCTL_KEXEC_TARGET="+tc.target, "CONFCTL_KEXEC_DIAL="+proxyURL.Host, "CONFCTL_KEXEC_CA="+string(ca), tc.key+"="+proxyURL.String())
			b, err := cmd.CombinedOutput()
			if err != nil || string(b) != tc.want {
				t.Fatalf("proxy fixture output%q err%v", b, err)
			}
		}
	}
	if direct.Load() != 2 || connect.Load() != 2 {
		t.Fatal("proxy request count", direct.Load(), connect.Load())
	}
}
