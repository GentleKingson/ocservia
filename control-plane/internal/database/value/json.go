package value

import (
	"cmp"
	"errors"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// JSONB retains decimal precision and scale. nil represents SQL NULL; the
// JSON literal null is a valid, non-nil value. Bytes returns a defensive copy.
type JSONB struct{ data []byte }

func (j JSONB) Bytes() []byte { return append([]byte(nil), j.data...) }
func (j JSONB) Valid() bool   { return j.data != nil }

// maxJSONBBytes bounds the whole canonical output of one document. The
// largest accepted input is the 1 MiB default HTTP body (telemetry batches are
// 512 KiB); canonical escaping grows a byte by at most six, so 8 MiB keeps every
// such document while capping repeated legal-but-wide numbers (up to ~147 KB
// each) that a few input bytes could otherwise expand into gigabytes.
const (
	maxJSONBBytes = 8 << 20
	maxJSONDepth  = 10000
)

var (
	errJSONBTooLarge = errors.New("database: JSON canonical output too large")
	errInvalidJSON   = errors.New("database: invalid JSON")
	errInvalidUTF8   = errors.New("database: invalid JSON Unicode")
	errTrailingJSON  = errors.New("database: trailing JSON input")
)

func ParseJSONB(data []byte) (JSONB, error) { return parseJSONB(data, maxJSONBBytes) }

func parseJSONB(data []byte, budget int64) (JSONB, error) {
	if data == nil {
		return JSONB{}, nil
	}
	if !utf8.Valid(data) {
		return JSONB{}, errInvalidUTF8
	}
	p := jsonParser{data: data}
	p.skipSpace()
	v, err := p.parseValue()
	if err != nil {
		return JSONB{}, err
	}
	p.skipSpace()
	if p.pos != len(p.data) {
		return JSONB{}, errTrailingJSON
	}
	initCap := len(data)
	if budget > 0 && int64(initCap) > budget {
		initCap = int(budget)
	}
	out := jsonWriter{buf: make([]byte, 0, initCap), left: budget}
	if err := appendJSON(&out, v); err != nil {
		return JSONB{}, err
	}
	return JSONB{data: out.buf}, nil
}

type jsonParser struct {
	data  []byte
	pos   int
	depth int
}

func (p *jsonParser) skipSpace() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\r', '\n':
			p.pos++
		default:
			return
		}
	}
}

func (p *jsonParser) parseValue() (any, error) {
	if p.pos >= len(p.data) {
		return nil, errInvalidJSON
	}
	switch p.data[p.pos] {
	case '{':
		return p.parseObject()
	case '[':
		return p.parseArray()
	case '"':
		return p.parseString()
	case 't':
		if p.pos+4 <= len(p.data) && string(p.data[p.pos:p.pos+4]) == "true" {
			p.pos += 4
			return true, nil
		}
		return nil, errInvalidJSON
	case 'f':
		if p.pos+5 <= len(p.data) && string(p.data[p.pos:p.pos+5]) == "false" {
			p.pos += 5
			return false, nil
		}
		return nil, errInvalidJSON
	case 'n':
		if p.pos+4 <= len(p.data) && string(p.data[p.pos:p.pos+4]) == "null" {
			p.pos += 4
			return nil, nil
		}
		return nil, errInvalidJSON
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return p.parseNumber()
	default:
		return nil, errInvalidJSON
	}
}

func (p *jsonParser) parseObject() (map[string]any, error) {
	p.depth++
	if p.depth > maxJSONDepth {
		return nil, errInvalidJSON
	}
	p.pos++ // '{'
	p.skipSpace()
	obj := make(map[string]any)
	if p.pos < len(p.data) && p.data[p.pos] == '}' {
		p.pos++
		p.depth--
		return obj, nil
	}
	for {
		if p.pos >= len(p.data) || p.data[p.pos] != '"' {
			return nil, errInvalidJSON
		}
		key, err := p.parseString()
		if err != nil {
			return nil, err
		}
		p.skipSpace()
		if p.pos >= len(p.data) || p.data[p.pos] != ':' {
			return nil, errInvalidJSON
		}
		p.pos++
		p.skipSpace()
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		obj[key] = val
		p.skipSpace()
		if p.pos >= len(p.data) {
			return nil, errInvalidJSON
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
			p.skipSpace()
		case '}':
			p.pos++
			p.depth--
			return obj, nil
		default:
			return nil, errInvalidJSON
		}
	}
}

