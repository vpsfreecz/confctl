package core

import (
	"context"
	"io"
	"math/big"
	"os"
	"runtime"

	"github.com/vpsfreecz/confctl/internal/ui"
	"golang.org/x/term"
)

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

// Parallel SSH is a reached consumer of the common view. Interactive SSH
// continues to borrow the caller's descriptors through its inherited runner.
func (e *Engine) sshLive(ctx context.Context, out, stderr io.Writer, command []string, machines []Machine, onError func(error)) (liveView, error) {
	header := e.liveHeader("SSH", sshShellJoin(command))
	rows := []ui.LiveRow{{ID: "ssh", Label: "Commands", Current: big.NewInt(0), Total: big.NewInt(int64(len(machines))), State: ui.LiveRunning}}
	for _, m := range machines {
		rows = append(rows, ui.LiveRow{ID: "host:" + m.Name, Label: m.Name, State: ui.LiveRunning, Target: true})
	}
	return e.openLiveView(ctx, out, stderr, liveViewSpec{Header: header, Rows: rows, RawLog: true}, onError)
}
