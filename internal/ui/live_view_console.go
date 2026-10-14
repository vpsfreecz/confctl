// Package ui owns private viewport text and console synchronization. Native
// streams, logger readers, dimensions, resize delivery and bars belong to callers.
package ui

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// LiveConsole is shared by one invocation's view, bars and messages. Its zero
// value is usable. Root Sync must not be reentered; nested helpers use the scope.
type LiveConsole struct{ mu sync.Mutex }

// LiveConsoleScope is borrowed only during its Sync callback. It must not escape
// or be passed to another goroutine. Nested Sync executes inline under that lock.
type LiveConsoleScope struct{ active bool }

func (c *LiveConsole) Sync(fn func(*LiveConsoleScope) error) error {
	if c == nil {
		return fmt.Errorf("live view console is unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	scope := &LiveConsoleScope{active: true}
	defer func() { scope.active = false }()
	return scope.Sync(fn)
}

func (s *LiveConsoleScope) Sync(fn func(*LiveConsoleScope) error) error {
	if s == nil || !s.active {
		return fmt.Errorf("live view console scope is no longer borrowed")
	}
	if fn == nil {
		return fmt.Errorf("live view console callback is unavailable")
	}
	return fn(s)
}

func (s *LiveConsoleScope) Print(out io.Writer, text string) error {
	if s == nil || !s.active {
		return fmt.Errorf("live view console scope is no longer borrowed")
	}
	if out == nil {
		return fmt.Errorf("live view output is unavailable")
	}
	n, err := io.WriteString(out, text)
	if err == nil && n != len(text) {
		return io.ErrShortWrite
	}
	return err
}

func (s *LiveConsoleScope) Puts(out io.Writer, text string) error {
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return s.Print(out, text)
}
