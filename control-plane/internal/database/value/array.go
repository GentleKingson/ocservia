package value

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"unicode/utf8"
)

type Dimension struct {
	Length     int32 `json:"length"`
	LowerBound int32 `json:"lower_bound"`
}

// PostgreSQL's MaxArraySize on the supported 64-bit servers is MaxAllocSize /
// sizeof(Datum). The table-specific 4096 member CHECK remains separate.
const MaxArrayElements = 134217727

// TextArray preserves dimensionality, lower bounds and NULL elements. An
// invalid value is SQL NULL; a valid zero-dimensional array is PostgreSQL '{}'.
type TextArray struct {
	Dimensions []Dimension `json:"dimensions"`
	Elements   []*string   `json:"elements"`
	Valid      bool        `json:"-"`
}

func (a TextArray) Validate() error {
	if !a.Valid {
		return nil
	}
	if len(a.Dimensions) > 6 {
		return errors.New("database: too many array dimensions")
	}
	n := int64(0)
	if len(a.Dimensions) > 0 {
		n = 1
	}
	for _, d := range a.Dimensions {
		if d.Length <= 0 || int64(d.LowerBound)+int64(d.Length) > math.MaxInt32 || n > MaxArrayElements/int64(d.Length) {
			return errors.New("database: invalid array dimensions")
		}
		n *= int64(d.Length)
	}
	if n != int64(len(a.Elements)) {
		return errors.New("database: array cardinality mismatch")
	}
	for _, v := range a.Elements {
		if v != nil && (!utf8.ValidString(*v) || strings.IndexByte(*v, 0) >= 0) {
			return errors.New("database: invalid array text")
		}
	}
	return nil
}

func (a TextArray) Bytes() ([]byte, error) {
	if !a.Valid {
		return nil, nil
	}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	if a.Dimensions == nil {
		a.Dimensions = []Dimension{}
	}
	if a.Elements == nil {
		a.Elements = []*string{}
	}
	return json.Marshal(a)
}

func ParseTextArray(data []byte) (TextArray, error) {
	if data == nil {
		return TextArray{}, nil
	}
	if !utf8.Valid(data) || !validEscapes(data) {
		return TextArray{}, errors.New("database: invalid array Unicode")
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil || len(envelope) != 2 || len(envelope["dimensions"]) == 0 || envelope["dimensions"][0] != '[' || len(envelope["elements"]) == 0 || envelope["elements"][0] != '[' {
		return TextArray{}, errors.New("database: invalid array representation")
	}
	var dimensions []map[string]json.RawMessage
	if err := json.Unmarshal(envelope["dimensions"], &dimensions); err != nil {
		return TextArray{}, errors.New("database: invalid array dimensions")
	}
	for _, d := range dimensions {
		if len(d) != 2 || d["length"] == nil || d["lower_bound"] == nil {
			return TextArray{}, errors.New("database: incomplete array dimensions")
		}
	}
	var a TextArray
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if len(data) == 0 || data[0] != '{' {
		return TextArray{}, errors.New("database: invalid array representation")
	}
	if err := d.Decode(&a); err != nil {
		return TextArray{}, errors.New("database: invalid array representation")
	}
	if d.Decode(new(any)) != io.EOF {
		return TextArray{}, errors.New("database: trailing array representation")
	}
	a.Valid = true
	return a, a.Validate()
}

func TextList(values []string) TextArray {
	if values == nil {
		return TextArray{}
	}
	a := TextArray{Valid: true, Elements: make([]*string, len(values))}
	if len(values) > 0 {
		a.Dimensions = []Dimension{{Length: int32(len(values)), LowerBound: 1}}
	}
	for i := range values {
		v := values[i]
		a.Elements[i] = &v
	}
	return a
}
