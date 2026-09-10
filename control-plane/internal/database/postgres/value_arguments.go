package postgres

import (
	"context"
	"database/sql/driver"
	"encoding/json"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/jackc/pgx/v5/pgtype"
)

func (t *transaction) TransactionTime(ctx context.Context) (value.Timestamp, error) {
	var now value.Timestamp
	err := t.QueryRow(ctx, "SELECT transaction_timestamp()").Scan(&now)
	return now, err
}

type invalidLogicalArgument struct{ err error }

func (a invalidLogicalArgument) Value() (driver.Value, error) { return nil, a.err }

func logicalArguments(args []any) []any {
	result := append([]any(nil), args...)
	for i, arg := range result {
		switch v := arg.(type) {
		case value.JSONB:
			if !v.Valid() {
				result[i] = nil
			} else {
				result[i] = json.RawMessage(v.Bytes())
			}
		case value.Timestamp:
			if err := v.Validate(); err != nil {
				result[i] = invalidLogicalArgument{err}
				continue
			}
			p := pgtype.Timestamptz{Valid: v.Valid}
			switch v.Micros {
			case value.NegativeInfinity:
				p.InfinityModifier = pgtype.NegativeInfinity
			case value.PositiveInfinity:
				p.InfinityModifier = pgtype.Infinity
			default:
				if v.Valid {
					p.Time, _ = v.Time()
				}
			}
			result[i] = p
		case value.TextArray:
			if err := v.Validate(); err != nil {
				result[i] = invalidLogicalArgument{err}
				continue
			}
			p := pgtype.Array[pgtype.Text]{Valid: v.Valid}
			if v.Valid {
				p.Dims = make([]pgtype.ArrayDimension, len(v.Dimensions))
				p.Elements = make([]pgtype.Text, len(v.Elements))
			}
			for j, d := range v.Dimensions {
				p.Dims[j] = pgtype.ArrayDimension{Length: d.Length, LowerBound: d.LowerBound}
			}
			for j, s := range v.Elements {
				if s != nil {
					p.Elements[j] = pgtype.Text{String: *s, Valid: true}
				}
			}
			result[i] = p
		}
	}
	return result
}

// Decode using pgx's native codecs, then publish a logical value only after the
// entire row scanned successfully. No textual array flattening or time sentinel.
func scanLogical(scan func(...any) error, dest []any) error {
	args := append([]any(nil), dest...)
	finish := make([]func() error, 0)
	for i, dst := range dest {
		switch dst := dst.(type) {
		case *value.Timestamp:
			p := new(pgtype.Timestamptz)
			args[i] = p
			finish = append(finish, func() error {
				v := value.Timestamp{Valid: p.Valid}
				if p.Valid {
					switch p.InfinityModifier {
					case pgtype.NegativeInfinity:
						v.Micros = value.NegativeInfinity
					case pgtype.Infinity:
						v.Micros = value.PositiveInfinity
					default:
						var err error
						v, err = value.FromTime(p.Time)
						if err != nil {
							return err
						}
					}
				}
				*dst = v
				return nil
			})
		case *value.JSONB:
			p := new([]byte)
			args[i] = p
			finish = append(finish, func() error {
				v, err := value.ParseJSONB(*p)
				if err == nil {
					*dst = v
				}
				return err
			})
		case *value.TextArray:
			p := new(pgtype.Array[pgtype.Text])
			args[i] = p
			finish = append(finish, func() error {
				v := value.TextArray{Valid: p.Valid}
				if p.Valid {
					v.Dimensions = make([]value.Dimension, len(p.Dims))
					v.Elements = make([]*string, len(p.Elements))
				}
				for j, d := range p.Dims {
					v.Dimensions[j] = value.Dimension{Length: d.Length, LowerBound: d.LowerBound}
				}
				for j, s := range p.Elements {
					if s.Valid {
						text := s.String
						v.Elements[j] = &text
					}
				}
				if err := v.Validate(); err != nil {
					return err
				}
				*dst = v
				return nil
			})
		}
	}
	if err := scan(args...); err != nil {
		return err
	}
	for _, f := range finish {
		if err := f(); err != nil {
			return err
		}
	}
	return nil
}
