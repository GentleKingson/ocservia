package mysql

import (
	"database/sql/driver"
	"errors"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
)

type logicalArgument struct{ value any }

func (a logicalArgument) Value() (driver.Value, error) {
	switch v := a.value.(type) {
	case value.Timestamp:
		if err := v.Validate(); err != nil {
			return nil, err
		}
		if !v.Valid {
			return nil, nil
		}
		return v.Micros, nil
	case value.JSONB:
		return v.Bytes(), nil
	case value.TextArray:
		return v.Bytes()
	}
	return nil, errors.New("database: invalid logical argument")
}

func logicalArguments(args []any) []any {
	result := append([]any(nil), args...)
	for i, arg := range result {
		switch arg.(type) {
		case value.Timestamp, value.JSONB, value.TextArray:
			result[i] = logicalArgument{arg}
		}
	}
	return result
}

type logicalDestination struct{ value any }

func (d logicalDestination) Scan(src any) error {
	if t, ok := d.value.(*value.Timestamp); ok {
		if src == nil {
			*t = value.Timestamp{}
			return nil
		}
		n, ok := src.(int64)
		if !ok {
			return errors.New("database: expected BIGINT timestamp")
		}
		next := value.Timestamp{Micros: n, Valid: true}
		if err := next.Validate(); err != nil {
			return err
		}
		*t = next
		return nil
	}
	var data []byte
	switch src := src.(type) {
	case nil:
	case []byte:
		data = src
	case string:
		data = []byte(src)
	default:
		return errors.New("database: expected binary logical value")
	}
	switch dst := d.value.(type) {
	case *value.JSONB:
		next, err := value.ParseJSONB(data)
		if err == nil {
			*dst = next
		}
		return err
	case *value.TextArray:
		next, err := value.ParseTextArray(data)
		if err == nil {
			*dst = next
		}
		return err
	}
	return errors.New("database: invalid logical destination")
}

func logicalDestinations(dest []any) []any {
	result := append([]any(nil), dest...)
	for i, dst := range result {
		switch dst.(type) {
		case *value.Timestamp, *value.JSONB, *value.TextArray:
			result[i] = logicalDestination{dst}
		}
	}
	return result
}
