package ui

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

type promptRead struct {
	text string
	err  error
}

type promptWrite struct {
	n   int
	err error
}

// This finite script records only operation names. Assertions intentionally do
// not print labels, answers, consumed candidates, or arbitrary callback errors.
type promptScript struct {
	trace       []string
	output      bytes.Buffer
	writes      []promptWrite
	writeCount  int
	flushCount  int
	flushFail   int
	flushErr    error
	lineRead    promptRead
	characters  []promptRead
	readCount   int
	beginErr    error
	restoreErr  error
	noRestore   bool
	noCharacter bool
	closed      int
}

func (s *promptScript) Write(p []byte) (int, error) {
	s.trace = append(s.trace, "write")
	i := s.writeCount
	s.writeCount++
	n, err := len(p), error(nil)
	if i < len(s.writes) {
		n, err = s.writes[i].n, s.writes[i].err
	}
	if n >= 0 && n <= len(p) {
		s.output.Write(p[:n])
	}
	return n, err
}

func (s *promptScript) Close() error {
	s.closed++
	return errors.New("borrowed writer must remain open")
}

func (s *promptScript) flush() error {
	s.trace = append(s.trace, "flush")
	s.flushCount++
	if s.flushCount == s.flushFail {
		return s.flushErr
	}
	return nil
}

func (s *promptScript) ops(t *testing.T) GenDataPromptIO {
	t.Helper()
	return GenDataPromptIO{
		Writer: s,
		Flush:  s.flush,
		Line: func(context.Context) (string, error) {
			s.trace = append(s.trace, "line")
			return s.lineRead.text, s.lineRead.err
		},
		BeginProtected: func(context.Context) (GenDataProtectedInput, error) {
			s.trace = append(s.trace, "begin")
			input := GenDataProtectedInput{}
			if !s.noCharacter {
				input.Character = func(context.Context) (string, error) {
					s.trace = append(s.trace, "character")
					if s.readCount >= len(s.characters) {
						t.Fatal("unexpected Character invocation")
					}
					r := s.characters[s.readCount]
					s.readCount++
					return r.text, r.err
				}
			}
			if !s.noRestore {
				input.Restore = func() error {
					s.trace = append(s.trace, "restore")
					return s.restoreErr
				}
			}
			return input, s.beginErr
		},
	}
}

func promptTrace(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("operation order differs: got %v, want %v", got, want)
	}
}

func promptAnswer(t *testing.T, got, want string, err error) {
	t.Helper()
	if err != nil || got != want {
		t.Fatal("successful raw answer differs")
	}
}

func promptFailure(t *testing.T, answer string, got, want error) {
	t.Helper()
	if answer != "" || got != want {
		t.Fatal("failure result or error identity differs")
	}
}

func promptBindingFailure(t *testing.T, answer string, err error, operation, kind string) {
	t.Helper()
	want := "authentication prompt " + operation + " binding " + kind
	if answer != "" || err == nil || err.Error() != want {
		t.Fatal("fixed binding diagnostic differs")
	}
}

func promptOutput(t *testing.T, s *promptScript, want string) {
	t.Helper()
	if s.output.String() != want {
		t.Fatal("retained output bytes differ")
	}
	if s.closed != 0 {
		t.Fatal("borrowed writer was closed")
	}
}

func TestGenDataPromptConstruction(t *testing.T) {
	s := &promptScript{lineRead: promptRead{text: "synthetic-user\n"}}
	ops := s.ops(t)
	p := NewGenDataPrompt(ops)
	if len(s.trace) != 0 {
		t.Fatal("constructor accessed bindings")
	}
	// Replacing every caller field must not change the copied invocation.
	ops.Writer, ops.Flush, ops.Line, ops.BeginProtected = nil, nil, nil, nil
	answer, err := p.Ask(context.Background(), "User name: ", false)
	promptAnswer(t, answer, "synthetic-user\n", err)
	promptTrace(t, s.trace, []string{"write", "flush", "line"})
	promptOutput(t, s, "User name: ")
	// Unused terminal and Line bindings are deliberately optional.
	t.Run("echoed-only", func(t *testing.T) {
		s := &promptScript{lineRead: promptRead{text: "\n"}}
		ops := s.ops(t)
		ops.BeginProtected = nil
		answer, err := NewGenDataPrompt(ops).Ask(context.Background(), "", false)
		promptAnswer(t, answer, "\n", err)
	})
	t.Run("protected-only", func(t *testing.T) {
		s := &promptScript{noRestore: true, characters: []promptRead{{err: io.EOF}}}
		ops := s.ops(t)
		ops.Line = nil
		answer, err := NewGenDataPrompt(ops).Ask(context.Background(), "", true)
		promptAnswer(t, answer, "", err)
		promptTrace(t, s.trace, []string{"write", "flush", "begin", "character", "write", "flush"})
	})
}

