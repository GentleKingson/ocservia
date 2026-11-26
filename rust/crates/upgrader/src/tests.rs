use std::fs::{self, OpenOptions};
use std::io::Write as _;
use std::os::unix::fs::{OpenOptionsExt as _, PermissionsExt as _};
use std::path::{Path, PathBuf};
use std::sync::{Mutex, PoisonError};

use super::*;
use crate::fs::{TempOwner, write_atomic};
use crate::runner::write_terminal;
use crate::scheduler::load_state;
use ocservia_test_support::PrivateTempDir;

static RUNNER_PROCESS_LOCK: Mutex<()> = Mutex::new(());

fn lock_runner_tests() -> std::sync::MutexGuard<'static, ()> {
    RUNNER_PROCESS_LOCK
        .lock()
        .unwrap_or_else(PoisonError::into_inner)
}

struct TempRoot(PrivateTempDir);
impl TempRoot {
    fn new(tag: &str) -> Self {
        Self(PrivateTempDir::new(&format!("upg-{tag}")))
    }
    fn path(&self) -> &Path {
        self.0.path()
    }
    fn ops(&self) -> PathBuf {
        self.0.join("var/lib/ocservia-upgrade/operations")
    }
    fn scheduler(&self) -> UpgradeScheduler {
        UpgradeScheduler::new(self.ops(), UpgradeTrigger::Disabled)
    }
    fn runner(&self) -> UpgradeRunner {
        UpgradeRunner::new(self.0.path().to_path_buf())
    }
    fn force_state(&self, op: &Uuid, state: OperationState) {
        write_atomic(
            &self.ops().join(op.to_string()).join("state"),
            format!("{}\n", state.as_str()).as_bytes(),
            TempOwner::Run,
        )
        .expect("force");
    }
}

fn new_intent(sha: [u8; 32]) -> UpgradeIntent {
    UpgradeIntent::new(
        *Uuid::now_v7().as_bytes(),
        *Uuid::now_v7().as_bytes(),
        "1.2.3",
        sha,
        ocservia_contracts::agent_upgrade::runtime_architecture().expect("arch"),
        [0x55; 32],
    )
    .expect("intent")
}

fn plant_tmp(path: &Path, bytes: &[u8]) {
    let mut f = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(path)
        .expect("tmp");
    f.write_all(bytes).expect("w");
    f.sync_all().expect("sync");
}

#[test]
fn scheduling_idempotency_conflict_and_active_slot() {
    let root = TempRoot::new("sched");
    let intent = new_intent([0x43; 32]);
    assert_eq!(
        root.scheduler().schedule_and_trigger(&intent).expect("1st"),
        ScheduleOutcome::Scheduled
    );
    assert_eq!(
        root.scheduler()
            .schedule_and_trigger(&intent)
            .expect("replay"),
        ScheduleOutcome::AlreadyScheduled
    );

    let mut conflict = intent.clone();
    conflict.package_sha256[0] ^= 1;
    assert!(matches!(
        root.scheduler().schedule_and_trigger(&conflict),
        Err(UpgradeStoreError::IdentityConflict)
    ));
    assert!(matches!(
        root.scheduler()
            .schedule_and_trigger(&new_intent([0x44; 32])),
        Err(UpgradeStoreError::ActiveConflict)
    ));

    root.force_state(&intent.operation_id, OperationState::Succeeded);
    assert_eq!(
        root.scheduler()
            .schedule_and_trigger(&intent)
            .expect("terminal"),
        ScheduleOutcome::AlreadyScheduled
    );
    assert_eq!(
        root.scheduler()
            .schedule_and_trigger(&new_intent([0x44; 32]))
            .expect("next"),
        ScheduleOutcome::Scheduled
    );
}

