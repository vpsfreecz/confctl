package inputs

// Scalar grammar/error ordering translated from JSON2.21.2 parser.c; see
// ordered_lock.NOTICE. No SIMD, SWAR or vendored numeric decoder is copied.
import (
	"bytes"
	"fmt"
	"math/big"
	"strings"
)

type orderedLockParser struct {
	input []byte
	pos   int
}

func parseOrderedLock(input []byte) (*orderedLockDocument, error) {
	p := orderedLockParser{input: input}
	v, err := p.value(0)
	if err != nil {
		return nil, err
	}
	if err := p.space(); err != nil {
		return nil, err
	}
	if p.pos != len(input) {
		return nil, p.fail("unexpected token at end of stream %s", p.pos)
	}
	return &orderedLockDocument{root: v}, nil
}

func (p *orderedLockParser) peek() byte {
	if p.pos == len(p.input) {
		return 0
	}
	return p.input[p.pos]
}

func (p *orderedLockParser) space() error {
	for p.pos < len(p.input) {
		switch p.peek() {
		case ' ', '\t', '\r', '\n':
			p.pos++
		case '/':
			start := p.pos
			p.pos++
			switch p.peek() {
			case '/':
				if i := bytes.IndexByte(p.input[p.pos:], '\n'); i >= 0 {
					p.pos += i + 1
				} else {
					p.pos = len(p.input)
				}
			case '*':
				p.pos++
				i := bytes.Index(p.input[p.pos:], []byte("*/"))
				if i < 0 {
					return p.fail("unterminated comment, expected closing '*/'", start)
				}
				p.pos += i + 2
			default:
				return p.fail("unexpected token %s", start)
			}
		default:
			return nil
		}
	}
	return nil
}

func (p *orderedLockParser) fail(format string, at int) error {
	fragment := "EOF"
	if at < len(p.input) {
		// With a zero-length fragment the source uses the unquoted C string
		// at the cursor (including a literal NUL's empty string).
		rest := p.input[at:]
		if zero := bytes.IndexByte(rest, 0); zero >= 0 {
			rest = rest[:zero]
		}
		fragment = string(rest)
		end := at
		for end < len(p.input) && end-at < 32 && p.input[end] != 0 && !strings.ContainsRune(" \t\r\n", rune(p.input[end])) {
			end++
		}
		if end > at {
			// Source fragments avoid ending on a partial or final multibyte character.
			for end > at && p.input[end-1] >= 0x80 && p.input[end-1] < 0xc0 {
				end--
			}
			if end > at && p.input[end-1] >= 0xc0 {
				end--
			}
			fragment = "'" + string(p.input[at:end]) + "'"
		}
	}
	line, column := 1, 0
	for i := 0; i <= at; i++ {
		if i < len(p.input) && p.input[i] == '\n' {
			line++
			column = 0
		} else {
			column++
		}
	}
	message := format
	if strings.Contains(format, "%s") {
		message = fmt.Sprintf(format, fragment)
	}
	message += fmt.Sprintf(" at line %d column %d", line, column)
	return &orderedLockError{phase: "parse", kind: "JSON::ParserError", message: message, cursor: at, line: line, column: column}
}

func (p *orderedLockParser) value(depth int) (*orderedLockValue, error) {
	if err := p.space(); err != nil {
		return nil, err
	}
	start := p.pos
	if start == len(p.input) {
		return nil, p.fail("unexpected end of input", start)
	}
	switch p.peek() {
	case 'n', 't', 'f':
		word, v := "null", &orderedLockValue{kind: orderedNull}
		if p.peek() == 't' {
			word, v = "true", &orderedLockValue{kind: orderedBool, boolean: true}
		}
		if p.peek() == 'f' {
			word, v = "false", &orderedLockValue{kind: orderedBool}
		}
		if !bytes.HasPrefix(p.input[start:], []byte(word)) {
			return nil, p.fail("unexpected token %s", start)
		}
		p.pos += len(word)
		return v, nil
	case 'N', 'I':
		return nil, p.fail("unexpected token %s", start)
	case 0:
		return nil, p.fail("unexpected NULL byte: %s", start)
	case '"':
		s, err := p.string()
		if err != nil {
			return nil, err
		}
		return &orderedLockValue{kind: orderedString, text: s}, nil
	case '[', '{':
		return p.container(depth)
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return p.number()
	default:
		return nil, p.fail("unexpected character: %s", start)
	}
}