func TestGenDataPromptLabels(t *testing.T) {
	labels := []string{"User name: ", "Password: ", "Challenge: ", "<%= 1 + 1 %>: ", "% synthetic: \t", "\x1b[31m\n\x00: ", string([]byte{0xff, ':', ' '})}
	for i, label := range labels {
		for _, protected := range []bool{false, true} {
			t.Run(fmt.Sprintf("label-%d/protected-%t", i, protected), func(t *testing.T) {
				s := &promptScript{lineRead: promptRead{text: "\n"}, characters: []promptRead{{text: "\n"}}}
				answer, err := NewGenDataPrompt(s.ops(t)).Ask(context.Background(), label, protected)
				want := "\n"
				trace := []string{"write", "flush", "line"}
				output := label
				if protected {
					want = ""
					trace = []string{"write", "flush", "begin", "character", "restore", "write", "flush"}
					output += "\n"
				}
				promptAnswer(t, answer, want, err)
				promptTrace(t, s.trace, trace)
				promptOutput(t, s, output)
			})
		}
	}
}

func TestGenDataPromptLine(t *testing.T) {
	readErr := errors.New("synthetic read failure")
	wrappedEOF := fmt.Errorf("end: %w", io.EOF)
	for _, tc := range []struct {
		name    string
		read    promptRead
		wantErr error
		invalid bool
	}{
		{"lf", promptRead{text: " synthetic \t\n"}, nil, false},
		{"blank", promptRead{text: "\n"}, nil, false},
		{"crlf", promptRead{text: "synthetic\r\n"}, nil, false},
		{"bare-cr-final", promptRead{text: "synthetic\r", err: io.EOF}, nil, false},
		{"multibyte-final", promptRead{text: "příliš東京", err: wrappedEOF}, nil, false},
		{"empty-eof", promptRead{err: io.EOF}, io.EOF, false},
		{"empty-wrapped-eof", promptRead{err: wrappedEOF}, wrappedEOF, false},
		{"partial-error", promptRead{text: "synthetic", err: readErr}, readErr, false},
		{"invalid-partial-error", promptRead{text: "\xff", err: readErr}, readErr, false},
		{"empty-nil", promptRead{}, nil, true},
		{"unterminated-nil", promptRead{text: "synthetic"}, nil, true},
		{"two-lines", promptRead{text: "first\nsecond\n"}, nil, true},
		{"suffix-after-lf", promptRead{text: "first\nsuffix"}, nil, true},
		{"lf-with-eof", promptRead{text: "synthetic\n", err: io.EOF}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &promptScript{lineRead: tc.read}
			answer, err := NewGenDataPrompt(s.ops(t)).Ask(context.Background(), "Label: ", false)
			if tc.invalid {
				promptBindingFailure(t, answer, err, "Line", "returned an invalid result")
			} else if tc.wantErr != nil {
				promptFailure(t, answer, err, tc.wantErr)
			} else {
				promptAnswer(t, answer, tc.read.text, err)
			}
			promptTrace(t, s.trace, []string{"write", "flush", "line"})
			promptOutput(t, s, "Label: ")
		})
	}
}

