package mysql

import (
	"context"
	"fmt"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
)

var longKeys = []string{"identities", "workspaces", "nodes", "operations", "agent_command_results", "upstream_sync_records", "user_policy_enforcements", "telemetry_rollups_5m", "telemetry_rollups_1h"}

func LockExactKey(ctx context.Context, tx database.Tx, table string) error {
	for _, d := range longKeys {
		if d == table {
			var key []byte
			return tx.QueryRow(ctx, "SELECT key_name FROM exact_key_guards WHERE key_name=? FOR UPDATE", []byte(table)).Scan(&key)
		}
	}
	return fmt.Errorf("unknown exact natural key: %w", database.ErrUnsupported)
}
