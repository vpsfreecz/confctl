package inputs

// Default pretty generation ordering/escaping/depth from JSON2.21.2
// generator.c; see ordered_lock.NOTICE. Generation is buffered: errors expose
// no partial bytes, and the caller owns the separate final newline.
import (
	"bytes"
	"math"
	"strings"
	"unicode/utf8"
)

func prettyOrderedLock(d *orderedLockDocument) ([]byte, error) {
	var buffer bytes.Buffer
	if err := orderedLockGenerate(&buffer, d.root, 0); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func orderedLockGenerateError(kind, message string) error {
	return &orderedLockError{phase: "generate", kind: kind, message: message, cursor: -1}
}

func orderedLockGenerate(out *bytes.Buffer, v *orderedLockValue, depth int) error {
	switch v.kind {
	case orderedNull:
		out.WriteString("null")
	case orderedBool:
		if v.boolean {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case orderedString:
		return orderedLockQuote(out, v.text)
	case orderedInteger:
		out.WriteString(v.integer.String())
	case orderedFloat:
		if math.IsInf(v.float, 0) || math.IsNaN(v.float) {
			name := "NaN"
			if math.IsInf(v.float, 1) {
				name = "Infinity"
			}
			if math.IsInf(v.float, -1) {
				name = "-Infinity"
			}
			return orderedLockGenerateError("JSON::GeneratorError", name+" not allowed in JSON")
		}
		out.WriteString(orderedLockFloatString(v.float))
	case orderedObject, orderedArray:
		depth++
		if depth > 100 {
			return orderedLockGenerateError("JSON::NestingError", "nesting of 100 is too deep. Did you try to serialize objects with circular references?")
		}
		open, close, count := byte('['), byte(']'), len(v.elements)
		if v.kind == orderedObject {
			open, close, count = '{', '}', len(v.members)
		}
		out.WriteByte(open)
		if count == 0 {
			out.WriteByte(close)
			return nil
		}
		for i := 0; i < count; i++ {
			if i != 0 {
				out.WriteByte(',')
			}
			out.WriteByte('\n')
			out.WriteString(strings.Repeat("  ", depth))
			var child *orderedLockValue
			if v.kind == orderedObject {
				if err := orderedLockQuote(out, v.members[i].key); err != nil {
					return err
				}
				out.WriteString(": ")
				child = v.members[i].value
			} else {
				child = v.elements[i]
			}
			if err := orderedLockGenerate(out, child, depth); err != nil {
				return err
			}
		}
		out.WriteByte('\n')
		out.WriteString(strings.Repeat("  ", depth-1))
		out.WriteByte(close)
	default:
		panic("invalid ordered lock tag")
	}
	return nil
}

func orderedLockQuote(out *bytes.Buffer, s string) error {
	if !utf8.ValidString(s) {
		return orderedLockGenerateError("JSON::GeneratorError", "source sequence is illegal/malformed utf-8")
	}
	const hex = "0123456789abcdef"
	out.WriteByte('"')
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch b {
		case '"', '\\':
			out.WriteByte('\\')
			out.WriteByte(b)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if b < 0x20 {
				out.WriteString(`\u00`)
				out.WriteByte(hex[b>>4])
				out.WriteByte(hex[b&15])
			} else {
				out.WriteByte(b)
			}
		}
	}
	out.WriteByte('"')
	return nil
}
