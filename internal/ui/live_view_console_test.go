package ui

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"
)

type liveWriteRecorder struct {
	mu              sync.Mutex
	writes          []string
	data            bytes.Buffer
	failAt, shortAt int
	err             error
}

func (w *liveWriteRecorder) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, string(data))
	if len(w.writes) == w.failAt || len(w.writes) == w.shortAt {
		n := len(data) / 2
		w.data.Write(data[:n])
		if len(w.writes) == w.failAt {
			return n, w.err
		}
		return n, nil
	}
	return w.data.Write(data)
}
func (w *liveWriteRecorder) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.data.String()
}
func (w *liveWriteRecorder) trace() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.writes...)
}
func (*liveWriteRecorder) Close() error { panic("borrowed writer must never be closed") }

func TestLiveConsoleNestedScopePutsAndErrorUnlock(t *testing.T) {
	var console LiveConsole
	w := &liveWriteRecorder{}
	sentinel := errors.New("nested")
	var borrowed *LiveConsoleScope
	err := console.Sync(func(scope *LiveConsoleScope) error {
		borrowed = scope
		return scope.Sync(func(nested *LiveConsoleScope) error {
			if nested != scope {
				t.Fatal("nested scope replaced")
			}
			for _, text := range []string{"", "a", "b\n", "c\r"} {
				if err := nested.Puts(w, text); err != nil {
					return err
				}
			}
			return sentinel
		})
	})
	if err != sentinel || w.text() != "\na\nb\nc\r\n" || !reflect.DeepEqual(w.trace(), []string{"\n", "a\n", "b\n", "c\r\n"}) {
		t.Fatal(err, w.trace())
	}
	if err := borrowed.Print(w, "escaped"); err == nil {
		t.Fatal("expired scope accepted")
	}
	if err := console.Sync(func(s *LiveConsoleScope) error { return s.Print(w, "after") }); err != nil {
		t.Fatal(err)
	}
	if w.text() != "\na\nb\nc\r\nafter" {
		t.Fatal(w.text())
	}
	for _, test := range []struct {
		short bool
		want  error
	}{{true, io.ErrShortWrite}, {false, sentinel}} {
		writer := &liveWriteRecorder{err: sentinel}
		if test.short {
			writer.shortAt = 1
		} else {
			writer.failAt = 1
		}
		if err := console.Sync(func(s *LiveConsoleScope) error { return s.Print(writer, "abcd") }); err != test.want || writer.text() != "ab" || len(writer.trace()) != 1 {
			t.Fatal(err, writer.trace())
		}
	}
}

func TestLiveConsoleExcludesCompleteViewBarAndMessageOperations(t *testing.T) {
	var console LiveConsole
	var out bytes.Buffer
	entered, release := make(chan struct{}), make(chan struct{})
	viewDone := make(chan error, 1)
	go func() {
		viewDone <- console.Sync(func(scope *LiveConsoleScope) error {
			if err := scope.Print(&out, "view-start|"); err != nil {
				return err
			}
			close(entered)
			<-release
			return scope.Sync(func(scope *LiveConsoleScope) error { return scope.Print(&out, "view-end|") })
		})
	}()
	<-entered
	attempted, otherDone := make(chan struct{}), make(chan error, 1)
	go func() {
		close(attempted)
		otherDone <- console.Sync(func(scope *LiveConsoleScope) error {
			if err := scope.Print(&out, "bar|"); err != nil {
				return err
			}
			return scope.Sync(func(scope *LiveConsoleScope) error { return scope.Puts(&out, "message") })
		})
	}()
	<-attempted
	select {
	case err := <-otherDone:
		t.Fatal("operation entered held console", err)
	default:
	}
	close(release)
	if err := <-viewDone; err != nil {
		t.Fatal(err)
	}
	if err := <-otherDone; err != nil {
		t.Fatal(err)
	}
	if out.String() != "view-start|view-end|bar|message\n" {
		t.Fatal(out.String())
	}
}
