package core

import (
	"fmt"
	"io"
	"sync"
)

// liveLogRaw borrows the chunk for one existing serialized raw write followed
// by its flush. It returns the actual write count, skips flush on a failed or
// short write, and releases raw ownership before returning, including on error.
type liveLogRaw func([]byte) (int, error)

// liveLog adds synchronous subscriptions to that compound raw operation. It
// owns no file, raw lock, line buffer, worker or cancellation. Construction is
// inert; the existing Engine/Process logging callbacks are not bound here.
type liveLog struct {
	raw     liveLogRaw
	mu      sync.Mutex
	readers []*liveLogReader
}

// Membership, admission and completion are protected by the owner's mu. A
// token's done closes once, after detachment and its last admitted callback.
type liveLogReader struct {
	read     func([]byte) error
	active   bool
	inflight int
	done     chan struct{}
}

func newLiveLog(raw liveLogRaw) *liveLog { return &liveLog{raw: raw} }

// Write borrows chunk until return; neither raw nor readers may mutate it.
// Successful full raw write+flush precedes a registration-order snapshot.
// Reader errors retain the completed raw count and stop only this traversal.
// Separate Writes can notify concurrently and out of raw-write order.
func (l *liveLog) Write(chunk []byte) (int, error) {
	if l.raw == nil {
		return 0, fmt.Errorf("live log raw write is unavailable")
	}
	n, err := l.raw(chunk)
	if err != nil {
		return n, err
	}
	if n != len(chunk) {
		return n, io.ErrShortWrite
	}
	l.mu.Lock()
	readers := append([]*liveLogReader(nil), l.readers...)
	l.mu.Unlock()
	for _, reader := range readers {
		if err := l.notify(reader, chunk); err != nil {
			return n, err
		}
	}
	return n, nil
}

func (l *liveLog) notify(reader *liveLogReader, chunk []byte) error {
	l.mu.Lock()
	if !reader.active {
		l.mu.Unlock()
		return nil
	}
	reader.inflight++
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		reader.inflight--
		if !reader.active && reader.inflight == 0 {
			close(reader.done)
		}
		l.mu.Unlock()
	}()
	return reader.read(chunk) // no raw or membership ownership spans the callback
}

// Attach starts at the next snapshot. Detach removes only this token, prevents
// admission from older snapshots and joins all its admitted callbacks. Repeated
// or concurrent detach calls join the same completion and return nil. A reader
// must not detach itself or call its own view.Stop: that would wait for itself.
// Actual view readers only feed input; teardown runs outside callbacks/console
// scopes. Readers retaining borrowed bytes must copy them.
func (l *liveLog) Attach(read func([]byte) error) (func() error, error) {
	if read == nil {
		return nil, fmt.Errorf("live log reader is unavailable")
	}
	reader := &liveLogReader{read: read, active: true, done: make(chan struct{})}
	l.mu.Lock()
	l.readers = append(l.readers, reader)
	l.mu.Unlock()
	return func() error {
		l.mu.Lock()
		if reader.active {
			reader.active = false
			for i, token := range l.readers {
				if token == reader {
					copy(l.readers[i:], l.readers[i+1:])
					l.readers[len(l.readers)-1] = nil
					l.readers = l.readers[:len(l.readers)-1]
					break
				}
			}
			if reader.inflight == 0 {
				close(reader.done)
			}
		}
		l.mu.Unlock()
		<-reader.done
		return nil
	}, nil
}
