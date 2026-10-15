package ui

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"unicode/utf8"
)

var ErrBasicEncoding = errors.New("basic pager encoding error")

const pagerANSIReset = "\x1b[0m"
const pagerANSIPattern = `(\[)?\x1b(\[)?[;?0-9]*[0-9A-Za-z]([\];])?`

var pagerANSIMatcher = regexp.MustCompile(pagerANSIPattern)

// Ruby ^/$ anchor each LF-delimited line, not the entire string.
var pagerANSIOnly = regexp.MustCompile(`(?m)^(` + pagerANSIPattern + `)+$`)

func pagerSanitize(text string) string { return pagerANSIMatcher.ReplaceAllString(text, "") }

// WrapBasic ports only the pinned Strings.wrap default separator/width mode.
// Width zero bypasses decoding. Valid non-ASCII skips Ruby's provider-dependent
// printable fast path: a short colored value can gain a RESET or reach the
// source's display-offset/character-index insertion error.
func WrapBasic(text string, columns *big.Int) (string, error) {
	if columns == nil {
		return "", fmt.Errorf("basic pager wrap width is unavailable")
	}
	if columns.Sign() == 0 {
		return text, nil
	}
	if !utf8.ValidString(text) {
		return "", fmt.Errorf("%w: text is not valid UTF-8", ErrBasicEncoding)
	}
	ascii, printable := true, 0
	for i := 0; i < len(text); i++ {
		if text[i] >= utf8.RuneSelf {
			ascii = false
		}
		if text[i] >= 0x20 && text[i] <= 0x7e {
			printable++
		}
	}
	if ascii && new(big.Int).SetInt64(int64(printable)).Cmp(columns) < 0 {
		return text, nil
	}
	var stack []pagerANSIEntry
	var result strings.Builder
	for _, line := range pagerLines(text) {
		formatted, err := formatPagerLine(line, columns, &stack)
		if err != nil {
			return "", err
		}
		result.WriteString(strings.Join(formatted, "\n"))
	}
	return result.String(), nil
}

