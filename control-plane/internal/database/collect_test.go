package database_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
)

type columnRows struct {
	values  []int
	scanErr error
	err     error
	closed  bool
}

func (r *columnRows) Next() bool { return len(r.values) > 0 }
func (r *columnRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	*dest[0].(*int), r.values = r.values[0], r.values[1:]
	return nil
}
func (r *columnRows) Err() error { return r.err }
func (r *columnRows) Close()     { r.closed = true }

func TestCollectColumn(t *testing.T) {
	queryErr := errors.New("query")
	if got, err := database.CollectColumn[int](nil, queryErr); got != nil || !errors.Is(err, queryErr) {
		t.Fatalf("query error = %v, %v", got, err)
	}
	rows := &columnRows{values: []int{3, 1, 2}}
	if got, err := database.CollectColumn[int](rows, nil); err != nil || !reflect.DeepEqual(got, []int{3, 1, 2}) || !rows.closed {
		t.Fatalf("rows = %v, %v, closed %v", got, err, rows.closed)
	}
	for _, rows := range []*columnRows{{values: []int{1}, scanErr: errors.New("scan")}, {err: errors.New("iterate")}} {
		if got, err := database.CollectColumn[int](rows, nil); got != nil || err == nil || !rows.closed {
			t.Fatalf("failure = %v, %v, closed %v", got, err, rows.closed)
		}
	}
}
