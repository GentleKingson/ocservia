use ocservia_test_support::PrivateTempDir;

use super::*;

struct TempDb(std::path::PathBuf, #[allow(dead_code)] PrivateTempDir);
impl TempDb {
    fn new(label: &str) -> Self {
        let dir = PrivateTempDir::new("cj");
        let path = dir.join(format!("{label}.db"));
        Self(path, dir)
    }
    fn open(&self) -> Journal {
        Journal::open(&self.0).expect("open")
    }
}

#[test]
fn pragmas_corrupt_readonly_and_full_db_invariants() {
    let db = TempDb::new("pragmas");
    {
        let j = db.open();
        let mode: String = j
            .connection
            .pragma_query_value(None, "journal_mode", |r| r.get(0))
            .expect("m");
        let fk: bool = j
            .connection
            .pragma_query_value(None, "foreign_keys", |r| r.get(0))
            .expect("fk");
        let bt: u64 = j
            .connection
            .pragma_query_value(None, "busy_timeout", |r| r.get(0))
            .expect("bt");
        let sync: u8 = j
            .connection
            .pragma_query_value(None, "synchronous", |r| r.get(0))
            .expect("s");
        assert_eq!(mode.to_ascii_lowercase(), "wal");
        assert!(fk);
        assert_eq!((bt, sync), (5000, 2));
        assert_eq!(
            std::fs::metadata(&db.0).expect("md").permissions().mode() & 0o777,
            0o600
        );
    }

    // Corrupt database is refused.
    let corrupt = TempDb::new("corrupt");
    std::fs::write(&corrupt.0, b"not a sqlite database").expect("w");
    assert!(Journal::open(&corrupt.0).is_err());

    // Read-only database cannot accept a command (non-root only).
    let is_root = std::fs::read_to_string("/proc/self/status").is_ok_and(|st| {
        st.lines().any(|l| {
            l.strip_prefix("Uid:")
                .and_then(|r| r.split_whitespace().nth(1))
                == Some("0")
        })
    });
    if !is_root {
        let ro = TempDb::new("readonly");
        drop(ro.open());
        std::fs::set_permissions(&ro.0, std::fs::Permissions::from_mode(0o444)).expect("444");
        let res = Journal::open(&ro.0)
            .and_then(|mut j| j.accept_command(&[1; 16], &[2; 16], &[3; 32], 0, 10));
        std::fs::set_permissions(&ro.0, std::fs::Permissions::from_mode(0o600)).expect("600");
        assert!(res.is_err());
    }

    // Full database rolls back partial synthetic effect.
    let full = TempDb::new("full");
    let (key, cmd_id, payload) = ([1_u8; 16], [3_u8; 16], [2_u8; 32]);
    let mut j = full.open();
    j.accept_command(&key, &cmd_id, &payload, 0, 10)
        .expect("acc");
    j.transition_command(
        &key,
        &[CommandState::Accepted],
        CommandState::Running,
        None,
        None,
        10,
    )
    .expect("run");
    let _ = j
        .connection
        .pragma_update(None, "wal_checkpoint", "TRUNCATE");
    let pages: u32 = j
        .connection
        .pragma_query_value(None, "page_count", |r| r.get(0))
        .expect("p");
    j.connection
        .pragma_update(None, "max_page_count", pages)
        .expect("lim");
    assert!(
        j.execute_and_complete_synthetic(&key, &cmd_id, &payload, &vec![7; 900_000], 11)
            .is_err()
    );
    assert!(j.synthetic_effect(&key).expect("q").is_none());
    assert_eq!(j.synthetic_execution_count().expect("c"), 0);
}

#[test]
fn command_identity_replay_attested_proof_and_revision_commit() {
    let db = TempDb::new("cmd-attested");
    let (k1, c1) = (
        *uuid::Uuid::now_v7().as_bytes(),
        *uuid::Uuid::now_v7().as_bytes(),
    );
    {
        let mut j = db.open();
        assert!(matches!(
            j.accept_command(&k1, &c1, &[7; 32], 2, 10).expect("a"),
            AcceptOutcome::Accepted(_)
        ));
        assert!(matches!(
            j.accept_command(&k1, &c1, &[7; 32], 2, 11).expect("r"),
            AcceptOutcome::Replay(_)
        ));
        assert!(matches!(
            j.accept_command(&k1, uuid::Uuid::now_v7().as_bytes(), &[7; 32], 2, 12)
                .expect("ic1"),
            AcceptOutcome::IdentityConflict(_)
        ));
        assert!(matches!(
            j.accept_command(uuid::Uuid::now_v7().as_bytes(), &c1, &[7; 32], 2, 13)
                .expect("ic2"),
            AcceptOutcome::IdentityConflict(_)
        ));
        assert!(matches!(
            j.accept_command(&k1, &c1, &[8; 32], 2, 14).expect("pc"),
            AcceptOutcome::PayloadConflict(_)
        ));
        j.transition_command(
            &k1,
            &[CommandState::Accepted],
            CommandState::Running,
            None,
            None,
            15,
        )
        .expect("run");
        let done = j
            .transition_command_attested(
                &k1,
                &[CommandState::Running],
                CommandState::Succeeded,
                Some(b"exact-result"),
                None,
                b"opaque-root-receipt",
                16,
            )
            .expect("att");
        assert_eq!(
            (
                done.result.as_deref(),
                done.privileged_result_proof.as_deref()
            ),
            (
                Some(b"exact-result".as_slice()),
                Some(b"opaque-root-receipt".as_slice())
            )
        );
    }
    let mut j = db.open();
    let AcceptOutcome::Replay(rec) = j.accept_command(&k1, &c1, &[7; 32], 2, 17).expect("rep")
    else {
        panic!("expected replay");
    };
    assert_eq!(
        (
            rec.state,
            rec.result.as_deref(),
            rec.privileged_result_proof.as_deref()
        ),
        (
            CommandState::Succeeded,
            Some(b"exact-result".as_slice()),
            Some(b"opaque-root-receipt".as_slice())
        )
    );

    // External completion + applied resource revision commit atomically across restart.
    let (k2, c2) = (
        *uuid::Uuid::now_v7().as_bytes(),
        *uuid::Uuid::now_v7().as_bytes(),
    );
    j.accept_command(&k2, &c2, &[7; 32], 1, 20).expect("a");
    j.transition_command(
        &k2,
        &[CommandState::Accepted],
        CommandState::Running,
        None,
        None,
        21,
    )
    .expect("r");
    j.complete_external_with_revision_attested(
        &k2,
        &c2,
        AppliedResourceRevision {
            resource_type: "user",
            resource_key: "alice",
            revision: 2,
        },
        b"applied",
        b"proof",
        22,
    )
    .expect("c");
    drop(j);
    let j = db.open();
    assert_eq!(j.applied_revision("user", "alice").expect("rev"), Some(2));
    assert_eq!(
        j.command(&k2).expect("q").expect("rec").state,
        CommandState::Succeeded
    );
}

#[test]
fn telemetry_eviction_and_five_minute_offline_recovery() {
    let db = TempDb::new("telemetry");
    let mut j = db.open();
    let (sec, raw) = (
        *uuid::Uuid::now_v7().as_bytes(),
        *uuid::Uuid::now_v7().as_bytes(),
    );
    assert!(
        j.enqueue_telemetry(&TelemetryInsert {
            batch_id: &sec,
            priority: 1,
            observed_at: 10,
            expires_at: 100,
            payload: b"security",
            now: 20,
            max_bytes: 12
        })
        .expect("s")
    );
    assert!(
        !j.enqueue_telemetry(&TelemetryInsert {
            batch_id: &raw,
            priority: 4,
            observed_at: 11,
            expires_at: 100,
            payload: b"raw-data",
            now: 20,
            max_bytes: 12
        })
        .expect("r")
    );
    let pending = j.telemetry_pending(10, 20).expect("p");
    assert_eq!((pending.len(), pending[0].0), (1, sec));
    assert_eq!(j.telemetry_drop_counters().expect("d"), [0, 0, 0, 1]);
    j.acknowledge_telemetry_batch(&[sec]).expect("ack");
    assert_eq!(j.telemetry_pending(10, 20).expect("e").len(), 0);

    let recovery_secs = i64::try_from(OFFLINE_RECOVERY_RETENTION_SECONDS).expect("s");
    let (legacy, rec) = (
        *uuid::Uuid::now_v7().as_bytes(),
        *uuid::Uuid::now_v7().as_bytes(),
    );
    j.enqueue_telemetry(&TelemetryInsert {
        batch_id: &legacy,
        priority: 2,
        observed_at: 1_000,
        expires_at: 1_000 + 86_400,
        payload: b"legacy-day-window",
        now: 1_000,
        max_bytes: 1_024,
    })
    .expect("l");
    drop(j);
    let mut j = db.open();
    assert!(
        j.telemetry_pending(10, 1_299)
            .expect("b")
            .iter()
            .any(|x| x.0 == legacy)
    );
    assert!(
        j.telemetry_pending(10, 1_300)
            .expect("a")
            .iter()
            .all(|x| x.0 != legacy)
    );
    assert_eq!(j.telemetry_drop_counters().expect("d"), [0, 1, 0, 1]);
    j.enqueue_telemetry(&TelemetryInsert {
        batch_id: &rec,
        priority: 2,
        observed_at: 1_300,
        expires_at: 1_300 + recovery_secs,
        payload: b"reconnected",
        now: 1_300,
        max_bytes: 1_024,
    })
    .expect("rec");
    let pending = j.telemetry_pending(10, 1_301).expect("p");
    assert!(!pending.iter().any(|x| x.0 == legacy) && pending.iter().any(|x| x.0 == rec));
}

#[test]
fn synthetic_effect_atomicity_rollback_and_restart_idempotency() {
    let db = TempDb::new("synth");
    let (k1, c1, p1) = (
        *uuid::Uuid::now_v7().as_bytes(),
        *uuid::Uuid::now_v7().as_bytes(),
        [9_u8; 32],
    );
    {
        let mut j = db.open();
        j.accept_command(&k1, &c1, &p1, 0, 10).expect("a");
        j.transition_command(
            &k1,
            &[CommandState::Accepted],
            CommandState::Running,
            None,
            None,
            11,
        )
        .expect("r");
        j.connection
            .execute_batch(
                "CREATE TEMP TRIGGER fail_terminal_result
             BEFORE UPDATE OF state ON command_journal WHEN NEW.state = 'succeeded'
             BEGIN SELECT RAISE(FAIL, 'injected'); END;",
            )
            .expect("trig");
        assert!(
            j.execute_and_complete_synthetic(&k1, &c1, &p1, b"result", 12)
                .is_err()
        );
        assert!(j.synthetic_effect(&k1).expect("q").is_none());
        assert_eq!(j.synthetic_execution_count().expect("c"), 0);
        assert_eq!(
            j.command(&k1).expect("q").unwrap().state,
            CommandState::Running
        );
    }

    let (k2, c2, p2) = (
        *uuid::Uuid::now_v7().as_bytes(),
        *uuid::Uuid::now_v7().as_bytes(),
        [3_u8; 32],
    );
    {
        let mut j = db.open();
        j.accept_command(&k2, &c2, &p2, 0, 10).expect("a");
        j.transition_command(
            &k2,
            &[CommandState::Accepted],
            CommandState::Running,
            None,
            None,
            10,
        )
        .expect("r");
        let rec = j
            .execute_and_complete_synthetic(&k2, &c2, &p2, b"done", 10)
            .expect("1st");
        assert_eq!(rec.state, CommandState::Succeeded);
    }
    let mut j = db.open();
    assert!(matches!(
        j.accept_command(&k2, &c2, &p2, 0, 11).expect("replay"),
        AcceptOutcome::Replay(rec) if rec.state == CommandState::Succeeded
    ));
    assert!(
        j.execute_and_complete_synthetic(&k2, &c2, &p2, b"done", 11)
            .is_err()
    );
    assert_eq!(j.synthetic_execution_count().expect("c"), 1);
    assert_eq!(
        j.synthetic_effect(&k2).expect("e").expect("p").result,
        b"done"
    );
}

#[test]
fn owner_fence_epoch_floor_is_monotonic_durable_and_concurrent_safe() {
    let db = TempDb::new("fence-floor");
    {
        let j = db.open();
        assert_eq!(j.owner_fence_epoch_floor().expect("0"), 0);
        assert_eq!(j.raise_owner_fence_epoch_floor(4).expect("4"), 4);
        assert_eq!(j.raise_owner_fence_epoch_floor(2).expect("4"), 4);
        assert_eq!(j.raise_owner_fence_epoch_floor(9).expect("9"), 9);
    }
    let stale = db.open();
    let current = db.open();
    assert_eq!(stale.owner_fence_epoch_floor().expect("9"), 9);
    assert_eq!(current.raise_owner_fence_epoch_floor(10).expect("10"), 10);
    assert_eq!(stale.raise_owner_fence_epoch_floor(6).expect("10"), 10);
    assert_eq!(stale.owner_fence_epoch_floor().expect("10"), 10);
}

#[test]
fn telemetry_batch_acknowledge_is_atomic_and_idempotent() {
    let db = TempDb::new("telemetry-batch-ack");
    let mut j = db.open();
    let mut ids = Vec::new();
    for i in 0..8_i64 {
        let id = *uuid::Uuid::now_v7().as_bytes();
        j.enqueue_telemetry(&TelemetryInsert {
            batch_id: &id,
            priority: 2,
            observed_at: 1_000 + i,
            expires_at: 2_000,
            payload: b"telemetry-batch",
            now: 1_000 + i,
            max_bytes: 1_048_576,
        })
        .expect("enqueue");
        ids.push(id);
    }

    // Empty slice is a safe no-op.
    j.acknowledge_telemetry_batch(&[]).expect("empty ack");
    assert_eq!(j.telemetry_pending(16, 1_100).expect("pending").len(), 8);

    // Acknowledge first 5 batches including a duplicate ID in one transaction.
    let first_five = [ids[0], ids[1], ids[2], ids[2], ids[3], ids[4]];
    j.acknowledge_telemetry_batch(&first_five)
        .expect("first five");
    assert_eq!(
        j.telemetry_pending(16, 1_100)
            .expect("after first five")
            .len(),
        3
    );

    // Re-acknowledging the same batch is idempotent and leaves remaining items intact.
    j.acknowledge_telemetry_batch(&first_five)
        .expect("repeat ack");

    let remaining = j.telemetry_pending(16, 1_100).expect("remaining");
    assert_eq!(remaining.len(), 3);
    let remaining_ids: Vec<[u8; 16]> = remaining.into_iter().map(|row| row.0).collect();
    assert_eq!(remaining_ids, vec![ids[5], ids[6], ids[7]]);
}
