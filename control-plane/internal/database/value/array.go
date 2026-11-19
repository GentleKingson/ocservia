package value

import (
	"errors"
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
