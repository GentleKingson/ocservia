// Package value contains lossless logical values shared by database stores.
package value

import (
	"errors"
	"math"
	"time"
)

const (
	MinTimestamp     int64 = -211813488000000000
	EndTimestamp     int64 = 9223371331200000000
	NegativeInfinity int64 = math.MinInt64
	PositiveInfinity int64 = math.MaxInt64
	unixEpochOffset  int64 = 946684800
)

// Timestamp uses PostgreSQL's signed microseconds since 2000-01-01 UTC,
// including its infinity encodings. The zero value is SQL NULL, not year zero.
type Timestamp struct {
	Micros int64
	Valid  bool
}

func (t Timestamp) Validate() error {
	if t.Valid && t.Micros != NegativeInfinity && t.Micros != PositiveInfinity && (t.Micros < MinTimestamp || t.Micros >= EndTimestamp) {
		return errors.New("database: timestamp out of range")
	}
	return nil
}

// FromTime follows pgx's binary timestamptz precision: sub-microseconds are
// truncated, not rounded. Sub/UnixNano cannot represent the PostgreSQL range.
func FromTime(t time.Time) (Timestamp, error) {
	seconds := t.Unix() - unixEpochOffset
	if t.Year() < -4713 || t.Year() > 294277 || seconds < MinTimestamp/1000000 || seconds > EndTimestamp/1000000 {
		return Timestamp{}, errors.New("database: timestamp out of range")
	}
	v := Timestamp{Micros: seconds*1000000 + int64(t.Nanosecond()/1000), Valid: true}
	return v, v.Validate()
}

// Time fails for NULL and infinities instead of replacing them with a sentinel.
func (t Timestamp) Time() (time.Time, error) {
	if err := t.Validate(); err != nil {
		return time.Time{}, err
	}
	if !t.Valid || t.Micros == NegativeInfinity || t.Micros == PositiveInfinity {
		return time.Time{}, errors.New("database: non-finite timestamp")
	}
	return time.Unix(t.Micros/1000000+unixEpochOffset, (t.Micros%1000000)*1000).UTC(), nil
}

// Add preserves NULL and infinity and refuses finite arithmetic overflow.
func (t Timestamp) Add(duration time.Duration) (Timestamp, error) {
	if err := t.Validate(); err != nil {
		return Timestamp{}, err
	}
	if !t.Valid || t.Micros == NegativeInfinity || t.Micros == PositiveInfinity {
		return t, nil
	}
	delta := duration.Microseconds()
	if (delta > 0 && t.Micros > EndTimestamp-1-delta) || (delta < 0 && t.Micros < MinTimestamp-delta) {
		return Timestamp{}, errors.New("database: timestamp out of range")
	}
	t.Micros += delta
	return t, nil
}