func TestGenDataPromptEditing(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		chars      []string
	}{
		{"unicode-deletion", "é", []string{"\b", "\x7f", "a", "\b", "é", "東", "\x7f", "\n"}},
		{"combining-codepoint", "e", []string{"e", "\u0301", "\b", "\r"}},
		{"clear-and-controls", "\x00\t\x04", []string{"a", "東", "\x15", "\x00", "\t", "\x04", "\n"}},
		{"clear-empty", "", []string{"\x15", "\b", "\x7f", "\r"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &promptScript{}
			for _, c := range tc.chars {
				s.characters = append(s.characters, promptRead{text: c})
			}
			answer, err := NewGenDataPrompt(s.ops(t)).Ask(context.Background(), "Password: ", true)
			promptAnswer(t, answer, tc.want, err)
			trace := []string{"write", "flush", "begin"}
			for range tc.chars[:len(tc.chars)-1] {
				trace = append(trace, "character", "flush")
			}
			trace = append(trace, "character", "restore", "write", "flush")
			promptTrace(t, s.trace, trace)
			promptOutput(t, s, "Password: \n")
		})
	}
	t.Run("crlf-retains-lf", func(t *testing.T) {
		s := &promptScript{characters: []promptRead{{text: "a"}, {text: "\r"}, {text: "\n"}}}
		p := NewGenDataPrompt(s.ops(t))
		answer, err := p.Ask(context.Background(), "One: ", true)
		promptAnswer(t, answer, "a", err)
		if s.readCount != 2 {
			t.Fatal("CR consumed trailing LF")
		}
		answer, err = p.Ask(context.Background(), "Two: ", true)
		promptAnswer(t, answer, "", err)
		if s.readCount != 3 {
			t.Fatal("retained LF was not consumed by next question")
		}
	})
}

func TestGenDataPromptEscape(t *testing.T) {
	wrappedEOF := fmt.Errorf("end: %w", io.EOF)
	for i, pair := range [][2]promptRead{
		{{text: "["}, {text: "A"}},
		{{text: "\b"}, {text: "\x15"}},
		{{text: "\r"}, {text: "\n"}},
		{{text: "\x03"}, {text: "\x1b"}},
		{{text: "\xff"}, {text: "\xe2\x82"}},
		{{text: "two"}, {text: "\x00\x03"}},
		{{err: io.EOF}, {text: "x"}},
		{{text: "x"}, {err: io.EOF}},
		{{err: io.EOF}, {err: wrappedEOF}},
	} {
		t.Run(fmt.Sprintf("discard-%d", i), func(t *testing.T) {
			s := &promptScript{characters: []promptRead{{text: "a"}, {text: "\x1b"}, pair[0], pair[1], {text: "b"}, {text: "\n"}}}
			answer, err := NewGenDataPrompt(s.ops(t)).Ask(context.Background(), "Label: ", true)
			promptAnswer(t, answer, "ab", err)
			promptTrace(t, s.trace, []string{"write", "flush", "begin", "character", "flush", "character", "character", "character", "flush", "character", "flush", "character", "restore", "write", "flush"})
			promptOutput(t, s, "Label: \n")
		})
	}
	for _, position := range []int{0, 1} {
		for _, shape := range []string{"read-error", "empty-nil", "data-eof"} {
			t.Run(fmt.Sprintf("failure-%d-%s", position, shape), func(t *testing.T) {
				readErr := errors.New("synthetic escape read error")
				bad := promptRead{text: "candidate", err: readErr}
				if shape == "empty-nil" {
					bad = promptRead{}
				} else if shape == "data-eof" {
					bad = promptRead{text: "x", err: io.EOF}
				}
				chars := []promptRead{{text: "\x1b"}}
				if position == 1 {
					chars = append(chars, promptRead{text: "x"})
				}
				chars = append(chars, bad)
				s := &promptScript{characters: chars}
				answer, err := NewGenDataPrompt(s.ops(t)).Ask(context.Background(), "Label: ", true)
				if shape == "read-error" {
					promptFailure(t, answer, err, readErr)
				} else {
					promptBindingFailure(t, answer, err, "Character", "returned an invalid result")
				}
				trace := []string{"write", "flush", "begin", "character", "character"}
				if position == 1 {
					trace = append(trace, "character")
				}
				promptTrace(t, s.trace, append(trace, "restore"))
				promptOutput(t, s, "Label: ")
			})
		}
	}
}

