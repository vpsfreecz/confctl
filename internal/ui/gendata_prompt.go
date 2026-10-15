package ui

import (
	"context"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// GenDataProtectedInput borrows one input owner. An acquired Restore must be
// returned even when entry fails; Ask calls it once without a context.
type GenDataProtectedInput struct {
	Character func(context.Context) (string, error)
	Restore   func() error
}

// GenDataPromptIO supplies invocation-owned streams and input operations. Line
// and Character must share any read-ahead buffer. Ask neither closes these
// resources nor creates a reader, worker, or native terminal binding.
type GenDataPromptIO struct {
	Writer         io.Writer
	Flush          func() error
	Line           func(context.Context) (string, error)
	BeginProtected func(context.Context) (GenDataProtectedInput, error)
}

// GenDataPrompt implements only the finite gen-data question sequence derived
// from HighLine 3.1.2; see gendata_prompt.NOTICE. Calls must be sequential, with
// a non-nil context and stable borrowed resources.
type GenDataPrompt struct {
	ops GenDataPromptIO
}

var (
	ErrGenDataPromptInterrupted = errors.New("authentication input interrupted")
	ErrGenDataPromptEncoding    = errors.New("authentication input is not valid UTF-8")
)

// NewGenDataPrompt copies bindings without accessing or validating resources.
func NewGenDataPrompt(ops GenDataPromptIO) *GenDataPrompt {
	return &GenDataPrompt{ops: ops}
}

// Ask writes the literal label before reaching input. Successful answers remain
// raw: authentication owns stripping and validation. Cancellation is observed
// only by input callbacks, and restoration errors supersede earlier errors as
// HighLine's outer ensure does. This private error identity is separate from
// the authentication owner's redacted diagnostics.
func (p *GenDataPrompt) Ask(ctx context.Context, label string, protected bool) (string, error) {
	if err := p.write(label); err != nil {
		return "", err
	}
	if p.ops.Flush == nil {
		return "", genDataMissing("Flush")
	}
	if err := p.ops.Flush(); err != nil {
		return "", err
	}
	if !protected {
		return p.line(ctx)
	}
	if p.ops.BeginProtected == nil {
		return "", genDataMissing("BeginProtected")
	}
	input, err := p.ops.BeginProtected(ctx)
	var answer string
	if err == nil {
		answer, err = p.protected(ctx, input.Character)
	}
	// Retain partial acquisition on Begin failure. There is no invented restore
	// when no saved state was returned, no cancellation check, and no retry.
	if input.Restore != nil {
		if restoreErr := input.Restore(); restoreErr != nil {
			return "", restoreErr
		}
	}
	if err != nil {
		return "", err
	}
	if err := p.write("\n"); err != nil {
		return "", err
	}
	if err := p.ops.Flush(); err != nil {
		return "", err
	}
	return answer, nil
}

func (p *GenDataPrompt) write(text string) error {
	if p.ops.Writer == nil {
		return genDataMissing("Writer")
	}
	n, err := p.ops.Writer.Write([]byte(text))
	if err != nil {
		return err
	}
	if n != len(text) {
		return io.ErrShortWrite
	}
	return nil
}

func (p *GenDataPrompt) line(ctx context.Context) (string, error) {
	if p.ops.Line == nil {
		return "", genDataMissing("Line")
	}
	line, err := p.ops.Line(ctx)
	lf := strings.IndexByte(line, '\n')
	if err != nil {
		if !errors.Is(err, io.EOF) {
			return "", err
		}
		if line == "" {
			return "", err
		}
		if lf >= 0 {
			return "", genDataInvalid("Line")
		}
	} else if line == "" || lf != len(line)-1 {
		return "", genDataInvalid("Line")
	}
	if !utf8.ValidString(line) {
		return "", ErrGenDataPromptEncoding
	}
	return line, nil
}

func (p *GenDataPrompt) protected(ctx context.Context, character func(context.Context) (string, error)) (string, error) {
	if character == nil {
		return "", genDataMissing("Character")
	}
	var answer []rune
	for {
		c, eof, err := genDataCharacter(ctx, character)
		if err != nil {
			return "", err
		}
		if eof {
			return string(answer), nil
		}
		switch c {
		case "\x03":
			return "", ErrGenDataPromptInterrupted
		case "\r", "\n":
			return string(answer), nil
		case "\b", "\x7f":
			if len(answer) > 0 {
				answer = answer[:len(answer)-1]
			}
		case "\x15":
			answer = answer[:0]
		case "\x1b":
			// Source discards exactly two candidates, including EOF. These
			// results are shape-checked but never decoded or interpreted.
			for i := 0; i < 2; i++ {
				if _, _, err := genDataCharacter(ctx, character); err != nil {
					return "", err
				}
			}
		default:
			if !utf8.ValidString(c) {
				return "", ErrGenDataPromptEncoding
			}
			r, size := utf8.DecodeRuneInString(c)
			if size != len(c) {
				return "", genDataInvalid("Character")
			}
			answer = append(answer, r)
		}
		if err := p.ops.Flush(); err != nil {
			return "", err
		}
	}
}

// genDataCharacter checks representation only. UTF-8 admission belongs to a
// reached ordinary append, so escape discards cannot validate buffered input.
func genDataCharacter(ctx context.Context, read func(context.Context) (string, error)) (string, bool, error) {
	c, err := read(ctx)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			return "", false, err
		}
		if c == "" {
			return "", true, nil
		}
		return "", false, genDataInvalid("Character")
	}
	if c == "" {
		return "", false, genDataInvalid("Character")
	}
	return c, false, nil
}

func genDataMissing(operation string) error {
	return errors.New("authentication prompt " + operation + " binding is unavailable")
}

func genDataInvalid(operation string) error {
	return errors.New("authentication prompt " + operation + " binding returned an invalid result")
}