// Width's ASCII path and non-ASCII shortcut differ deliberately. Do not replace
// either with compiler unicode tables, per-rune clamping or emoji correction.
func pagerDisplayWidth(text string) int {
	text = pagerSanitize(text)
	ascii := true
	for i := 0; i < len(text); i++ {
		if text[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	sum := 0
	if ascii {
		for i := 0; i < len(text); i++ {
			switch text[i] {
			case '\b':
				sum--
			case 0, 5, 7, '\n', '\v', '\f', '\r', 14, 15:
			default:
				sum++
			}
		}
	} else {
		for _, codepoint := range text {
			sum += pagerRangeValue(pagerWidthRanges[:], codepoint, 1)
		}
	}
	return max(0, sum)
}

func pagerGraphemes(text string) []string {
	var clusters []string
	start, previous := 0, 255
	for offset, codepoint := range text {
		property := pagerRangeValue(pagerGraphemeRanges[:], codepoint, 255)
		boundary := true
		switch {
		case previous == 0 && property == 1:
			boundary = false // CR x LF
		case previous <= 2 || property <= 2:
		case previous == 6 && (property == 6 || property == 7 || property == 9 || property == 10):
			boundary = false
		case (previous == 9 || previous == 7) && (property == 7 || property == 8):
			boundary = false
		case (previous == 10 || previous == 8) && property == 8:
			boundary = false
		case previous == 11 && property == 11:
			boundary = false // old adjacent-RI rule, without modern pair parity
		case property == 3 || property == 5 || previous == 4:
			boundary = false
		}
		if boundary && offset != start {
			clusters = append(clusters, text[start:offset])
			start = offset
		}
		previous = property
	}
	if start < len(text) {
		clusters = append(clusters, text[start:])
	}
	return clusters
}

type pagerANSIEntry struct {
	code     string
	position int
}

func pagerWithinWidth(count int, columns *big.Int) bool {
	return new(big.Int).SetInt64(int64(count)).Cmp(columns) <= 0
}

func formatPagerLine(text string, columns *big.Int, stack *[]pagerANSIEntry) ([]string, error) {
	var lines, line, word, ansi []string
	matched := false
	wordLength, lineLength, totalLength := 0, 0, 0
	textLength := pagerDisplayWidth(text)
	appendLine := func(text string) error {
		formatted, err := insertPagerANSI(text, stack)
		if err != nil {
			return err
		}
		lines = append(lines, formatted)
		return nil
	}
	for _, char := range pagerGraphemes(text) {
		if char == "\x1b" || len(ansi) > 0 {
			ansi = append(ansi, char)
			if pagerANSIOnly.MatchString(strings.Join(ansi, "")) {
				matched = true
			} else if matched {
				*stack = append(*stack, pagerANSIEntry{strings.Join(ansi[:len(ansi)-1], ""), lineLength + wordLength})
				matched = false
				if ansi[len(ansi)-1] == "\x1b" {
					ansi = []string{ansi[len(ansi)-1]}
				} else {
					ansi = nil
				}
			}
			if len(ansi) > 0 {
				continue
			}
		}
		charLength := pagerDisplayWidth(char)
		totalLength += charLength
		if pagerWithinWidth(lineLength+wordLength+charLength, columns) {
			if char == " " || totalLength == textLength {
				line = append(line, strings.Join(word, "")+char)
				lineLength += wordLength + charLength
				word, wordLength = nil, 0
			} else {
				word = append(word, char)
				wordLength += charLength
			}
			continue
		}
		if char == " " {
			if err := appendLine(strings.Join(line, "")); err != nil {
				return nil, err
			}
			line, lineLength = nil, 0
			word = append(word, char)
			wordLength += charLength
		} else if pagerWithinWidth(wordLength+charLength, columns) {
			if err := appendLine(strings.Join(line, "")); err != nil {
				return nil, err
			}
			line = []string{strings.Join(word, "") + char}
			lineLength = wordLength + charLength
			word, wordLength = nil, 0
		} else {
			// The source calls this hyphenate, but inserts no hyphen and does
			// not clear line. Its later output order is retained literally.
			if err := appendLine(strings.Join(word, "")); err != nil {
				return nil, err
			}
			lineLength = 0
			word = []string{char}
			wordLength = charLength
		}
	}
	if len(line) != 0 {
		if err := appendLine(strings.Join(line, "")); err != nil {
			return nil, err
		}
	}
	if len(word) != 0 {
		if err := appendLine(strings.Join(word, "")); err != nil {
			return nil, err
		}
	}
	return lines, nil
}

func insertPagerText(text string, position int, inserted string) (string, error) {
	characters := []rune(text)
	if position < 0 {
		position += len(characters) + 1
	}
	if position < 0 || position > len(characters) {
		return "", fmt.Errorf("basic pager ANSI insertion index %d outside %d characters", position, len(characters))
	}
	return string(characters[:position]) + inserted + string(characters[position:]), nil
}

func insertPagerANSI(text string, stack *[]pagerANSIEntry) (string, error) {
	if len(*stack) == 0 || text == "" {
		return text, nil
	}
	output := text
	length := utf8.RuneCountInString(text)
	matchedReset := false
	var nextStack []pagerANSIEntry
	for i := len(*stack) - 1; i >= 0; i-- {
		entry := (*stack)[i]
		var err error
		if strings.Contains(entry.code, pagerANSIReset) {
			matchedReset = true
			output, err = insertPagerText(output, entry.position, pagerANSIReset)
			if err != nil {
				return "", err
			}
			continue
		} else if !matchedReset {
			nextStack = append(nextStack, entry)
			if entry.position == length {
				continue
			}
			position := -1
			if strings.HasSuffix(output, "\n") {
				position = -2
			}
			output, err = insertPagerText(output, position, pagerANSIReset)
			if err != nil {
				return "", err
			}
		}
		output, err = insertPagerText(output, entry.position, entry.code)
		if err != nil {
			return "", err
		}
	}
	*stack = nextStack
	return output, nil
}
