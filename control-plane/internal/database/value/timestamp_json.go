package value

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// MarshalJSON preserves RFC 3339 for ordinary years. Outside 0000..9999 it
// uses an ISO 8601 signed six-digit year; infinities are explicit strings.
func (t Timestamp) MarshalJSON() ([]byte, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	if !t.Valid {
		return []byte("null"), nil
	}
	switch t.Micros {
	case NegativeInfinity:
		return []byte(`"-infinity"`), nil
	case PositiveInfinity:
		return []byte(`"infinity"`), nil
	}
	finite, err := t.Time()
	if err != nil {
		return nil, err
	}
	if finite.Year() >= 0 && finite.Year() <= 9999 {
		return finite.MarshalJSON()
	}
	return json.Marshal(fmt.Sprintf("%+07d", finite.Year()) + finite.Format("-01-02T15:04:05.999999999Z07:00"))
}

func (t *Timestamp) UnmarshalJSON(data []byte) error {
	var text *string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	if text == nil {
		*t = Timestamp{}
		return nil
	}
	next, err := ParseTimestamp(*text)
	if err != nil {
		return err
	}
	*t = next
	return nil
}

// ParseTimestamp accepts the same non-NULL text domain emitted by MarshalJSON.
func ParseTimestamp(text string) (Timestamp, error) {
	switch text {
	case "-infinity":
		return Timestamp{Micros: NegativeInfinity, Valid: true}, nil
	case "infinity":
		return Timestamp{Micros: PositiveInfinity, Valid: true}, nil
	}
	if len(text) > 7 && (text[0] == '+' || text[0] == '-') && text[7] == '-' {
		for _, c := range text[1:7] {
			if c < '0' || c > '9' {
				return Timestamp{}, errors.New("database: invalid extended timestamp year")
			}
		}
		year, err := strconv.Atoi(text[:7])
		if err != nil {
			return Timestamp{}, err
		}
		// A congruent year preserves Gregorian leap-day validation in Parse.
		calendarYear := 2000 + (year%400+400)%400
		parsed, err := time.Parse(time.RFC3339Nano, fmt.Sprintf("%04d", calendarYear)+text[7:])
		if err != nil {
			return Timestamp{}, err
		}
		_, offset := parsed.Zone()
		return FromTime(time.Date(year, parsed.Month(), parsed.Day(), parsed.Hour(), parsed.Minute(), parsed.Second(), parsed.Nanosecond(), time.FixedZone("", offset)))
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return Timestamp{}, err
	}
	return FromTime(parsed)
}
