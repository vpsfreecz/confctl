//go:build linux

package core

import (
	"os"
	"testing"
)

func TestLiveTerminalLinuxFactoryIsInert(t *testing.T) {
	// Memory identities only. No native closure is called: this is not an
	// ioctl, clock, signal, descriptor, CLOEXEC or controlling-TTY certificate.
	in, out, errout := new(os.File), new(os.File), new(os.File)
	terminal := newLinuxLiveTerminal(in, out, errout)
	if terminal.stdin != in || terminal.stdout != out || terminal.stderr != errout || terminal.device != nil || terminal.closed || terminal.activeResize {
		t.Fatal("factory changed original resources or acquired state")
	}
	ops := terminal.ops
	if ops.OpenConsole == nil || ops.CloseConsole == nil || ops.Dimensions == nil || ops.IoctlSize == nil || ops.IsTTY == nil || ops.Env == nil || ops.NotifyResize == nil || ops.Now == nil || ops.Wait == nil {
		t.Fatal("native factory lacks a declared lazy operation")
	}
	_ = terminal.ProgressOps(nil, nil)
	_ = terminal.NewResize()
	if terminal.Console() != terminal.Console() || terminal.device != nil || terminal.activeResize {
		t.Fatal("callback construction was not inert")
	}
}