func (p *jsonParser) parseArray() ([]any, error) {
	p.depth++
	if p.depth > maxJSONDepth {
		return nil, errInvalidJSON
	}
	p.pos++ // '['
	p.skipSpace()
	if p.pos < len(p.data) && p.data[p.pos] == ']' {
		p.pos++
		p.depth--
		return []any{}, nil
	}
	arr := make([]any, 0, 4)
	for {
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		arr = append(arr, val)
		p.skipSpace()
		if p.pos >= len(p.data) {
			return nil, errInvalidJSON
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
			p.skipSpace()
		case ']':
			p.pos++
			p.depth--
			return arr, nil
		default:
			return nil, errInvalidJSON
		}
	}
}

// parseString parses a JSON string in a single pass, rejecting invalid
// surrogate escapes and escaped/unescaped NUL bytes (which PostgreSQL jsonb
// rejects even inside discarded duplicate keys).
func (p *jsonParser) parseString() (string, error) {
	p.pos++ // opening '"'
	start := p.pos
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		if c == '"' {
			s := string(p.data[start:p.pos])
			p.pos++
			return s, nil
		}
		if c == '\\' {
			break
		}
		if c < 0x20 {
			return "", errInvalidJSON
		}
		p.pos++
	}
	if p.pos >= len(p.data) {
		return "", errInvalidJSON
	}
	buf := make([]byte, 0, (p.pos-start)+16)
	buf = append(buf, p.data[start:p.pos]...)
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		if c == '"' {
			p.pos++
			return string(buf), nil
		}
		if c < 0x20 {
			return "", errInvalidJSON
		}
		if c != '\\' {
			buf = append(buf, c)
			p.pos++
			continue
		}
		p.pos++
		if p.pos >= len(p.data) {
			return "", errInvalidJSON
		}
		esc := p.data[p.pos]
		p.pos++
		switch esc {
		case '"', '\\', '/':
			buf = append(buf, esc)
		case 'b':
			buf = append(buf, '\b')
		case 'f':
			buf = append(buf, '\f')
		case 'n':
			buf = append(buf, '\n')
		case 'r':
			buf = append(buf, '\r')
		case 't':
			buf = append(buf, '\t')
		case 'u':
			u, ok := parseHex4(p.data, p.pos)
			if !ok || u == 0 || (u >= 0xdc00 && u <= 0xdfff) {
				return "", errInvalidUTF8
			}
			p.pos += 4
			r := rune(u)
			if u >= 0xd800 && u <= 0xdbff {
				if p.pos+6 > len(p.data) || p.data[p.pos] != '\\' || p.data[p.pos+1] != 'u' {
					return "", errInvalidUTF8
				}
				low, ok := parseHex4(p.data, p.pos+2)
				if !ok || low < 0xdc00 || low > 0xdfff {
					return "", errInvalidUTF8
				}
				p.pos += 6
				r = utf16.DecodeRune(rune(u), rune(low))
			}
			buf = utf8.AppendRune(buf, r)
		default:
			return "", errInvalidJSON
		}
	}
	return "", errInvalidJSON
}

func parseHex4(data []byte, pos int) (uint16, bool) {
	if pos+4 > len(data) {
		return 0, false
	}
	var out uint16
	for i := range 4 {
		c := data[pos+i]
		var nib uint16
		switch {
		case c >= '0' && c <= '9':
			nib = uint16(c - '0')
		case c >= 'a' && c <= 'f':
			nib = uint16(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			nib = uint16(c - 'A' + 10)
		default:
			return 0, false
		}
		out = (out << 4) | nib
	}
	return out, true
}

func (p *jsonParser) parseNumber() (decimalNumber, error) {
	start := p.pos
	if p.pos < len(p.data) && p.data[p.pos] == '-' {
		p.pos++
	}
	if p.pos >= len(p.data) {
		return decimalNumber{}, errInvalidJSON
	}
	if p.data[p.pos] == '0' {
		p.pos++
	} else if p.data[p.pos] >= '1' && p.data[p.pos] <= '9' {
		p.pos++
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
		}
	} else {
		return decimalNumber{}, errInvalidJSON
	}
	if p.pos < len(p.data) && p.data[p.pos] == '.' {
		p.pos++
		if p.pos >= len(p.data) || p.data[p.pos] < '0' || p.data[p.pos] > '9' {
			return decimalNumber{}, errInvalidJSON
		}
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
		}
	}
	if p.pos < len(p.data) && (p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		p.pos++
		if p.pos < len(p.data) && (p.data[p.pos] == '+' || p.data[p.pos] == '-') {
			p.pos++
		}
		if p.pos >= len(p.data) || p.data[p.pos] < '0' || p.data[p.pos] > '9' {
			return decimalNumber{}, errInvalidJSON
		}
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
		}
	}
	return newDecimal(string(p.data[start:p.pos]))
}