func TestGenDataPromptProtectedEOF(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial-%t", partial), func(t *testing.T) {
			s := &promptScript{}
			want := ""
			trace := []string{"write", "flush", "begin"}
			if partial {
				s.characters = append(s.characters, promptRead{text: "東"})
				want = "東"
				trace = append(trace, "character", "flush")
			}
			s.characters = append(s.characters, promptRead{err: fmt.Errorf("end: %w", io.EOF)})
			answer, err := NewGenDataPrompt(s.ops(t)).Ask(context.Background(), "Label: ", true)
			promptAnswer(t, answer, want, err)
			promptTrace(t, s.trace, append(trace, "character", "restore", "write", "flush"))
			promptOutput(t, s, "Label: \n")
		})
	}
}

func TestGenDataPromptEncoding(t *testing.T) {
	for i, invalid := range []string{"\xff", "\xe2\x82", "\xc0\xaf", "\xed\xa0\x80"} {
		for _, final := range []bool{false, true} {
			t.Run(fmt.Sprintf("line-%d-final-%t", i, final), func(t *testing.T) {
				read := promptRead{text: invalid + "\n"}
				if final {
					read = promptRead{text: invalid, err: io.EOF}
				}
				s := &promptScript{lineRead: read}
				answer, err := NewGenDataPrompt(s.ops(t)).Ask(context.Background(), "Label: ", false)
				promptFailure(t, answer, err, ErrGenDataPromptEncoding)
				promptTrace(t, s.trace, []string{"write", "flush", "line"})
			})
		}
		t.Run(fmt.Sprintf("append-%d", i), func(t *testing.T) {
			s := &promptScript{characters: []promptRead{{text: "a"}, {text: invalid}}}
			answer, err := NewGenDataPrompt(s.ops(t)).Ask(context.Background(), "Label: ", true)
			promptFailure(t, answer, err, ErrGenDataPromptEncoding)
			promptTrace(t, s.trace, []string{"write", "flush", "begin", "character", "flush", "character", "restore"})
			promptOutput(t, s, "Label: ")
		})
	}
	t.Run("replacement-codepoint-is-valid", func(t *testing.T) {
		s := &promptScript{characters: []promptRead{{text: "\ufffd"}, {text: "\n"}}}
		answer, err := NewGenDataPrompt(s.ops(t)).Ask(context.Background(), "Label: ", true)
		promptAnswer(t, answer, "\ufffd", err)
	})
	for _, read := range []promptRead{{}, {text: "a", err: io.EOF}, {text: "éx"}} {
		t.Run(fmt.Sprintf("shape-%d-%t", len(read.text), read.err != nil), func(t *testing.T) {
			s := &promptScript{characters: []promptRead{read}}
			answer, err := NewGenDataPrompt(s.ops(t)).Ask(context.Background(), "Label: ", true)
			promptBindingFailure(t, answer, err, "Character", "returned an invalid result")
			promptTrace(t, s.trace, []string{"write", "flush", "begin", "character", "restore"})
		})
	}
	t.Run("future-candidate-not-validated", func(t *testing.T) {
		s := &promptScript{characters: []promptRead{{text: "\n"}, {text: "\xff"}}}
		p := NewGenDataPrompt(s.ops(t))
		answer, err := p.Ask(context.Background(), "First: ", true)
		promptAnswer(t, answer, "", err)
		if s.readCount != 1 {
			t.Fatal("future candidate was consumed")
		}
		answer, err = p.Ask(context.Background(), "Second: ", true)
		promptFailure(t, answer, err, ErrGenDataPromptEncoding)
	})
}