func (p *orderedLockParser) container(depth int) (*orderedLockValue, error) {
	object := p.peek() == '{'
	p.pos++
	if err := p.space(); err != nil {
		return nil, err
	}
	v := &orderedLockValue{kind: orderedArray}
	end := byte(']')
	if object {
		v.kind = orderedObject
		v.index = make(map[string]int)
		end = '}'
	}
	// Empty containers close before the parser's depth check, unlike generation.
	if p.peek() == end {
		p.pos++
		return v, nil
	}
	depth++
	if depth > 100 {
		return nil, &orderedLockError{phase: "parse", kind: "JSON::NestingError", message: fmt.Sprintf("nesting of %d is too deep", depth), cursor: -1}
	}
	count := 0
	for {
		var key string
		if object {
			if err := p.space(); err != nil {
				return nil, err
			}
			if p.peek() != '"' {
				f := "expected object key, got %s"
				if count > 0 {
					f = "expected object key, got: %s"
				}
				return nil, p.fail(f, p.pos)
			}
			var err error
			key, err = p.string()
			if err != nil {
				return nil, err
			}
			if err := p.space(); err != nil {
				return nil, err
			}
			if p.peek() != ':' {
				f := "expected ':' after object key"
				if count > 0 {
					f = "expected ':' after object key, got: %s"
				}
				return nil, p.fail(f, p.pos)
			}
			p.pos++
		}
		item, err := p.value(depth)
		if err != nil {
			return nil, err
		}
		if object {
			v.setOwned(key, item)
		} else {
			v.elements = append(v.elements, item)
		}
		count++
		if err := p.space(); err != nil {
			return nil, err
		}
		if p.peek() == end {
			p.pos++
			return v, nil
		}
		if p.peek() != ',' {
			f := "expected ',' or ']' after array value"
			if object {
				f = "expected ',' or '}' after object value, got: %s"
			}
			return nil, p.fail(f, p.pos)
		}
		p.pos++
	}
}

func (p *orderedLockParser) string() (string, error) {
	p.pos++
	start, end := p.pos, p.pos
	// Scan to the closing quote before unescaping, preserving original error order.
	for end < len(p.input) && p.input[end] != '"' {
		if p.input[end] < 0x20 {
			return "", p.fail("invalid ASCII control character in string: %s", end)
		}
		if p.input[end] == '\\' {
			end++
		}
		end++
	}
	if end >= len(p.input) {
		return "", p.fail("unexpected end of input, expected closing \"", len(p.input))
	}
	p.pos = end + 1
	out := make([]byte, 0, end-start)
	// segment is the C unescaper's p: diagnostics for a surrogate use this
	// source segment start, which can precede the particular escape.
	segment := start
	for i := start; i < end; {
		if p.input[i] != '\\' {
			out = append(out, p.input[i])
			i++
			continue
		}
		escape := i
		i++
		switch p.input[i] {
		case '"', '/':
			out = append(out, p.input[i])
			segment = i
			i++
		case '\\':
			out = append(out, '\\')
			i++
			segment = i
		case 'b':
			out = append(out, '\b')
			i++
			segment = i
		case 'f':
			out = append(out, '\f')
			i++
			segment = i
		case 'n':
			out = append(out, '\n')
			i++
			segment = i
		case 'r':
			out = append(out, '\r')
			i++
			segment = i
		case 't':
			out = append(out, '\t')
			i++
			segment = i
		case 'u':
			ch, err := p.hex4(i+1, end)
			if err != nil {
				return "", err
			}
			i += 5
			if ch&0xfc00 == 0xd800 {
				if i+6 > end || p.input[i] != '\\' || p.input[i+1] != 'u' {
					return "", p.fail("incomplete surrogate pair at %s", segment)
				}
				low, err := p.hex4(i+2, end)
				if err != nil {
					return "", err
				}
				if low&0xfc00 != 0xdc00 {
					return "", p.fail("invalid surrogate pair at %s", segment)
				}
				ch = 0x10000 + ((ch - 0xd800) << 10) + low - 0xdc00
				i += 6
			}
			out = orderedAppendUTF8(out, ch)
			segment = i
		case 0:
			// The original unescaper returns an incomplete string for an
			// escaped literal NUL, after the scanner consumed its quote.
			return "", p.fail("unexpected end of input, expected closing \"", p.pos)
		default:
			if p.input[i] < 0x20 {
				if p.input[i] == '\n' {
					return "", p.fail("Invalid unescaped newline character (\\n) in string: %s", escape)
				}
				return "", p.fail("invalid ASCII control character in string: %s", escape)
			}
			return "", p.fail("invalid escape character in string: %s", escape)
		}
	}
	return string(out), nil
}