// jsonWriter charges every byte against the remaining output budget before it
// is allocated or appended.
type jsonWriter struct {
	buf  []byte
	left int64
}

func (w *jsonWriter) reserve(n int64) error {
	if n > w.left {
		return errJSONBTooLarge
	}
	w.left -= n
	return nil
}

func (w *jsonWriter) writeByte(c byte) error {
	if err := w.reserve(1); err != nil {
		return err
	}
	w.buf = append(w.buf, c)
	return nil
}

const hexDigits = "0123456789abcdef"

func appendJSONString(out *jsonWriter, s string) error {
	// Fast path: check whether string is pure safe ASCII requiring no escaping.
	safe := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c >= 0x80 || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			if c == 0 {
				return errors.New("database: JSON NUL")
			}
			safe = false
			break
		}
	}
	if safe {
		if err := out.reserve(int64(len(s)) + 2); err != nil {
			return err
		}
		out.buf = append(out.buf, '"')
		out.buf = append(out.buf, s...)
		out.buf = append(out.buf, '"')
		return nil
	}
	if strings.IndexByte(s, 0) >= 0 {
		return errors.New("database: JSON NUL")
	}
	// Compute exact canonical escaped size before charging and emitting,
	// matching encoding/json escaping rules (including HTML and U+2028/U+2029).
	size := int64(2)
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"', '\\', '\b', '\f', '\n', '\r', '\t':
				size += 2
			case '<', '>', '&':
				size += 6
			default:
				if c < 0x20 {
					size += 6
				} else {
					size++
				}
			}
			i++
			continue
		}
		r, width := utf8.DecodeRuneInString(s[i:])
		if r == '\u2028' || r == '\u2029' {
			size += 6
		} else {
			size += int64(width)
		}
		i += width
	}
	if err := out.reserve(size); err != nil {
		return err
	}
	out.buf = append(out.buf, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' && c != '<' && c != '>' && c != '&' {
				i++
				continue
			}
			out.buf = append(out.buf, s[start:i]...)
			switch c {
			case '"':
				out.buf = append(out.buf, '\\', '"')
			case '\\':
				out.buf = append(out.buf, '\\', '\\')
			case '\b':
				out.buf = append(out.buf, '\\', 'b')
			case '\f':
				out.buf = append(out.buf, '\\', 'f')
			case '\n':
				out.buf = append(out.buf, '\\', 'n')
			case '\r':
				out.buf = append(out.buf, '\\', 'r')
			case '\t':
				out.buf = append(out.buf, '\\', 't')
			default:
				out.buf = append(out.buf, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			}
			i++
			start = i
			continue
		}
		r, width := utf8.DecodeRuneInString(s[i:])
		if r == '\u2028' || r == '\u2029' {
			out.buf = append(out.buf, s[start:i]...)
			if r == '\u2028' {
				out.buf = append(out.buf, `\u2028`...)
			} else {
				out.buf = append(out.buf, `\u2029`...)
			}
			i += width
			start = i
			continue
		}
		i += width
	}
	out.buf = append(out.buf, s[start:]...)
	out.buf = append(out.buf, '"')
	return nil
}

func appendJSON(out *jsonWriter, v any) error {
	switch v := v.(type) {
	case nil:
		if err := out.reserve(4); err != nil {
			return err
		}
		out.buf = append(out.buf, "null"...)
	case bool:
		if v {
			if err := out.reserve(4); err != nil {
				return err
			}
			out.buf = append(out.buf, "true"...)
		} else {
			if err := out.reserve(5); err != nil {
				return err
			}
			out.buf = append(out.buf, "false"...)
		}
	case string:
		return appendJSONString(out, v)
	case decimalNumber:
		if err := out.reserve(v.size); err != nil {
			return err
		}
		out.buf = v.appendTo(out.buf)
	case []any:
		if err := out.writeByte('['); err != nil {
			return err
		}
		for i, item := range v {
			if i > 0 {
				if err := out.writeByte(','); err != nil {
					return err
				}
			}
			if err := appendJSON(out, item); err != nil {
				return err
			}
		}
		return out.writeByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		slices.SortFunc(keys, func(a, b string) int {
			if c := cmp.Compare(len(a), len(b)); c != 0 {
				return c
			}
			return cmp.Compare(a, b)
		})
		if err := out.writeByte('{'); err != nil {
			return err
		}
		for i, key := range keys {
			if i > 0 {
				if err := out.writeByte(','); err != nil {
					return err
				}
			}
			if err := appendJSONString(out, key); err != nil {
				return err
			}
			if err := out.writeByte(':'); err != nil {
				return err
			}
			if err := appendJSON(out, v[key]); err != nil {
				return err
			}
		}
		return out.writeByte('}')
	default:
		return errors.New("database: invalid JSON value")
	}
	return nil
}

