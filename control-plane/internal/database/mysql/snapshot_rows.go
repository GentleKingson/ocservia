package mysql

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// Hex encoding is independent of connection escaping and preserves binary
// UUIDs, JSON, decimal strings and NULL distinctly. Sort rows by their complete
// encoded value rather than environment-specific collations.
func snapshotRows(ctx context.Context, conn schemaQueryer, table string, columns []string) ([]string, [][]*string, error) {
	if len(columns) == 0 {
		rows, err := conn.QueryContext(ctx, "SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND EXTRA NOT LIKE '%GENERATED%' ORDER BY ORDINAL_POSITION", table)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var c string
			if err = rows.Scan(&c); err != nil {
				rows.Close()
				return nil, nil, err
			}
			columns = append(columns, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, nil, err
		}
	}
	var fields []string
	for _, c := range columns {
		fields = append(fields, "CAST(`"+c+"` AS BINARY)")
	}
	rows, err := conn.QueryContext(ctx, "SELECT "+strings.Join(fields, ",")+" FROM `"+table+"`")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	values := [][]*string{}
	for rows.Next() {
		raw := make([][]byte, len(columns))
		dest := make([]any, len(columns))
		for i := range dest {
			dest[i] = &raw[i]
		}
		if err = rows.Scan(dest...); err != nil {
			return nil, nil, err
		}
		row := make([]*string, len(raw))
		for i, v := range raw {
			if v != nil {
				x := hex.EncodeToString(v)
				row[i] = &x
			}
		}
		values = append(values, row)
	}
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	sort.Slice(values, func(i, j int) bool {
		a, _ := json.Marshal(values[i])
		b, _ := json.Marshal(values[j])
		return bytes.Compare(a, b) < 0
	})
	return columns, values, nil
}
