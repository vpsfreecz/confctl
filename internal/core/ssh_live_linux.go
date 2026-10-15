package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

// SSH's terminal-only view follows Cli::LogView and TTY::ProgressBar. Read
// newly appended command-log bytes in their existing order; other command
// families and the log writer acquire no observer or terminal behavior.
type sshLiveView struct {
	mu                 sync.Mutex
	out, progress, log *os.File
	console            *os.File
	header, title      string
	rows, cols, size   int
	current, total     int
	lastProgressWidth  int
	lines              []string
	pending            string
	err                error
	stop, done         chan struct{}
	resize             chan os.Signal
}

func sshTerminalSize(f *os.File) (int, int) {
	rows, cols := 24, 80
	fd, err := basicBorrowedFD(f)
	if err != nil {
		return rows, cols
	}
	columns, lines, err := term.GetSize(int(fd))
	runtime.KeepAlive(f)
	if err == nil {
		if lines > 0 {
			rows = lines
		}
		if columns > 0 {
			cols = columns
		}
	}
	return rows, cols
}

func sshIsTTY(f *os.File) bool {
	fd, err := basicBorrowedFD(f)
	if err != nil {
		return false
	}
	tty := term.IsTerminal(int(fd))
	runtime.KeepAlive(f)
	return tty
}

func (e *Engine) sshLive(out io.Writer, stderr *os.File, command []string, total int) (*sshLiveView, error) {
	stdout, ok := out.(*os.File)
	if !ok || os.Getenv("CONFCTL_TTY") == "0" || !sshIsTTY(stdout) || !sshIsTTY(stderr) {
		return nil, nil
	}
	v := &sshLiveView{
		out: stdout, progress: stderr,
		header: e.Colorize("Executing", "bright") + " " + sshShellJoin(command) + "\n",
		title:  e.Colorize("Live view", "bright"), total: total,
		stop: make(chan struct{}), done: make(chan struct{}), resize: make(chan os.Signal, 1),
	}
	if e.Log != nil {
		var err error
		v.log, err = os.Open(e.Log.Name())
		if err != nil {
			return nil, err
		}
		if _, err = v.log.Seek(0, io.SeekEnd); err != nil {
			_ = v.log.Close()
			return nil, err
		}
	}
	// IO.console supplies LogView's dimensions, independently of redirection
	// of standard output to another terminal. No controlling terminal uses the
	// source's 24x80 fallback.
	v.console, _ = os.OpenFile("/dev/tty", os.O_RDONLY|syscall.O_NOCTTY, 0)
	v.fetchSize()
	v.write(v.out, strings.Repeat("\n", v.rows)+"\x1b[2J\x1b[H")
	v.render(false)
	signal.Notify(v.resize, syscall.SIGWINCH)
	go v.loop()
	return v, nil
}

func (v *sshLiveView) write(out io.Writer, s string) {
	_, err := io.WriteString(out, s)
	v.err = errors.Join(v.err, err)
}

func (v *sshLiveView) fetchSize() {
	v.rows, v.cols = 24, 80
	if v.console != nil {
		v.rows, v.cols = sshTerminalSize(v.console)
	}
	v.size = max(v.rows-strings.Count(v.header, "\n")-10, 10)
}

func (v *sshLiveView) feed() bool {
	if v.log == nil {
		return false
	}
	b, err := io.ReadAll(v.log)
	if err != nil {
		v.err = errors.Join(v.err, err)
	}
	v.pending += string(b)
	changed := false
	for {
		line, rest, ok := strings.Cut(v.pending, "\n")
		if !ok {
			break
		}
		v.pending = rest
		line = strings.Trim(line, "\x00 \t\r\v\f")
		v.lines = append(v.lines, strings.ReplaceAll(line, "\t", "  "))
		if len(v.lines) > v.size {
			v.lines = v.lines[len(v.lines)-v.size:]
		}
		changed = true
	}
	return changed
}

func (v *sshLiveView) render(scoped bool) {
	var b strings.Builder
	if scoped {
		b.WriteString("\x1b7\x1b[H")
	}
	for _, line := range strings.SplitAfter(v.header, "\n") {
		if line != "" {
			b.WriteString("\x1b[2K\x1b[1G" + line)
		}
	}
	b.WriteString("\x1b[2K\x1b[1G" + v.title + " " + strings.Repeat("-", max(v.cols-len("Live view")-2, 1)) + ">\n")
	for i := 0; i < v.size; i++ {
		b.WriteString("\x1b[2K\x1b[1G")
		if i < len(v.lines) {
			line := []rune(v.lines[i])
			width := max(v.cols-4, 1)
			if len(line) >= width {
				b.WriteString(string(line[:min(width+1, len(line))]) + "...")
			} else {
				b.WriteString(string(line))
			}
		}
		b.WriteByte('\n')
	}
	b.WriteString("\x1b[2K\x1b[1G<" + strings.Repeat("-", max(v.cols-1, 1)) + "\n\n")
	if scoped {
		b.WriteString("\x1b8")
	}
	v.write(v.out, b.String())
}

func (v *sshLiveView) loop() {
	defer close(v.done)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-v.stop:
			return
		case <-ticker.C:
			v.mu.Lock()
			if v.feed() {
				v.render(true)
			}
			v.mu.Unlock()
		case <-v.resize:
			v.mu.Lock()
			v.fetchSize()
			v.feed()
			v.write(v.out, "\x1b[2J")
			v.render(true)
			v.mu.Unlock()
		}
	}
}

func (v *sshLiveView) advance() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.current++
	percent := int(float64(v.current) / float64(v.total) * 100)
	suffix := fmt.Sprintf("] %d/%d (%d%%)", v.current, v.total, percent)
	_, cols := sshTerminalSize(v.progress)
	width := min(80, max(cols-len("Command [")-len(suffix), 0))
	complete := int(math.Round(float64(width) * float64(v.current) / float64(v.total)))
	text := "Command [" + strings.Repeat("=", complete) + strings.Repeat(" ", width-complete) + suffix
	v.write(v.progress, "\x1b[1G"+text+strings.Repeat(" ", max(v.lastProgressWidth-len(text), 0)))
	v.lastProgressWidth = len(text)
	if v.current == v.total {
		v.write(v.progress, "\n")
	}
}

func (v *sshLiveView) finish(ctx context.Context, success bool) error {
	if success {
		// Cli::LogView.flush lets the last streamed lines render for a second.
		// This is presentation delay, not an execution deadline.
		timer := time.NewTimer(time.Second)
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
	}
	signal.Stop(v.resize)
	close(v.stop)
	<-v.done
	if v.log != nil {
		v.err = errors.Join(v.err, v.log.Close())
	}
	if v.console != nil {
		v.err = errors.Join(v.err, v.console.Close())
	}
	return v.err
}