#[test]
fn filesystem_substitution_and_unsafe_ancestry_are_rejected() {
    let root = TempRoot::new("subst");
    let sub = TempRoot::new("subst-target");
    let intent = new_intent([6; 32]);
    root.scheduler()
        .schedule_and_trigger(&intent)
        .expect("sched");
    let op_dir = root.ops().join(intent.operation_id.to_string());
    let (ip, sp) = (op_dir.join("intent"), op_dir.join("state"));
    let trusted = fs::read(&ip).expect("read");
    let outside = sub.path().join("outside");
    fs::write(&outside, "attacker").expect("w");

    fs::remove_file(&ip).expect("rm");
    std::os::unix::fs::symlink(&outside, &ip).expect("sym");
    assert!(matches!(
        root.scheduler().schedule_and_trigger(&intent),
        Err(UpgradeStoreError::Unsafe(_))
    ));
    fs::remove_file(&ip).expect("rm");
    write_atomic(&ip, &trusted, TempOwner::Schedule).expect("restore");

    fs::set_permissions(&sp, fs::Permissions::from_mode(0o644)).expect("644");
    assert!(matches!(
        root.scheduler().schedule_and_trigger(&intent),
        Err(UpgradeStoreError::Unsafe(_))
    ));
    fs::set_permissions(&sp, fs::Permissions::from_mode(0o600)).expect("600");

    let hl = op_dir.join("state.link");
    fs::hard_link(&sp, &hl).expect("hl");
    assert!(matches!(
        root.scheduler().schedule_and_trigger(&intent),
        Err(UpgradeStoreError::Unsafe(_))
    ));
    fs::remove_file(&hl).expect("rm");
}

#[test]
fn runner_input_validation_terminal_replay_and_spool_failure_recovery() {
    let _guard = lock_runner_tests();
    let sys_runner = UpgradeRunner::new(PathBuf::from("/"));
    for bad in ["../etc/shadow", "not-a-uuid", ""] {
        assert!(matches!(
            sys_runner.run(bad),
            Err(UpgradeStoreError::Invalid(_))
        ));
    }

    let root = TempRoot::new("term-spool");
    let t_intent = new_intent([9; 32]);
    root.scheduler().schedule_and_trigger(&t_intent).expect("s");
    root.force_state(&t_intent.operation_id, OperationState::Succeeded);
    assert_eq!(
        root.runner()
            .run(&t_intent.operation_id.to_string())
            .expect("rep"),
        OperationState::Succeeded
    );

    let f_intent = new_intent([0x0b; 32]);
    root.scheduler().schedule_and_trigger(&f_intent).expect("s");
    let op_dir = root.ops().join(f_intent.operation_id.to_string());
    let stale = op_dir.join(format!(".result.run.tmp.{}", Uuid::now_v7()));
    let sched_tmp = op_dir.join(format!(".state.schedule.tmp.{}", Uuid::now_v7()));
    plant_tmp(&stale, b"incomplete");
    plant_tmp(&sched_tmp, b"accepted\n");
    assert!(matches!(
        root.runner().run(&f_intent.operation_id.to_string()),
        Err(UpgradeStoreError::Io(_))
    ));
    assert!(!stale.exists() && sched_tmp.exists());
    assert_eq!(load_state(&op_dir).expect("st"), OperationState::Failed);
}

#[test]
fn read_recent_results_ordering_drift_and_permissions() {
    let root = TempRoot::new("recent");
    let (a, b) = (new_intent([0x43; 32]), new_intent([0x44; 32]));
    root.scheduler().schedule_and_trigger(&a).expect("a");
    write_terminal(
        &root.ops().join(a.operation_id.to_string()),
        &a,
        OperationState::Succeeded,
        "activated",
    )
    .expect("ta");
    root.scheduler().schedule_and_trigger(&b).expect("b");
    write_terminal(
        &root.ops().join(b.operation_id.to_string()),
        &b,
        OperationState::Failed,
        "spool missing",
    )
    .expect("tb");

    let res = read_recent_results(&root.ops(), 8).expect("res");
    assert_eq!(
        (res.len(), res[0].operation_id, res[1].operation_id),
        (2, b.operation_id, a.operation_id)
    );

    let a_res = root.ops().join(a.operation_id.to_string()).join("result");
    fs::set_permissions(&a_res, fs::Permissions::from_mode(0o644)).expect("644");
    assert!(matches!(
        read_recent_results(&root.ops(), 8),
        Err(UpgradeStoreError::Unsafe(_))
    ));
    fs::set_permissions(&a_res, fs::Permissions::from_mode(0o600)).expect("600");

    let mut drifted = a.clone();
    drifted.operation_id = Uuid::now_v7();
    write_terminal(
        &root.ops().join(a.operation_id.to_string()),
        &drifted,
        OperationState::Succeeded,
        "drift",
    )
    .expect("td");
    assert!(matches!(
        read_recent_results(&root.ops(), 8),
        Err(UpgradeStoreError::Unsafe(_))
    ));

    let (resilient, skipped) = read_recent_results_resilient(&root.ops(), 8).expect("resilient");
    assert_eq!(
        (skipped, resilient.len(), resilient[0].operation_id),
        (1, 1, b.operation_id)
    );
}
