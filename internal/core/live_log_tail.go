package core

import (
	"fmt"
	"io"
	"os"
)

// liveLogTail owns only a read descriptor and offset. The checked writer and
// its offset, subscriptions, raw bytes and capture ownership stay untouched.
type liveLogTail struct {
	file   *os.File
	offset int64
}

func (e *Engine) openLiveLogTail() (*liveLogTail, error) {
	owner, err := e.checkedLogOwner()
	if err != nil || owner == nil {
		return nil, err
	}
	original, err := owner.file.Stat()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(owner.file.Name())
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err == nil && (!opened.Mode().IsRegular() || !os.SameFile(original, opened)) {
		err = fmt.Errorf("live log no longer names the checked log file")
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &liveLogTail{file: f, offset: opened.Size()}, nil
}

// A barrier captures one end, never chases an active writer indefinitely.
func (t *liveLogTail) end() (int64, error) {
	if t == nil {
		return 0, nil
	}
	info, err := t.file.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size() < t.offset {
		return 0, fmt.Errorf("live log was truncated")
	}
	return info.Size(), nil
}

func (t *liveLogTail) drain(end int64, feed func([]byte)) error {
	if t == nil {
		return nil
	}
	var buf [32768]byte
	for t.offset < end {
		n, err := t.file.ReadAt(buf[:min(int64(len(buf)), end-t.offset)], t.offset)
		if n > 0 {
			feed(buf[:n])
			t.offset += int64(n)
		}
		if err != nil && !(err == io.EOF && t.offset == end) {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func (t *liveLogTail) close() error {
	if t == nil {
		return nil
	}
	return t.file.Close()
}
