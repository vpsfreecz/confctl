package ui

import (
	"errors"
	"unicode/utf8"
)

var ErrFamilyMeasureEncoding = errors.New("family progress text is not valid UTF-8")

// MeasureFamilyProgress measures reached family-progress text using the pinned
// Strings::ANSI sanitizer and unicode-display_width 2.6.0 / Unicode 16.0.0
// defaults: ambiguous width 1, empty overwrite map and emoji correction disabled.
// It reuses the pager width owner; its source licenses are in pager_source.NOTICE.
// Whole-input UTF-8 admission precedes sanitization, which affects measurement
// only. The caller retains the original text for output.
func MeasureFamilyProgress(text string) (int, error) {
	if !utf8.ValidString(text) {
		return 0, ErrFamilyMeasureEncoding
	}
	return pagerDisplayWidth(text), nil
}
