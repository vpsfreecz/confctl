package gendata

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

const warningMessage = "synthetic warning"

type warningWriter struct {
	n                       int
	err                     error
	output                  bytes.Buffer
	writes, flushes, closes int
}

func (w *warningWriter) Write(p []byte) (int, error) {
	w.writes++
	n := w.n
	if n < 0 {
		n = len(p)
	}
	w.output.Write(p[:n])
	return n, w.err
}
func (w *warningWriter) Flush() error { w.flushes++; return nil }
func (w *warningWriter) Close() error { w.closes++; return nil }

func TestWarningTo(t *testing.T) {
	cause := errors.New("synthetic writer error")
	body := warningMessage + "\n"
	for _, test := range []struct {
		name      string
		n         int
		err, want error
	}{
		{"full", -1, nil, nil}, {"zero-short", 0, nil, io.ErrShortWrite}, {"partial-short", 3, nil, io.ErrShortWrite},
		{"zero-error", 0, cause, cause}, {"partial-error", 3, cause, cause}, {"full-error", len(body), cause, cause},
	} {
		t.Run(test.name, func(t *testing.T) {
			writer := &warningWriter{n: test.n, err: test.err}
			writer.output.WriteString("prior")
			sink := WarningTo(writer)
			if writer.writes != 0 {
				t.Fatal("warning closure construction wrote")
			}
			err := sink(warningMessage)
			n := test.n
			if n < 0 {
				n = len(body)
			}
			if err != test.want || writer.writes != 1 || writer.flushes != 0 || writer.closes != 0 || writer.output.String() != "prior"+body[:n] {
				t.Fatal("warning single-write/error/residue changed")
			}
		})
	}
	if err := WarningTo(nil)(warningMessage); err == nil || err.Error() != "warning output is unavailable" {
		t.Fatal("nil warning writer skipped")
	}
}

// Constructors remain source compatible for the existing prompt adapter. The
// generated schema has no runtime description or protocol warning dispatch.
func TestVPSAdminWarningConstruction(t *testing.T) {
	do := func(*http.Request) (*http.Response, error) { t.Fatal("constructor dispatched"); return nil, nil }
	now := func() time.Time { t.Fatal("constructor clock"); return time.Time{} }
	if p, err := NewVPSAdminWithWarning(nil, nil); p != nil || err == nil {
		t.Fatal("nil sink accepted")
	}
	if p, err := NewVPSAdminWithWarningBindings(nil, nil, do, now); p != nil || err == nil {
		t.Fatal("nil sink accepted")
	}
	for _, bound := range []bool{false, true} {
		sink := func(string) error { t.Fatal("generated schema warned"); return nil }
		var p *VPSAdmin
		var err error
		if bound {
			p, err = NewVPSAdminWithWarningBindings(nil, sink, do, now)
		} else {
			p, err = NewVPSAdminWithWarning(nil, sink)
		}
		if err != nil || p == nil {
			t.Fatal(err)
		}
		if err = p.Authenticate(context.Background()); err == nil {
			t.Fatal("missing prompt accepted")
		}
	}
}
