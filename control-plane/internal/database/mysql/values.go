package mysql

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/netip"

	"github.com/google/uuid"
)

// UUIDBytes uses RFC byte order, without UUID_TO_BIN's optional time swapping.
func UUIDBytes(id uuid.UUID) []byte { return append([]byte(nil), id[:]...) }

// InetBytes stores family, prefix length and the original host address (not a
// masked network). SQL NULL is distinct from every IPv4/IPv6 prefix.
func InetBytes(prefix netip.Prefix) ([]byte, error) {
	if !prefix.IsValid() {
		return nil, errors.New("database: invalid inet value")
	}
	address := prefix.Addr()
	family := byte(6)
	if address.Is4() {
		family = 4
	}
	return append([]byte{family, byte(prefix.Bits())}, address.AsSlice()...), nil
}
func ParseInet(value []byte) (netip.Prefix, error) {
	if len(value) != 6 && len(value) != 18 {
		return netip.Prefix{}, errors.New("database: invalid inet encoding")
	}
	address, ok := netip.AddrFromSlice(value[2:])
	bits := int(value[1])
	if !ok || (value[0] != 4 || len(value) != 6 || bits > 32) && (value[0] != 6 || len(value) != 18 || bits > 128) {
		return netip.Prefix{}, errors.New("database: invalid inet encoding")
	}
	return netip.PrefixFrom(address, bits), nil
}

// TextArray is the one-dimensional member-list contract. A nil array is SQL
// NULL; an empty non-nil array is []; nil elements remain JSON null. PostgreSQL
// multidimensional arrays/lower bounds are not implicitly flattened.
type TextArray []*string

func (a TextArray) Value() (driver.Value, error) {
	if a == nil {
		return nil, nil
	}
	data, err := json.Marshal(a)
	if err != nil {
		return nil, errors.New("database: invalid text array")
	}
	return data, nil
}
func (a *TextArray) Scan(src any) error {
	if src == nil {
		*a = nil
		return nil
	}
	var data []byte
	switch value := src.(type) {
	case []byte:
		data = value
	case string:
		data = []byte(value)
	default:
		return errors.New("database: invalid text array encoding")
	}
	var decoded []*string
	if len(data) == 0 || data[0] != '[' || json.Unmarshal(data, &decoded) != nil {
		return errors.New("database: expected one-dimensional text array")
	}
	*a = decoded
	return nil
}
