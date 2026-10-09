package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type processIdentity struct {
	PID   int
	Start string
}

func identityState(pid int) (processIdentity, byte, error) {
	b, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if e != nil {
		return processIdentity{}, 0, e
	}
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return processIdentity{}, 0, fmt.Errorf("bad proc stat")
	}
	fields := strings.Fields(string(b[end+2:]))
	if len(fields) < 20 {
		return processIdentity{}, 0, fmt.Errorf("short proc stat")
	}
	return processIdentity{pid, fields[19]}, fields[0][0], nil
}
func identity(pid int) (processIdentity, error) {
	p, _, e := identityState(pid)
	return p, e
}
func sameIdentity(p processIdentity) bool {
	current, e := identity(p.PID)
	return e == nil && current == p
}
func liveIdentity(p processIdentity) bool {
	current, state, e := identityState(p.PID)
	return e == nil && current == p && state != 'Z' && state != 'X'
}

type ownedTree struct {
	mu      sync.Mutex
	root    int
	seen    map[int]processIdentity
	stop    chan struct{}
	stopped chan struct{}
}

func newOwnedTree() *ownedTree {
	_, _, _ = syscall.Syscall6(syscall.SYS_PRCTL, 36, 1, 0, 0, 0, 0)
	return &ownedTree{seen: map[int]processIdentity{}, stop: make(chan struct{}), stopped: make(chan struct{})}
}
func (t *ownedTree) visit(pid int) {
	p, e := identity(pid)
	if e != nil {
		return
	}
	if old, ok := t.seen[pid]; ok && old != p {
		return
	}
	t.seen[pid] = p
	tasks, _ := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	for _, task := range tasks {
		b, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "task", task.Name(), "children"))
		for _, s := range strings.Fields(string(b)) {
			child, e := strconv.Atoi(s)
			if e == nil {
				t.visit(child)
			}
		}
	}
}
func (t *ownedTree) start(pid int) {
	t.mu.Lock()
	t.root = pid
	t.visit(pid)
	t.mu.Unlock()
	go func() {
		defer close(t.stopped)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			t.mu.Lock()
			t.collect()
			t.mu.Unlock()
			select {
			case <-ticker.C:
			case <-t.stop:
				return
			}
		}
	}()
}

// Called with mu held. Continue following proven descendants after root exit;
// do not discover unrelated children adopted by this subreaper.
func (t *ownedTree) collect() {
	if p, ok := t.seen[t.root]; ok && sameIdentity(p) {
		t.visit(t.root)
		return
	}
	for _, p := range t.seen {
		if sameIdentity(p) {
			t.visit(p.PID)
		}
	}
}
func (t *ownedTree) signal(sig syscall.Signal) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.collect()
	for _, p := range t.seen {
		if sameIdentity(p) {
			_ = syscall.Kill(p.PID, sig)
		}
	}
}
func (t *ownedTree) finish(cancelled bool) error {
	close(t.stop)
	<-t.stopped
	t.mu.Lock()
	t.collect()
	leftover := false
	for _, p := range t.seen {
		if p.PID != t.root && liveIdentity(p) {
			leftover = true
		}
	}
	t.mu.Unlock()
	if cancelled {
		t.signal(syscall.SIGKILL)
	} else if leftover {
		t.signal(syscall.SIGTERM)
	}
	// Subreaper adoption can follow the parent's death; wait only for proven PIDs.
	start := time.Now()
	killed := cancelled
	for {
		pending := false
		t.mu.Lock()
		t.collect()
		for _, p := range t.seen {
			if p.PID == t.root || !sameIdentity(p) {
				continue
			}
			var status syscall.WaitStatus
			_, _ = syscall.Wait4(p.PID, &status, syscall.WNOHANG, nil)
			if sameIdentity(p) {
				pending = true
			}
		}
		t.mu.Unlock()
		if !pending {
			if leftover {
				return fmt.Errorf("test driver leftover live descendants after CLI exit")
			}
			return nil
		}
		if !killed && time.Since(start) >= 250*time.Millisecond {
			t.signal(syscall.SIGKILL)
			killed = true
		}
		if time.Since(start) >= time.Second {
			return fmt.Errorf("test driver owned descendant cleanup/reaping incomplete")
		}
		time.Sleep(time.Millisecond)
	}
}
