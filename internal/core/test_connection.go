package core

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/vpsfreecz/confctl/internal/cli"
)

// Named fields prevent Buffer's ReadFrom from being promoted. os.File's generic
// io.Copy path selects this ReaderFrom; no syscall.Conn or WriterTo is exposed.
// Each positive real read is captured and logged before storage is reused.
type testConnectionCapture struct {
	buffer *bytes.Buffer
	chunk  func([]byte)
}

func (c *testConnectionCapture) Write(b []byte) (int, error) {
	n, err := c.buffer.Write(b)
	if n > 0 {
		c.chunk(b[:n])
	}
	return n, err
}

func (c *testConnectionCapture) ReadFrom(r io.Reader) (int64, error) {
	b := make([]byte, 16384)
	var total int64
	for {
		n, err := r.Read(b)
		if n > 0 {
			_, _ = c.Write(b[:n])
			total += int64(n)
		}
		if err != nil {
			if err == io.EOF {
				return total, nil
			}
			return total, err
		}
	}
}

type testConnectionDeps struct {
	Run func(context.Context, string, []string, []byte) (Result, error)
}

type testConnectionArgumentError struct{ error }

func (e *Engine) TestConnection(inv cli.Invocation, in io.Reader, out io.Writer) error {
	return e.runTestConnection(inv, in, out, testConnectionDeps{Run: e.testConnectionProcess})
}

func (e *Engine) runTestConnection(inv cli.Invocation, in io.Reader, out io.Writer, deps testConnectionDeps) error {
	all, err := e.Machines(false)
	if err != nil {
		return err
	}
	opts := handlerOptions(inv)
	var pattern *string
	if len(opts.Args) > 0 {
		pattern = &opts.Args[0]
	}
	managed := opts.Managed
	if managed == "" {
		managed = "yes"
	}
	ms, err := Select(all, pattern, opts.Attrs, opts.Tags, managed)
	if err != nil {
		// Ruby's failed parse destructuring leaves the interpolated value nil.
		// Keep this observed leaf diagnostic without changing shared selection.
		for _, f := range opts.Attrs {
			if !strings.Contains(f, "=") {
				return &testConnectionArgumentError{fmt.Errorf("Invalid filter ''")}
			}
		}
		return &testConnectionArgumentError{err}
	}
	selected := []Machine{}
	for _, m := range ms {
		if m.CarrierName == nil && m.Target.Host != nil {
			selected = append(selected, m)
		}
	}
	if len(selected) == 0 {
		return fmt.Errorf("No machines to test")
	}
	if !opts.Yes {
		text, err := e.List(selected, nil, true)
		if err != nil {
			return err
		}
		fmt.Fprint(out, "Test SSH connection to the following machines:\n"+text)
		if err := testConnectionConfirm(in, out); err != nil {
			return err
		}
	}
	failed := []string{}
	for _, m := range selected {
		argv, err := testConnectionArgs(m)
		if err != nil {
			return err
		}
		r, err := deps.Run(e.Context, e.Root, argv, nil)
		status, err := testConnectionStatus(e.Context, argv, r, err)
		if err != nil {
			return err
		}
		if status == 0 {
			continue
		}
		failed = append(failed, m.Name)
		fmt.Fprintf(out, "Unable to connect to %s: Running `%s` failed with\n  exit status: %d\n  stdout: %s\n  stderr: %s\n\n", m.Name, testConnectionShellJoin(argv), status, testConnectionOutput(r.Stdout), testConnectionOutput(testConnectionStderr(r.Stderr)))
	}
	fmt.Fprintf(out, "\nResult: %d successful, %d failed\n\nFailed machines:\n", len(selected)-len(failed), len(failed))
	for _, name := range failed {
		fmt.Fprintf(out, "  %s\n", name)
	}
	return nil
}

func testConnectionConfirm(in io.Reader, out io.Writer) error {
	r := bufio.NewReader(in)
	for {
		if _, err := fmt.Fprint(out, "\nContinue? [y/N]: "); err != nil {
			return err
		}
		line, err := r.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		if err == io.EOF && line == "" {
			return fmt.Errorf("end of file reached")
		}
		switch strings.ToLower(testConnectionStrip(line)) {
		case "y":
			_, err := fmt.Fprintln(out)
			return err
		case "n":
			if _, err := fmt.Fprintln(out); err != nil {
				return err
			}
			return fmt.Errorf("Aborted")
		}
	}
}

func testConnectionArgs(m Machine) ([]string, error) {
	// Localhost is the literal host, not a carried-machine or default shell route.
	if m.Target.Host != nil && *m.Target.Host == "localhost" {
		return []string{"cat", "/proc/uptime"}, nil
	}
	a, err := SSHArgs(m, []string{"cat", "/proc/uptime"})
	if err != nil {
		return nil, err
	}
	options := []string{"-o", "ConnectTimeout=3", "-o", "ServerAliveInterval=3", "-o", "ServerAliveCountMax=1"}
	n := len(a) - 2
	return append(append(append([]string{}, a[:n]...), options...), a[n:]...), nil
}

func testConnectionStatus(ctx context.Context, argv []string, r Result, runErr error) (int, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if runErr != nil {
		var missing *exec.Error
		if len(argv) == 2 && argv[0] == "cat" && argv[1] == "/proc/uptime" && errors.As(runErr, &missing) && missing.Name == "cat" && errors.Is(missing.Err, exec.ErrNotFound) {
			return 0, fmt.Errorf("No such file or directory - cat")
		}
		return 0, runErr
	}
	if r.waitErr != nil {
		var exit *exec.ExitError
		if !errors.As(r.waitErr, &exit) {
			return 0, r.waitErr
		}
	}
	if r.signal != 0 {
		return int(r.signal), nil
	}
	if r.ExitCode < 0 {
		return 0, fmt.Errorf("command did not return an exit status")
	}
	return r.ExitCode, nil
}

func (e *Engine) testConnectionProcess(ctx context.Context, root string, argv []string, input []byte) (Result, error) {
	id := logID()
	e.logText("[" + id + "] Running " + testConnectionShellJoin(argv) + "\n")
	start := time.Now()
	r, err := processWithTestConnectionLog(ctx, root, argv, input, func(b []byte) {
		// Consume synchronously; never retain the reusable read buffer's slice.
		e.logText("[" + id + "] \t" + strings.TrimSuffix(string(b), "\n") + "\n")
	})
	status, failure := testConnectionStatus(ctx, argv, r, err)
	if failure != nil {
		return r, failure
	}
	display := r
	display.Stdout, display.Stderr = nil, nil
	display.ExitCode = status
	e.processLogFinish(id, start, display)
	return r, nil
}

func testConnectionShellJoin(argv []string) string {
	// ShellJoin already escapes backslashes, so each remaining percent is literal.
	return strings.ReplaceAll(ShellJoin(argv), "%", "\\%")
}

func testConnectionStrip(s string) string {
	return strings.Trim(s, "\x00 \t\n\v\f\r")
}

func testConnectionOutput(b []byte) string {
	s := testConnectionStrip(string(b))
	if s == "" {
		return "Nothing written"
	}
	return s
}

// Diagnostic-only ASCII bounds; the complete captured/logged bytes are retained.
func testConnectionStderr(b []byte) []byte {
	if len(b) <= 65536 {
		return b
	}
	return []byte(string(b[:32768]) + fmt.Sprintf("\n... omitting %d bytes ...\n", len(b)-65536) + string(b[len(b)-32768:]))
}
