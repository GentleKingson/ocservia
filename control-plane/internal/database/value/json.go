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

func ParseJSONB(data []byte) (JSONB, error) {
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
			if _, err := decimal(string(n)); err != nil {
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
	var out bytes.Buffer
	if err := appendJSON(&out, v); err != nil {
		return JSONB{}, err
	}
	return JSONB{data: out.Bytes()}, nil
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

func appendJSON(out *bytes.Buffer, v any) error {
	switch v := v.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		out.WriteString(strconv.FormatBool(v))
	case string:
		if strings.IndexByte(v, 0) >= 0 {
			return errors.New("database: JSON NUL")
		}
		encoded, _ := json.Marshal(v)
		out.Write(encoded)
	case json.Number:
		number, err := decimal(string(v))
		if err != nil {
			return err
		}
		out.WriteString(number)
	case []any:
		out.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := appendJSON(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
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
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := appendJSON(out, key); err != nil {
				return err
			}
			out.WriteByte(':')
			if err := appendJSON(out, v[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return errors.New("database: invalid JSON value")
	}
	return nil
}

func decimal(input string) (string, error) {
	negative := strings.HasPrefix(input, "-")
	input = strings.TrimPrefix(input, "-")
	exponent := int64(0)
	if i := strings.IndexAny(input, "eE"); i >= 0 {
		var err error
		exponent, err = strconv.ParseInt(input[i+1:], 10, 32)
		if err != nil || exponent > 1073741823 || exponent < -1073741823 {
			return "", errors.New("database: JSON numeric out of range")
		}
		input = input[:i]
	}
	digits := strings.ReplaceAll(input, ".", "")
	fraction := int64(0)
	if i := strings.IndexByte(input, '.'); i >= 0 {
		fraction = int64(len(input) - i - 1)
	}
	scale := fraction - exponent
	if scale < 0 {
		scale = 0
	}
	integerDigits := int64(len(digits)) - fraction + exponent
	leading := len(digits) - len(strings.TrimLeft(digits, "0"))
	if scale > 16383 {
		return "", errors.New("database: JSON numeric out of range")
	}
	if leading == len(digits) {
		if scale == 0 {
			return "0", nil
		}
		return "0." + strings.Repeat("0", int(scale)), nil
	}
	if integerDigits-int64(leading) > 131072 {
		return "", errors.New("database: JSON numeric out of range")
	}
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
	if scale > 0 {
		result += "." + digits[integerDigits:]
	}
	if negative && strings.Trim(digits, "0") != "" {
		result = "-" + result
	}
	return result, nil
}
