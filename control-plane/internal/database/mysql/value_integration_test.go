package mysql

import (
	"github.com/GentleKingson/ocservia/control-plane/internal/database/semantictest"
	"testing"
)

func TestRealLogicalValues(t *testing.T) {
	b, _, _ := migrateFixture(t)
	semantictest.LogicalValues(t, b, `CREATE TEMPORARY TABLE logical_values(id INT PRIMARY KEY,ts BIGINT,j LONGBLOB,a LONGBLOB)`, `INSERT INTO logical_values VALUES(?,?,?,?)`, `SELECT ts,j,a FROM logical_values WHERE id=?`, `SELECT TIMESTAMPDIFF(MICROSECOND,'2000-01-01 00:00:00',UTC_TIMESTAMP(6))`)
}