func TestGenDataPromptOutputFailures(t *testing.T) {
	writeErr := errors.New("synthetic write error")
	for _, tc := range []struct {
		name string
		n    int
		err  error
		want error
	}{
		{"zero-short", 0, nil, io.ErrShortWrite},
		{"partial-short", 2, nil, io.ErrShortWrite},
		{"zero-error", 0, writeErr, writeErr},
		{"partial-error", 2, writeErr, writeErr},
		{"full-error", 7, writeErr, writeErr},
	} {
		t.Run("label-"+tc.name, func(t *testing.T) {
			s := &promptScript{writes: []promptWrite{{n: tc.n, err: tc.err}}}
			answer, err := NewGenDataPrompt(s.ops(t)).Ask(context.Background(), "Label: ", true)
			promptFailure(t, answer, err, tc.want)
			promptTrace(t, s.trace, []string{"write"})
			promptOutput(t, s, "Label: "[:tc.n])
		})
	}
	flushErr := errors.New("synthetic flush error")
	t.Run("initial-flush", func(t *testing.T) {
		s := &promptScript{flushFail: 1, flushErr: flushErr}
		answer, err := NewGenDataPrompt(s.ops(t)).Ask(context.Background(), "Label: ", true)
		promptFailure(t, answer, err, flushErr)
		promptTrace(t, s.trace, []string{"write", "flush"})
		promptOutput(t, s, "Label: ")
	})
	for _, action := range []string{"a", "\b", "\x7f", "\x15", "\x1b"} {
		t.Run(fmt.Sprintf("iteration-%02x", action[0]), func(t *testing.T) {
			s := &promptScript{flushFail: 2, flushErr: flushErr, characters: []promptRead{{text: action}}}
			trace := []string{"write", "flush", "begin", "character"}
			if action == "\x1b" {
				s.characters = append(s.characters, promptRead{text: "["}, promptRead{text: "A"})
				trace = append(trace, "character", "character")
			}
			answer, err := NewGenDataPrompt(s.ops(t)).Ask(context.Background(), "Label: ", true)
			promptFailure(t, answer, err, flushErr)
			promptTrace(t, s.trace, append(trace, "flush", "restore"))
			promptOutput(t, s, "Label: ")
		})
	}
	for _, tc := range []struct {
		name string
		n    int
		err  error
		want error
	}{
		{"short", 0, nil, io.ErrShortWrite},
		{"zero-error", 0, writeErr, writeErr},
		{"full-error", 1, writeErr, writeErr},
	} {
		t.Run("newline-"+tc.name, func(t *testing.T) {
			s := &promptScript{writes: []promptWrite{{n: 7}, {n: tc.n, err: tc.err}}, characters: []promptRead{{text: "\n"}}}
			answer, err := NewGenDataPrompt(s.ops(t)).Ask(context.Background(), "Label: ", true)
			promptFailure(t, answer, err, tc.want)
			promptTrace(t, s.trace, []string{"write", "flush", "begin", "character", "restore", "write"})
			promptOutput(t, s, "Label: "+"\n"[:tc.n])
		})
	}
	t.Run("final-flush", func(t *testing.T) {
		s := &promptScript{flushFail: 2, flushErr: flushErr, characters: []promptRead{{text: "\n"}}}
		answer, err := NewGenDataPrompt(s.ops(t)).Ask(context.Background(), "Label: ", true)
		promptFailure(t, answer, err, flushErr)
		promptTrace(t, s.trace, []string{"write", "flush", "begin", "character", "restore", "write", "flush"})
		promptOutput(t, s, "Label: \n")
	})
	for _, operation := range []string{"Writer", "Flush", "Line", "BeginProtected"} {
		t.Run("missing-"+operation, func(t *testing.T) {
			s := &promptScript{}
			ops := s.ops(t)
			trace := []string{}
			protected := false
			switch operation {
			case "Writer":
				ops.Writer = nil
			case "Flush":
				ops.Flush = nil
				trace = []string{"write"}
			case "Line":
				ops.Line = nil
				trace = []string{"write", "flush"}
			case "BeginProtected":
				ops.BeginProtected = nil
				protected = true
				trace = []string{"write", "flush"}
			}
			answer, err := NewGenDataPrompt(ops).Ask(context.Background(), "Label: ", protected)
			promptBindingFailure(t, answer, err, operation, "is unavailable")
			if len(trace) == 0 && len(s.trace) == 0 {
				return
			}
			promptTrace(t, s.trace, trace)
		})
	}
}

