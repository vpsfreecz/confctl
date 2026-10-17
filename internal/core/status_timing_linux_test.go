package core

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/cli"
)

// Reuse the ordinary query executable fixture with two owned FIFO barriers.
// Clock changes occur while its actual child is held, never from log text or
// callback counts. The caller's done closes only after the query owner returns.
func statusTimingBarriers(t *testing.T, e *Engine, root string, run func()) (func(int), func(int)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(e.Context, 10*time.Second)
	t.Cleanup(cancel)
	e.Context = ctx
	files := make([]*os.File, 0, 2)
	for _, name := range []string{"ready", "release"} {
		path := filepath.Join(root, name)
		if err := syscall.Mkfifo(path, 0600); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
		t.Cleanup(func() { _ = f.Close() })
	}
	t.Setenv("STATUS_TIMING_ROOT", root)
	done := make(chan struct{})
	go func() { defer close(done); run() }()
	t.Cleanup(func() { cancel(); <-done })
	wait := func(n int) {
		t.Helper()
		ready := make(chan error, 1)
		go func() { _, err := io.ReadFull(files[0], make([]byte, n)); ready <- err }()
		select {
		case err := <-ready:
			if err != nil {
				t.Fatal("query child barrier", err)
			}
		case <-done:
			_ = files[0].Close()
			<-ready
			t.Fatal("query returned before child barrier")
		case <-ctx.Done():
			_ = files[0].Close()
			<-ready
			t.Fatal("query child did not reach barrier", ctx.Err())
		}
	}
	release := func(n int) {
		t.Helper()
		if _, err := io.WriteString(files[1], strings.Repeat("\n", n)); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("query did not join released children", ctx.Err())
		}
	}
	return wait, release
}

func TestStatusTimingBatchIncludesAllWorkerJoins(t *testing.T) {
	e, machine, root := queryFixture(t, `printf R > "$STATUS_TIMING_ROOT/ready"
IFS= read -r _ < "$STATUS_TIMING_ROOT/release"
printf '600 0\n'
`)
	clock := invocationAdapterTiming(e)
	second := machine
	second.Name = "second"
	statuses := []Status{{Machine: machine}, {Machine: second}}
	wait, release := statusTimingBarriers(t, e, root, func() { e.statusQuery(statuses, []Machine{machine, second}, queryOptions{}, 2) })
	wait(2)
	clock.advance(3 * time.Second)
	if s := e.timing.Snapshot(); !s.Seen[timingStatusChecks] || s.Phases[timingStatusChecks] != 3*time.Second {
		t.Fatal("held queries are outside batch interval", s)
	}
	release(2)
	for _, st := range statuses {
		if st.Err != nil || st.Uptime == nil || *st.Uptime != 600 {
			t.Fatal("joined status result changed", st)
		}
	}
	clock.advance(5 * time.Second)
	if s := e.timing.Snapshot(); s.Phases[timingStatusChecks] != 3*time.Second || s.Work != 3*time.Second || s.Other != 5*time.Second {
		t.Fatal("status interval escaped batch return or summed workers", s)
	}
}

func TestStatusTimingExplicitGenerationUsesQueryBatch(t *testing.T) {
	e, machine, root := queryFixture(t, `case "$remote" in
 'cat /proc/uptime')
   printf R > "$STATUS_TIMING_ROOT/ready"
   IFS= read -r _ < "$STATUS_TIMING_ROOT/release"
   printf '600 0\n' ;;
 'realpath /run/current-system') printf '/target\n' ;;
 'bash --norc') printf '' ;;
 'cat /etc/confctl/inputs-info.json') printf '{}' ;;
 *) exit 99 ;;
esac
`)
	seedCoreGeneration(t, root, machine.Name, "held", coreGenerationRecord("/target", "2020-01-01T00:00:00Z", `{}`))
	clock := invocationAdapterTiming(e)
	inv, err := cli.BuiltinRegistry().Parse([]string{"status", "--yes", "--generation", "held", machine.Name})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	var statusErr error
	wait, release := statusTimingBarriers(t, e, root, func() { statusErr = e.statusCLI(inv, strings.NewReader(""), &out, io.Discard, buildDeps{}) })
	wait(1)
	clock.advance(3 * time.Second)
	if s := e.timing.Snapshot(); s.Phases[timingStatusChecks] != 3*time.Second {
		t.Fatal("explicit generation did not time its held child", s)
	}
	release(1)
	if statusErr != nil {
		t.Fatal(statusErr)
	}
	clock.advance(5 * time.Second)
	s := e.timing.Snapshot()
	if s.Phases[timingStatusChecks] != 3*time.Second || s.Seen[timingBuild] || s.Seen[timingEvaluation] || s.Prompt != 0 {
		t.Fatal("explicit generation skipped query timing or added build", s)
	}
	if got := fixtureFile(t, root, "trace"); got != "cat /proc/uptime\nrealpath /run/current-system\nbash --norc\ncat /etc/confctl/inputs-info.json\n" || !strings.Contains(out.String(), "Resolved host generations:") || !strings.Contains(out.String(), machine.Name) {
		t.Fatal("explicit status query/output changed", got, out.String())
	}
}

func TestStatusTimingInitialConfirmationAndEmptyBatch(t *testing.T) {
	e := connectionEngine(t, connectionMachine("one", "one", "localhost", true))
	clock := invocationAdapterTiming(e)
	inv, err := cli.BuiltinRegistry().Parse([]string{"status", "--generation", "none", "one"})
	if err != nil {
		t.Fatal(err)
	}
	reader := &invocationAdapterReader{clock: clock, steps: []connectionRead{{data: "invalid\n"}, {data: "n\n"}}}
	if err := e.statusCLI(inv, reader, &invocationAdapterWriter{clock: clock}, io.Discard, buildDeps{}); err == nil || err.Error() != "Aborted" {
		t.Fatal(err)
	}
	e.statusQuery(nil, nil, queryOptions{}, 2)
	clock.advance(5 * time.Second)
	if s := e.timing.Snapshot(); s.Prompt != 4*time.Second || s.Seen[timingStatusChecks] || s.Work != 0 {
		t.Fatal("refusal/empty batch invented status work", s)
	}
}
