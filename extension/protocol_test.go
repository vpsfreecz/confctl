package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrentRequestsAndStreamSeparation(t *testing.T) {
	ai, bo := io.Pipe()
	bi, ao := io.Pipe()
	defer ai.Close()
	defer bo.Close()
	defer bi.Close()
	defer ao.Close()
	a := NewPeer(context.Background(), ai, ao, "c")
	b := NewPeer(context.Background(), bi, bo, "e")
	a.SetHandler(func(_ context.Context, m string, p json.RawMessage) (any, error) {
		return map[string]string{"value": m}, nil
	})
	b.SetHandler(func(_ context.Context, m string, p json.RawMessage) (any, error) {
		return map[string]string{"value": m}, nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			name := fmt.Sprint(i)
			var r map[string]string
			if e := a.Call(ctx, name, map[string]any{}, &r); e != nil || r["value"] != name {
				t.Errorf("%d %v %v", i, r, e)
			}
		}(i)
	}
	wg.Wait()
	if a.Outstanding() != 0 {
		t.Fatal(a.Outstanding())
	}
}
func TestInvalidFramingFails(t *testing.T) {
	for _, s := range []string{"[{}]\n", "junk\n", "{\"jsonrpc\":\"1.0\"}\n", "{\"jsonrpc\":\"2.0\",\"id\":\"unexpected\",\"result\":{}}\n"} {
		p := NewPeer(context.Background(), strings.NewReader(s), io.Discard, "c")
		p.SetHandler(func(context.Context, string, json.RawMessage) (any, error) { return nil, nil })
		select {
		case <-p.done:
			if p.Err() == nil {
				t.Fatal("missing failure")
			}
		case <-time.After(time.Second):
			t.Fatal("framing hung")
		}
	}
}
func TestCallCancellation(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	p := NewPeer(context.Background(), r, io.Discard, "c")
	p.SetHandler(func(context.Context, string, json.RawMessage) (any, error) { return nil, nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out any
	if e := p.Call(ctx, "never", nil, &out); e != context.Canceled {
		t.Fatal(e)
	}
}
func TestAttrMissingNullFalse(t *testing.T) {
	m := Machine{Attributes: map[string]any{"custom": map[string]any{"false": false, "null": nil}}}
	if m.Attr("custom.false") != false || m.Attr("custom.missing") != nil || m.Attr("custom.null") != nil {
		t.Fatal(m)
	}
}

func TestPeerPreservesMetadataNumbers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	defer ar.Close()
	defer aw.Close()
	defer br.Close()
	defer bw.Close()
	a := NewPeer(ctx, ar, aw, "a")
	b := NewPeer(ctx, br, bw, "b")
	a.SetHandler(nil)
	b.SetHandler(func(context.Context, string, json.RawMessage) (any, error) {
		return map[string]any{"large": json.Number("9007199254740993"), "decimal": json.Number("1.0")}, nil
	})
	var result map[string]any
	if e := a.Call(ctx, "metadata", nil, &result); e != nil {
		t.Fatal(e)
	}
	if result["large"] != json.Number("9007199254740993") || result["decimal"] != json.Number("1.0") {
		t.Fatal(result)
	}
}

func TestRunDecodePreservesNumbersAndSingleValue(t *testing.T) {
	var run Run
	if err := decodeRun([]byte(`{"handler":"x","context":{"options":{"huge":92233720368547758081234567890,"null":null}}}`), &run); err != nil {
		t.Fatal(err)
	}
	if n, ok := run.Context.Options["huge"].(json.Number); !ok || n.String() != "92233720368547758081234567890" {
		t.Fatal(run)
	}
	if _, ok := run.Context.Options["null"]; !ok {
		t.Fatal("null lost")
	}
	for _, b := range []string{`{} {}`, `{} trailing`} {
		if err := decodeRun([]byte(b), &run); err == nil {
			t.Fatal("trailing Run accepted", b)
		}
	}
}