// decimalNumber is a validated number whose canonical text has not been built;
// size is its exact length, known without expanding any exponent.
type decimalNumber struct {
	negative             bool
	digits               string
	integerDigits, scale int64
	leading              int
	zero                 bool
	size                 int64
}

func newDecimal(input string) (decimalNumber, error) {
	n := decimalNumber{negative: strings.HasPrefix(input, "-")}
	if n.negative {
		input = input[1:]
	}
	exponent := int64(0)
	if i := strings.IndexAny(input, "eE"); i >= 0 {
		var err error
		exponent, err = strconv.ParseInt(input[i+1:], 10, 32)
		if err != nil || exponent > 1073741823 || exponent < -1073741823 {
			return n, errors.New("database: JSON numeric out of range")
		}
		input = input[:i]
	}
	fraction := int64(0)
	if dot := strings.IndexByte(input, '.'); dot >= 0 {
		fraction = int64(len(input) - dot - 1)
		var b strings.Builder
		b.Grow(len(input) - 1)
		b.WriteString(input[:dot])
		b.WriteString(input[dot+1:])
		n.digits = b.String()
	} else {
		n.digits = input
	}
	n.scale = fraction - exponent
	if n.scale < 0 {
		n.scale = 0
	}
	n.integerDigits = int64(len(n.digits)) - fraction + exponent
	leading := 0
	for leading < len(n.digits) && n.digits[leading] == '0' {
		leading++
	}
	n.leading = leading
	if n.scale > 16383 {
		return n, errors.New("database: JSON numeric out of range")
	}
	if n.leading == len(n.digits) {
		n.zero = true
		n.size = 1
		if n.scale > 0 {
			n.size = 2 + n.scale
		}
		return n, nil
	}
	if n.integerDigits-int64(n.leading) > 131072 {
		return n, errors.New("database: JSON numeric out of range")
	}
	integerLen := int64(1)
	if n.integerDigits > 0 {
		if kept := n.integerDigits - min(int64(n.leading), n.integerDigits); kept > 0 {
			integerLen = kept
		}
	}
	n.size = integerLen
	if n.scale > 0 {
		n.size += 1 + n.scale
	}
	if n.negative {
		n.size++
	}
	return n, nil
}

func appendZeros(dst []byte, count int) []byte {
	for range count {
		dst = append(dst, '0')
	}
	return dst
}

func (n decimalNumber) appendTo(dst []byte) []byte {
	if n.zero {
		dst = append(dst, '0')
		if n.scale > 0 {
			dst = append(dst, '.')
			dst = appendZeros(dst, int(n.scale))
		}
		return dst
	}
	if n.negative {
		dst = append(dst, '-')
	}
	if n.integerDigits <= 0 {
		dst = append(dst, '0')
		if n.scale > 0 {
			dst = append(dst, '.')
			dst = appendZeros(dst, int(-n.integerDigits))
			dst = append(dst, n.digits...)
		}
		return dst
	}
	if n.integerDigits >= int64(len(n.digits)) {
		// All digits (plus trailing zeros if any) are in the integer part.
		if n.leading < len(n.digits) {
			dst = append(dst, n.digits[n.leading:]...)
		} else {
			dst = append(dst, '0')
		}
		dst = appendZeros(dst, int(n.integerDigits)-len(n.digits))
		if n.scale > 0 {
			dst = append(dst, '.')
			dst = appendZeros(dst, int(n.scale))
		}
		return dst
	}
	// integerDigits is strictly between 1 and len(n.digits)-1.
	intPart := n.digits[:n.integerDigits]
	trim := 0
	for trim < len(intPart) && intPart[trim] == '0' {
		trim++
	}
	if trim == len(intPart) {
		dst = append(dst, '0')
	} else {
		dst = append(dst, intPart[trim:]...)
	}
	if n.scale > 0 {
		dst = append(dst, '.')
		dst = append(dst, n.digits[n.integerDigits:]...)
	}
	return dst
}

func (n decimalNumber) String() string {
	return string(n.appendTo(make([]byte, 0, n.size)))
}