func TestGenDataPromptRestore(t *testing.T) {
	beginErr := errors.New("synthetic begin error")
	readErr := errors.New("synthetic read error")
	restoreErr := errors.New("synthetic restore error")
	for _, tc := range []struct {
		name string
		s    promptScript
		want error
	}{
		{"begin-no-state", promptScript{beginErr: beginErr, noRestore: true}, beginErr},
		{"partial-begin", promptScript{beginErr: beginErr}, beginErr},
		{"partial-begin-restore-error", promptScript{beginErr: beginErr, restoreErr: restoreErr}, restoreErr},
		{"read", promptScript{characters: []promptRead{{text: "partial", err: readErr}}}, readErr},
		{"read-restore-error", promptScript{characters: []promptRead{{text: "partial", err: readErr}}, restoreErr: restoreErr}, restoreErr},
		{"interrupt", promptScript{characters: []promptRead{{text: "\x03"}}}, ErrGenDataPromptInterrupted},
		{"interrupt-restore-error", promptScript{characters: []promptRead{{text: "\x03"}}, restoreErr: restoreErr}, restoreErr},
		{"encoding-restore-error", promptScript{characters: []promptRead{{text: "\xff"}}, restoreErr: restoreErr}, restoreErr},
		{"flush-restore-error", promptScript{characters: []promptRead{{text: "a"}}, flushFail: 2, flushErr: readErr, restoreErr: restoreErr}, restoreErr},
		{"normal-restore-error", promptScript{characters: []promptRead{{text: "\r"}}, restoreErr: restoreErr}, restoreErr},
		{"eof-restore-error", promptScript{characters: []promptRead{{err: io.EOF}}, restoreErr: restoreErr}, restoreErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &tc.s
			answer, err := NewGenDataPrompt(s.ops(t)).Ask(context.Background(), "Label: ", true)
			promptFailure(t, answer, err, tc.want)
			trace := []string{"write", "flush", "begin"}
			if s.beginErr == nil {
				trace = append(trace, "character")
				if s.flushFail == 2 {
					trace = append(trace, "flush")
				}
			}
			if !s.noRestore {
				trace = append(trace, "restore")
			}
			promptTrace(t, s.trace, trace)
			promptOutput(t, s, "Label: ")
		})
	}
	for _, failRestore := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing-character-restore-error-%t", failRestore), func(t *testing.T) {
			s := &promptScript{noCharacter: true}
			if failRestore {
				s.restoreErr = restoreErr
			}
			answer, err := NewGenDataPrompt(s.ops(t)).Ask(context.Background(), "Label: ", true)
			if failRestore {
				promptFailure(t, answer, err, restoreErr)
			} else {
				promptBindingFailure(t, answer, err, "Character", "is unavailable")
			}
			promptTrace(t, s.trace, []string{"write", "flush", "begin", "restore"})
		})
	}
}

func TestGenDataPromptCancellation(t *testing.T) {
	for _, protected := range []bool{false, true} {
		for _, success := range []bool{false, true} {
			t.Run(fmt.Sprintf("already-canceled/protected-%t/success-%t", protected, success), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				s := &promptScript{}
				ops := s.ops(t)
				ops.Line = func(got context.Context) (string, error) {
					s.trace = append(s.trace, "line")
					if got != ctx || got.Err() != context.Canceled {
						t.Fatal("Line context was not forwarded")
					}
					if !success {
						return "partial", context.Canceled
					}
					return "synthetic\n", nil
				}
				ops.BeginProtected = func(got context.Context) (GenDataProtectedInput, error) {
					s.trace = append(s.trace, "begin")
					if got != ctx {
						t.Fatal("Begin context was not forwarded")
					}
					return GenDataProtectedInput{
						Character: func(got context.Context) (string, error) {
							s.trace = append(s.trace, "character")
							if got != ctx || got.Err() != context.Canceled {
								t.Fatal("Character context was not forwarded")
							}
							if !success {
								return "partial", context.Canceled
							}
							return "\n", nil
						},
						Restore: func() error { s.trace = append(s.trace, "restore"); return nil },
					}, nil
				}
				answer, err := NewGenDataPrompt(ops).Ask(ctx, "Label: ", protected)
				trace := []string{"write", "flush", "line"}
				want := "synthetic\n"
				output := "Label: "
				if protected {
					trace = []string{"write", "flush", "begin", "character", "restore"}
					want = ""
					if success {
						trace = append(trace, "write", "flush")
						output += "\n"
					}
				}
				if success {
					promptAnswer(t, answer, want, err)
				} else {
					promptFailure(t, answer, err, context.Canceled)
				}
				promptTrace(t, s.trace, trace)
				promptOutput(t, s, output)
			})
		}
	}
	t.Run("cancel-during-callback", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
		s := &promptScript{}
		ops := s.ops(t)
		ops.Line = func(got context.Context) (string, error) {
			s.trace = append(s.trace, "line")
			close(entered)
			<-release
			return "partial", got.Err()
		}
		var answer string
		var err error
		go func() {
			answer, err = NewGenDataPrompt(ops).Ask(ctx, "Label: ", false)
			close(done)
		}()
		<-entered
		cancel()
		close(release)
		<-done
		promptFailure(t, answer, err, context.Canceled)
		promptTrace(t, s.trace, []string{"write", "flush", "line"})
	})
	for _, end := range []promptRead{{text: "\r"}, {err: io.EOF}} {
		t.Run(fmt.Sprintf("cancel-at-normal-end/eof-%t", end.err != nil), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := &promptScript{}
			ops := s.ops(t)
			ops.BeginProtected = func(context.Context) (GenDataProtectedInput, error) {
				s.trace = append(s.trace, "begin")
				return GenDataProtectedInput{
					Character: func(context.Context) (string, error) {
						s.trace = append(s.trace, "character")
						cancel()
						return end.text, end.err
					},
					Restore: func() error { s.trace = append(s.trace, "restore"); return nil },
				}, nil
			}
			answer, err := NewGenDataPrompt(ops).Ask(ctx, "Label: ", true)
			promptAnswer(t, answer, "", err)
			promptTrace(t, s.trace, []string{"write", "flush", "begin", "character", "restore", "write", "flush"})
		})
	}
	t.Run("callback-and-restore-errors-retained-on-canceled-context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		beginErr, restoreErr := errors.New("synthetic entry error"), errors.New("synthetic restore error")
		for _, restored := range []bool{false, true} {
			s := &promptScript{beginErr: beginErr}
			want := beginErr
			if restored {
				s.restoreErr, want = restoreErr, restoreErr
			}
			answer, err := NewGenDataPrompt(s.ops(t)).Ask(ctx, "Label: ", true)
			promptFailure(t, answer, err, want)
			promptTrace(t, s.trace, []string{"write", "flush", "begin", "restore"})
		}
	})
}

