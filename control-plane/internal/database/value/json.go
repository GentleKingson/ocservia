package value

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
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
const maxJSONBBytes = 8 << 20

var errJSONBTooLarge = errors.New("database: JSON canonical output too large")

func ParseJSONB(data []byte) (JSONB, error) { return parseJSONB(data, maxJSONBBytes) }

func parseJSONB(data []byte, budget int64) (JSONB, error) {
	if data == nil {
		return JSONB{}, nil
	}
	if !utf8.Valid(data) || !validEscapes(data) {
		return JSONB{}, errors.New("database: invalid JSON Unicode")
	}
	check := json.NewDecoder(bytes.NewReader(data))
	check.UseNumber()
	for {
		token, err := check.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return JSONB{}, errors.New("database: invalid JSON")
		}
		if n, ok := token.(json.Number); ok {
			if _, err := newDecimal(string(n)); err != nil {
				return JSONB{}, err
			}
		}
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return JSONB{}, errors.New("database: invalid JSON")
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return JSONB{}, errors.New("database: trailing JSON input")
	}
	out := jsonWriter{left: budget}
	if err := appendJSON(&out, v); err != nil {
		return JSONB{}, err
	}
	return JSONB{data: out.buf.Bytes()}, nil
}

// encoding/json intentionally replaces invalid surrogate escapes. PostgreSQL
// jsonb rejects them, including escaped NUL anywhere in a discarded duplicate.
func validEscapes(data []byte) bool {
	inString := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			return false
		}
		if data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return false
		}
		u, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil || u == 0 || u >= 0xdc00 && u <= 0xdfff {
			return false
		}
		i += 4
		if u >= 0xd800 && u <= 0xdbff {
			if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}

// jsonWriter charges every byte against the remaining output budget before it
// is allocated or appended.
type jsonWriter struct {
	buf  bytes.Buffer
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
	return w.buf.WriteByte(c)
}

func appendJSON(out *jsonWriter, v any) error {
	switch v := v.(type) {
	case nil:
		if err := out.reserve(4); err != nil {
			return err
		}
		out.buf.WriteString("null")
	case bool:
		text := strconv.FormatBool(v)
		if err := out.reserve(int64(len(text))); err != nil {
			return err
		}
		out.buf.WriteString(text)
	case string:
		if strings.IndexByte(v, 0) >= 0 {
			return errors.New("database: JSON NUL")
		}
		// Escaping is at least len(v)+2 and at most six bytes per input byte of
		// a string that is already in memory, so only the lower bound can gate
		// the allocation; the exact size is charged right after.
		if int64(len(v))+2 > out.left {
			return errJSONBTooLarge
		}
		encoded, _ := json.Marshal(v)
		if err := out.reserve(int64(len(encoded))); err != nil {
			return err
		}
		out.buf.Write(encoded)
	case json.Number:
		number, err := newDecimal(string(v))
		if err != nil {
			return err
		}
		if err := out.reserve(number.size); err != nil {
			return err
		}
		out.buf.WriteString(number.String())
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
		sort.Slice(keys, func(i, j int) bool {
			if len(keys[i]) != len(keys[j]) {
				return len(keys[i]) < len(keys[j])
			}
			return keys[i] < keys[j]
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
			if err := appendJSON(out, key); err != nil {
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
	input = strings.TrimPrefix(input, "-")
	exponent := int64(0)
	if i := strings.IndexAny(input, "eE"); i >= 0 {
		var err error
		exponent, err = strconv.ParseInt(input[i+1:], 10, 32)
		if err != nil || exponent > 1073741823 || exponent < -1073741823 {
			return n, errors.New("database: JSON numeric out of range")
		}
		input = input[:i]
	}
	n.digits = strings.ReplaceAll(input, ".", "")
	fraction := int64(0)
	if i := strings.IndexByte(input, '.'); i >= 0 {
		fraction = int64(len(input) - i - 1)
	}
	n.scale = fraction - exponent
	if n.scale < 0 {
		n.scale = 0
	}
	n.integerDigits = int64(len(n.digits)) - fraction + exponent
	n.leading = len(n.digits) - len(strings.TrimLeft(n.digits, "0"))
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

func (n decimalNumber) String() string {
	if n.zero {
		if n.scale == 0 {
			return "0"
		}
		return "0." + strings.Repeat("0", int(n.scale))
	}
	digits, integerDigits := n.digits, n.integerDigits
	if integerDigits <= 0 {
		digits = strings.Repeat("0", int(1-integerDigits)) + digits
		integerDigits = 1
	}
	if integerDigits > int64(len(digits)) {
		digits += strings.Repeat("0", int(integerDigits)-len(digits))
	}
	integer := strings.TrimLeft(digits[:integerDigits], "0")
	if integer == "" {
		integer = "0"
	}
	result := integer
	if n.scale > 0 {
		result += "." + digits[integerDigits:]
	}
	if n.negative {
		result = "-" + result
	}
	return result
}
