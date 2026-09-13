package mysql

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetryhistory"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Explicit opt-in: latency assertions are unsuitable for ordinary shared CI.
// Each invocation requires a disposable database and separate owner/runtime roles.
func TestTelemetryLowDataPerformance(t *testing.T) {
	if os.Getenv("PR07_PERFORMANCE") != "1" {
		t.Skip("PR07_PERFORMANCE=1 and isolated database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pg := os.Getenv("PR02_DSN") == ""
	var owner, runtime, monitor database.Backend
	if pg {
		for i, key := range []string{"OCSERV_TEST_OWNER_DATABASE_URL", "OCSERV_TEST_DATABASE_URL"} {
			if os.Getenv(key) == "" {
				t.Fatal("missing ", key)
			}
			pool, err := pgxpool.New(ctx, os.Getenv(key))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			if i == 0 {
				owner = postgres.WrapPool(pool)
			} else {
				runtime = postgres.WrapPool(pool)
			}
		}
		monitor = owner
	} else {
		admin, err := Open(ctx, testOptions(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { admin.Close() })
		monitor = admin
		b, _, options := migrateFixture(t)
		if err := b.PrepareControllerTelemetry(ctx); err != nil {
			t.Fatal(err)
		}
		if err := b.GrantTestPrivileges(ctx); err != nil {
			t.Fatal(err)
		}
		owner = b
		options.DSN = strings.Replace(options.DSN, "ocservia_owner:pr02-owner-test-only@", "ocservia_app:pr02-runtime-test-only@", 1)
		r, err := Open(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close() })
		runtime = r
	}
	query := func(sql string) string {
		if !pg {
			return sql
		}
		for n := 1; strings.Contains(sql, "?"); n++ {
			sql = strings.Replace(sql, "?", fmt.Sprintf("$%d", n), 1)
		}
		return sql
	}
	id := func(u uuid.UUID) any {
		if pg {
			return u
		}
		return UUIDBytes(u)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, query(sql), args...); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	stamp, _ := value.FromTime(now)
	ws, node, batch := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'performance',?,?,?)`, id(ws), ws.String(), stamp, stamp)
	exec(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,'performance','active',?,?)`, id(node), id(ws), stamp, stamp)
	exec(`INSERT INTO telemetry_ingest_batches(batch_id,node_id,sequence,kind,observed_at,payload_bytes) VALUES(?,?,1,'raw_history',?,1)`, id(batch), id(node), stamp)
	metrics := []string{"cpu_usage_ratio", "memory_used_bytes", "network_rx_bytes", "network_tx_bytes", "session_count", "connection_rtt_ms"}
	samplesAt := func(at time.Time) []telemetryhistory.Sample {
		var samples []telemetryhistory.Sample
		for _, metric := range metrics {
			samples = append(samples, telemetryhistory.Sample{SampledAt: at, Metric: metric, Value: 1})
		}
		return samples
	}
	storeTx := func(fn func(telemetryhistory.Store) error) error {
		return database.Within(ctx, runtime, database.ReadCommitted, func(tx database.Tx) error {
			store, err := telemetryhistory.FromTransaction(tx)
			if err != nil {
				return err
			}
			return fn(store)
		})
	}
	var seed []telemetryhistory.Sample
	for hour := 1; hour <= 14*24; hour++ {
		seed = append(seed, samplesAt(now.Add(-time.Duration(hour)*time.Hour))...)
	}
	if err := storeTx(func(s telemetryhistory.Store) error { return s.Insert(ctx, node, batch, seed) }); err != nil {
		t.Fatal(err)
	}
	old := time.Date(now.Year(), now.Month()-2, 1, 0, 0, 0, 0, time.UTC)
	oldTable := "telemetry_samples"
	if pg {
		// Recreate an owner-provisioned historical month as it existed before
		// the outage; the live provisioning routine rejects dates this old.
		name := "telemetry_samples_" + old.Format("200601")
		exec(`CREATE TABLE ` + name + ` PARTITION OF telemetry_samples FOR VALUES FROM ('` + old.Format(time.RFC3339) + `') TO ('` + old.AddDate(0, 1, 0).Format(time.RFC3339) + `')`)
		exec(`CREATE INDEX ` + name + `_query_idx ON ` + name + `(node_id,metric,sampled_at DESC)`)
	} else {
		b := owner.(*Backend)
		if err := b.ProvisionTelemetryMonth(ctx, old); err != nil {
			t.Fatal(err)
		}
		if err := b.GrantTestPrivileges(ctx); err != nil {
			t.Fatal(err)
		}
		oldTable, _, _, _ = telemetryMonth(old)
	}
	oldHours := int(old.AddDate(0, 1, 0).Sub(old) / time.Hour)
	for hour := 0; hour < oldHours; hour++ {
		for _, sample := range samplesAt(old.Add(time.Duration(hour) * time.Hour)) {
			at, _ := value.FromTime(sample.SampledAt)
			exec(`INSERT INTO `+oldTable+`(node_id,batch_id,sampled_at,metric,value) VALUES(?,?,?,?,?)`, id(node), id(batch), at, sample.Metric, sample.Value)
		}
	}
	var dbName string
	dbQuery := `SELECT DATABASE()`
	if pg {
		dbQuery = `SELECT current_database()`
	}
	if err := owner.QueryRow(ctx, dbQuery).Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	allocated := func() int64 {
		t.Helper()
		var bytes int64
		sql := `SELECT COALESCE(SUM(ALLOCATED_SIZE),0) FROM information_schema.INNODB_TABLESPACES WHERE NAME LIKE ?`
		arg := any(dbName + "/%")
		if pg {
			sql, arg = `SELECT pg_database_size($1)`, dbName
		} else if owner.(*Backend).engine == MariaDB {
			sql = `SELECT COALESCE(SUM(ALLOCATED_SIZE),0) FROM information_schema.INNODB_SYS_TABLESPACES WHERE NAME LIKE ?`
		}
		if err := monitor.QueryRow(ctx, sql, arg).Scan(&bytes); err != nil {
			t.Fatal(err)
		}
		if bytes <= 0 {
			t.Fatal("actual allocation unavailable")
		}
		return bytes
	}
	counts := func() int64 {
		var count int64
		if err := owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM telemetry_rollups_5m)+(SELECT count(*) FROM telemetry_rollups_1h)`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	maintain := func() error {
		return telemetryhistory.Maintain(ctx, runtime, now, nil, func(context.Context, database.Tx) error { return nil })
	}
	since, _ := value.FromTime(now.Add(-24 * time.Hour))
	planTable := "telemetry_samples"
	planPrefix := "EXPLAIN (FORMAT JSON) "
	if !pg {
		planTable, _, _, _ = telemetryMonth(now)
		planPrefix = "EXPLAIN FORMAT=JSON "
	}
	var historyPlan string
	if err := monitor.QueryRow(ctx, query(planPrefix+`SELECT sampled_at,value FROM `+planTable+` WHERE node_id=? AND metric=? AND sampled_at>=? ORDER BY sampled_at`), id(node), "cpu_usage_ratio", since).Scan(&historyPlan); err != nil {
		t.Fatal(err)
	}
	t.Logf("history_query_plan=%s", strings.ReplaceAll(historyPlan, "\n", " "))
	var durations [4][]time.Duration
	var wg sync.WaitGroup
	start := make(chan struct{})
	errors := make(chan error, 4)
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			n := 200
			if worker == 3 {
				n = 11
			}
			for i := 0; i < n; i++ {
				began := time.Now()
				var err error
				switch worker {
				case 0:
					err = storeTx(func(s telemetryhistory.Store) error {
						return s.Insert(ctx, node, batch, samplesAt(now.Add(time.Duration(i)*time.Microsecond)))
					})
				case 1:
					err = storeTx(func(s telemetryhistory.Store) error {
						_, err := s.History(ctx, node, "cpu_usage_ratio", "raw", since)
						return err
					})
				case 2:
					updated, _ := value.FromTime(now.Add(time.Duration(i+1) * time.Microsecond))
					_, err = runtime.Exec(ctx, query(`UPDATE nodes SET updated_at=? WHERE id=?`), updated, id(node))
				case 3:
					err = maintain()
				}
				durations[worker] = append(durations[worker], time.Since(began))
				if err != nil {
					errors <- fmt.Errorf("worker %d: %w", worker, err)
					return
				}
				delay := 10 * time.Millisecond
				if worker == 3 {
					delay = 150 * time.Millisecond
				}
				time.Sleep(delay)
			}
		}(worker)
	}
	// Sample real waiting transactions. Each observed episode is conservatively
	// padded by the largest observer gap; unobserved shorter waits are bounded by
	// that gap. Reject an observer stall instead of treating missing data as zero.
	lockSQL := `SELECT DISTINCT CAST(REQUESTING_ENGINE_TRANSACTION_ID AS CHAR) FROM performance_schema.data_lock_waits`
	if pg {
		lockSQL = `SELECT pid::text FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`
	} else if owner.(*Backend).engine == MariaDB {
		lockSQL = `SELECT DISTINCT requesting_trx_id FROM information_schema.INNODB_LOCK_WAITS`
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	benchmarkStart := time.Now()
	close(start)
	last := time.Now()
	maxGap := time.Duration(0)
	active := map[string]time.Time{}
	var waits []time.Duration
	observe := true
	for observe {
		at := time.Now()
		if gap := at.Sub(last); gap > maxGap {
			maxGap = gap
		}
		last = at
		rows, err := monitor.Query(ctx, lockSQL)
		if err != nil {
			cancel()
			wg.Wait()
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				cancel()
				wg.Wait()
				t.Fatal(err)
			}
			seen[key] = true
			if _, ok := active[key]; !ok {
				active[key] = at
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			cancel()
			wg.Wait()
			t.Fatal(err)
		}
		if gap := time.Since(at); gap > maxGap {
			maxGap = gap
		}
		for key, began := range active {
			if !seen[key] {
				waits = append(waits, at.Sub(began))
				delete(active, key)
			}
		}
		select {
		case <-done:
			observe = false
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	for _, began := range active {
		waits = append(waits, time.Since(began))
	}
	benchmarkDuration := time.Since(benchmarkStart)
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	if t.Failed() {
		if !pg {
			var kind, name, status string
			if err := monitor.QueryRow(ctx, `SHOW ENGINE INNODB STATUS`).Scan(&kind, &name, &status); err == nil {
				t.Log(status)
			}
		}
		return
	}
	if !pg {
		if err := owner.(*Backend).CollectRetiredTelemetryShards(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Catch up the final concurrent writes before freezing the dataset.
	if err := maintain(); err != nil {
		t.Fatal(err)
	}
	beforeBytes, beforeRows := allocated(), counts()
	for i := 0; i < 10; i++ {
		if err := maintain(); err != nil {
			t.Fatal(err)
		}
	}
	afterBytes, afterRows := allocated(), counts()
	if !pg {
		t.Logf("runtime_pool=%+v", runtime.(*Backend).pool.Stats())
	}
	t.Logf("concurrent_throughput=%.2f_ops_per_second elapsed=%s", float64(len(durations[0])+len(durations[1])+len(durations[2])+len(durations[3]))/benchmarkDuration.Seconds(), benchmarkDuration)
	percentile := func(values []time.Duration, p int) time.Duration {
		v := append([]time.Duration(nil), values...)
		sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
		return v[(len(v)*p+99)/100-1]
	}
	check := func(name string, got, limit time.Duration) {
		t.Helper()
		t.Logf("%s=%s limit=%s", name, got, limit)
		if got > limit {
			t.Errorf("%s exceeds limit", name)
		}
	}
	check("ingestion_p95", percentile(durations[0], 95), 25*time.Millisecond)
	check("ingestion_p99", percentile(durations[0], 99), 50*time.Millisecond)
	check("query_p95", percentile(durations[1], 95), 10*time.Millisecond)
	check("query_p99", percentile(durations[1], 99), 25*time.Millisecond)
	check("catchup", durations[3][0], 5*time.Second)
	check("maintenance_hard_max", percentile(durations[3], 100), 10*time.Second)
	check("normal_p95", percentile(durations[3][1:], 95), time.Second)
	check("normal_max", percentile(durations[3][1:], 100), 2*time.Second)
	t.Logf("controller_update_max=%s observed_lock_episodes=%d", percentile(durations[2], 100), len(waits))
	check("lock_observer_gap", maxGap, 100*time.Millisecond)
	if len(waits) == 0 {
		waits = append(waits, 0)
	}
	for i := range waits {
		waits[i] += maxGap
	}
	check("lock_wait_p95_upper_bound", percentile(waits, 95), 100*time.Millisecond)
	check("lock_wait_max_upper_bound", percentile(waits, 100), 500*time.Millisecond)
	t.Logf("dataset: 1 node, 6 metrics, recent=%d outage=%d; concurrent operations=%d/%d/%d/%d; allocated=%d->%d rollups=%d->%d", len(seed), oldHours*6, len(durations[0]), len(durations[1]), len(durations[2]), len(durations[3]), beforeBytes, afterBytes, beforeRows, afterRows)
	if beforeRows != afterRows {
		t.Error("fixed-dataset rollup count changed")
	}
	if float64(afterBytes) > float64(beforeBytes)*1.05 {
		t.Error("actual allocation grew more than 5 percent")
	}
}