func (p *orderedLockParser) hex4(start, end int) (uint32, error) {
	if start+4 > end {
		return 0, p.fail("incomplete unicode character escape sequence at %s", start-2)
	}
	var ch uint32
	for _, b := range p.input[start : start+4] {
		var n byte
		switch {
		case b >= '0' && b <= '9':
			n = b - '0'
		case b >= 'a' && b <= 'f':
			n = b - 'a' + 10
		case b >= 'A' && b <= 'F':
			n = b - 'A' + 10
		default:
			return 0, p.fail("incomplete unicode character escape sequence at %s", start-2)
		}
		ch = ch<<4 | uint32(n)
	}
	return ch, nil
}

// Deliberately encodes lone low surrogates as their original three bytes.
// utf8.AppendRune would replace them and move the observed generator failure.
func orderedAppendUTF8(out []byte, ch uint32) []byte {
	switch {
	case ch <= 0x7f:
		return append(out, byte(ch))
	case ch <= 0x7ff:
		return append(out, byte(ch>>6)|0xc0, byte(ch&0x3f)|0x80)
	case ch <= 0xffff:
		return append(out, byte(ch>>12)|0xe0, byte(ch>>6&0x3f)|0x80, byte(ch&0x3f)|0x80)
	default:
		return append(out, byte(ch>>18)|0xf0, byte(ch>>12&0x3f)|0x80, byte(ch>>6&0x3f)|0x80, byte(ch&0x3f)|0x80)
	}
}

func (p *orderedLockParser) digits(acc *uint64) uint64 {
	start := p.pos
	for p.peek() >= '0' && p.peek() <= '9' {
		*acc = *acc*10 + uint64(p.peek()-'0')
		p.pos++
	}
	return uint64(p.pos - start)
}

func (p *orderedLockParser) number() (*orderedLockValue, error) {
	start := p.pos
	negative := p.peek() == '-'
	if negative {
		p.pos++
	}
	first := p.peek()
	var mantissa uint64
	count := p.digits(&mantissa)
	if count == 0 || first == '0' && count > 1 {
		return nil, p.fail("invalid number: %s", start)
	}
	isFloat := false
	var fraction uint64
	if p.peek() == '.' {
		isFloat = true
		p.pos++
		fraction = p.digits(&mantissa)
		count += fraction
		if fraction == 0 {
			return nil, p.fail("invalid number: %s", start)
		}
	}
	var exponent int64
	if p.peek() == 'e' || p.peek() == 'E' {
		isFloat = true
		p.pos++
		negativeExponent := p.peek() == '-'
		if p.peek() == '-' || p.peek() == '+' {
			p.pos++
		}
		var magnitude uint64
		digits := p.digits(&magnitude)
		if digits == 0 {
			return nil, p.fail("invalid number: %s", start)
		}
		exponent = orderedLockExponent(magnitude, digits, negativeExponent)
	}
	token := string(p.input[start:p.pos])
	if !isFloat {
		n, ok := new(big.Int).SetString(token, 10)
		if !ok {
			return nil, fmt.Errorf("ordered lock: validated integer conversion failed")
		}
		return &orderedLockValue{kind: orderedInteger, integer: n}, nil
	}
	exponent = orderedLockFractionExponent(exponent, fraction)
	f, err := orderedLockParseFloat(token, count, exponent, negative)
	if err != nil {
		return nil, err
	}
	return &orderedLockValue{kind: orderedFloat, float: f}, nil
}
