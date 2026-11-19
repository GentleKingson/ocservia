#![allow(clippy::missing_panics_doc)]

use std::time::{Instant, SystemTime, UNIX_EPOCH};

use ocservia_command_journal::{Journal, TelemetryInsert};

#[test]
#[ignore = "run explicitly via scripts/perf-bench.sh"]
fn perf_journal_telemetry_enqueue_evict_drain() {
    let nonce = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .expect("clock")
        .as_nanos();
    let dir = std::env::temp_dir().join(format!(
        "ocservia-journal-bench-{}-{nonce}",
        std::process::id()
    ));
    std::fs::create_dir_all(&dir).expect("mkdir");
    let db_path = dir.join("journal.db");
    let mut journal = Journal::open(&db_path).expect("open journal");

    let payload = vec![b'x'; 8 * 1024];
    let now = 1_760_000_000i64;
    let max_bytes = 512 * 1024u64; // holds 64 batches -> subsequent inserts trigger eviction

    let enqueue_iters = 200u32;
    let start = Instant::now();
    for i in 0..enqueue_iters {
        let mut id = [0u8; 16];
        id[12..16].copy_from_slice(&i.to_be_bytes());
        let _ = journal
            .enqueue_telemetry(&TelemetryInsert {
                batch_id: &id,
                priority: u8::try_from((i % 4) + 1).expect("priority"),
                observed_at: now + i64::from(i),
                expires_at: now + 3600 + i64::from(i),
                payload: &payload,
                now: now + i64::from(i),
                max_bytes,
            })
            .expect("enqueue");
    }
    let enqueue_elapsed = start.elapsed();
    println!(
        "RUST_BENCH journal_enqueue_telemetry_200x8kib_evict iterations={enqueue_iters} total_ms={:.3} us_per_op={}",
        enqueue_elapsed.as_secs_f64() * 1000.0,
        enqueue_elapsed.as_micros() / u128::from(enqueue_iters)
    );

    let start = Instant::now();
    let pending = journal.telemetry_pending(64, now + 300).expect("pending");
    let pending_elapsed = start.elapsed();
    println!(
        "RUST_BENCH journal_telemetry_pending_64 count={} total_ms={:.3}",
        pending.len(),
        pending_elapsed.as_secs_f64() * 1000.0
    );

    let start = Instant::now();
    for (id, _) in &pending {
        journal.acknowledge_telemetry_batch(&[*id]).expect("ack");
    }
    let ack_elapsed = start.elapsed();
    println!(
        "RUST_BENCH journal_acknowledge_64_individual count={} total_ms={:.3}",
        pending.len(),
        ack_elapsed.as_secs_f64() * 1000.0
    );

    let mut batch_ids = Vec::with_capacity(64);
    for i in 0..64u32 {
        let mut id = [0u8; 16];
        id[0] = 1;
        id[12..16].copy_from_slice(&i.to_be_bytes());
        let _ = journal
            .enqueue_telemetry(&TelemetryInsert {
                batch_id: &id,
                priority: 2,
                observed_at: now + 500 + i64::from(i),
                expires_at: now + 4100 + i64::from(i),
                payload: &payload,
                now: now + 500 + i64::from(i),
                max_bytes,
            })
            .expect("enqueue batch");
        batch_ids.push(id);
    }
    let start = Instant::now();
    journal
        .acknowledge_telemetry_batch(&batch_ids)
        .expect("ack batch");
    let batch_ack_elapsed = start.elapsed();
    println!(
        "RUST_BENCH journal_acknowledge_64_batched count={} total_ms={:.3}",
        batch_ids.len(),
        batch_ack_elapsed.as_secs_f64() * 1000.0
    );

    drop(journal);
    let _ = std::fs::remove_dir_all(&dir);
}
