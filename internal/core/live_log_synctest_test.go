//go:build go1.25

package core

import (
	"bytes"
	"testing"
	"testing/synctest"
)

// The original ten live-log tests remain available with the module's Go 1.23
// minimum. This additional deterministic regression uses Go 1.25's synctest;
// the declared Go 1.26 verification route includes it without changing production.
func TestLiveLogDetachDeterministicQuiescence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLiveLog(func(p []byte) (int, error) { return len(p), nil })
		entered := make(chan byte, 2)
		releases := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
		detach := liveLogAttach(t, l, func(p []byte) error {
			if len(p) != 1 || p[0] > 1 {
				t.Error("unexpected callback after detach", p)
				return nil
			}
			entered <- p[0]
			<-releases[p[0]]
			return nil
		})
		token := l.readers[0]
		type writeResult struct {
			index     byte
			n         int
			err       error
			unchanged bool
		}
		writes := make(chan writeResult, 2)
		for i := byte(0); i < 2; i++ {
			go func() {
				chunk := []byte{i}
				n, err := l.Write(chunk)
				writes <- writeResult{i, n, err, bytes.Equal(chunk, []byte{i})}
			}()
		}
		first, second := <-entered, <-entered
		if first == second {
			t.Error("expected two independently blocked callbacks", first, second)
		}
		detached := make(chan error, 2)
		for i := 0; i < 2; i++ {
			go func() { detached <- detach() }()
		}
		waiting := func(wantInflight int) {
			t.Helper()
			// Wait establishes that BOTH real detachers have reached durable
			// blocking (or returned), not merely announced an intent to detach.
			synctest.Wait()
			l.mu.Lock()
			active, inflight, members := token.active, token.inflight, len(l.readers)
			l.mu.Unlock()
			if active || inflight != wantInflight || members != 0 {
				t.Error("detach admission state", active, inflight, members)
			}
			if len(detached) != 0 {
				t.Error("detacher completed with admitted callbacks", len(detached))
			}
			select {
			case <-token.done:
				t.Error("completion closed with admitted callbacks")
			default:
			}
		}
		waiting(2)
		if len(writes) != 0 {
			t.Error("write completed before its reader release")
		}
		close(releases[0])
		waiting(1) // one returned callback cannot release either detacher
		if len(writes) != 1 {
			t.Error("first release must complete exactly one write", len(writes))
		}
		close(releases[1])
		synctest.Wait()
		seen := [2]bool{}
		for i := 0; i < 2; i++ {
			result := <-writes
			if result.n != 1 || result.err != nil || !result.unchanged || seen[result.index] {
				t.Error("write result", result)
			}
			seen[result.index] = true
			if err := <-detached; err != nil {
				t.Error("detach result", err)
			}
		}
		<-token.done // both detachers observed this same completion
		if token.active || token.inflight != 0 || len(l.readers) != 0 {
			t.Error("detached state retained", token.active, token.inflight, len(l.readers))
		}
		if err := detach(); err != nil {
			t.Error("repeated detach", err)
		}
		liveLogWrite(t, l, []byte("after"), nil)
		if len(entered) != 0 {
			t.Error("reader called after quiescence")
		}
	})
}
