package core

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Explicit cancellation policy only: TERM, two seconds grace, KILL, then Wait.
// Identities are captured from the owned child, never a scan of unrelated PIDs.
type childIdentity struct {
	pid   int
	start string
	group int
}

func childID(pid int) (childIdentity, error) {
	b, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if e != nil {
		return childIdentity{}, e
	}
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return childIdentity{}, fmt.Errorf("invalid child stat")
	}
	f := strings.Fields(string(b[end+2:]))
	if len(f) < 20 {
		return childIdentity{}, fmt.Errorf("short child stat")
	}
	group, e := strconv.Atoi(f[2])
	return childIdentity{pid, f[19], group}, e
}
func childSame(p childIdentity) bool { now, e := childID(p.pid); return e == nil && now == p }

type childGroup struct {
	mu    sync.Mutex
	ready chan struct{}
	done  chan struct{}
	once  sync.Once
	ids   []childIdentity
	pgid  int
}

func prepareChild(cmd *exec.Cmd) *childGroup {
	g := &childGroup{ready: make(chan struct{}), done: make(chan struct{})}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = g.cancel
	return g
}
func (g *childGroup) bind(pid int) {
	g.mu.Lock()
	g.pgid = pid
	if p, e := childID(pid); e == nil {
		g.ids = append(g.ids, p)
	}
	g.mu.Unlock()
	close(g.ready)
}
func (g *childGroup) collect(pid int) {
	tasks, _ := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	for _, task := range tasks {
		b, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "task", task.Name(), "children"))
		for _, s := range strings.Fields(string(b)) {
			pid, e := strconv.Atoi(s)
			if e != nil {
				continue
			}
			p, e := childID(pid)
			if e != nil {
				continue
			}
			g.ids = append(g.ids, p)
			g.collect(pid)
		}
	}
}
func (g *childGroup) signal(sig syscall.Signal) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.ids {
		if p.group == g.pgid && childSame(p) {
			return syscall.Kill(-g.pgid, sig)
		}
	}
	return os.ErrProcessDone
}
func (g *childGroup) cancel() error {
	<-g.ready
	g.once.Do(func() {
		g.mu.Lock()
		if len(g.ids) > 0 && childSame(g.ids[0]) {
			g.collect(g.pgid)
		}
		g.mu.Unlock()
		_ = g.signal(syscall.SIGTERM)
		go func() {
			timer := time.NewTimer(2 * time.Second)
			defer timer.Stop()
			select {
			case <-timer.C:
				_ = g.signal(syscall.SIGKILL)
			case <-g.done:
			}
		}()
	})
	return nil
}
func (g *childGroup) finish() { close(g.done) }
