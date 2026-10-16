package ui

import (
	"errors"
	"fmt"
	"math/big"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

var ErrBasicEncoding = errors.New("basic pager encoding error")

// WrapBasic hard-wraps display columns while preserving spaces, ANSI sequences
// and grapheme clusters. The pager retains ownership of input and pagination.
func WrapBasic(text string, columns *big.Int) (string, error) {
	if columns == nil {
		return "", fmt.Errorf("basic pager wrap width is unavailable")
	}
	if columns.Sign() == 0 {
		return text, nil // Zero width deliberately bypasses UTF-8 decoding.
	}
	if !utf8.ValidString(text) {
		return "", fmt.Errorf("%w: text is not valid UTF-8", ErrBasicEncoding)
	}
	// Byte length bounds the display width. This also keeps widths beyond a
	// native int finite without narrowing them or allocating a viewport.
	if columns.Sign() < 0 || columns.Cmp(new(big.Int).SetInt64(int64(len(text)))) >= 0 {
		return text, nil
	}
	return ansi.Hardwrap(text, int(columns.Int64()), true), nil
}