type promptBorrowedReader struct {
	*strings.Reader
	closed int
}

func (r *promptBorrowedReader) Close() error { r.closed++; return nil }

func TestGenDataPromptBorrowedInput(t *testing.T) {
	// A single buffered owner reads ahead on the username. Every later mode
	// consumes its retained suffix, including two different challenge branches.
	input := &promptBorrowedReader{Reader: strings.NewReader(" synthetic-user \nsynthetic-password\nsynthetic-visible\nsynthetic-protected\n")}
	queue := bufio.NewReader(input)
	s := &promptScript{}
	var bufferedAfterLine int
	ops := GenDataPromptIO{
		Writer: s,
		Flush:  s.flush,
		Line: func(context.Context) (string, error) {
			s.trace = append(s.trace, "line")
			line, err := queue.ReadString('\n')
			bufferedAfterLine = queue.Buffered()
			return line, err
		},
		BeginProtected: func(context.Context) (GenDataProtectedInput, error) {
			s.trace = append(s.trace, "begin")
			return GenDataProtectedInput{
				Character: func(context.Context) (string, error) {
					s.trace = append(s.trace, "character")
					r, _, err := queue.ReadRune()
					if err != nil {
						return "", err
					}
					return string(r), nil
				},
				Restore: func() error { s.trace = append(s.trace, "restore"); return nil },
			}, nil
		},
	}
	p := NewGenDataPrompt(ops)
	questions := []struct {
		label, want string
		protected   bool
	}{
		{"User name: ", " synthetic-user \n", false},
		{"Password: ", "synthetic-password", true},
		{"Visible: ", "synthetic-visible\n", false},
		{"Protected: ", "synthetic-protected", true},
	}
	for i, q := range questions {
		answer, err := p.Ask(context.Background(), q.label, q.protected)
		promptAnswer(t, answer, q.want, err)
		if i == 0 && bufferedAfterLine == 0 {
			t.Fatal("fixture failed to exercise shared read-ahead")
		}
	}
	if queue.Buffered() != 0 || input.Len() != 0 || input.closed != 0 {
		t.Fatal("borrowed input consumption or ownership differs")
	}
	promptOutput(t, s, "User name: Password: \nVisible: Protected: \n")
}
