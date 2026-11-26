use std::fs::{self, OpenOptions};
use std::io::Write as _;
use std::os::unix::fs::{DirBuilderExt as _, OpenOptionsExt as _, PermissionsExt as _};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Barrier, Mutex, PoisonError};
use std::time::Duration;

use super::*;
use crate::fs::{
    TempOwner, UpgradeLock, ensure_operations_root, open_spool_file, sha256_reader, write_atomic,
};
use crate::runner::{NativePackage, validate_upgrade_base_url, write_terminal};
use crate::scheduler::{load_intent, load_state};
use ocservia_test_support::PrivateTempDir;

// Serialize tests that spawn child processes (`Command::new`) while holding
// non-blocking `flock` execution locks: concurrent threads calling `fork()`
// transiently inherit parent file descriptors prior to `execve()`/`O_CLOEXEC`,
// which otherwise causes false `EWOULDBLOCK` (`ActiveConflict`) on immediate
// lock re-acquisition in a sibling test.
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
    new_intent_ver(
        "1.2.3",
        sha,
        ocservia_contracts::agent_upgrade::runtime_architecture().expect("arch"),
    )
}
fn new_intent_ver(ver: &str, sha: [u8; 32], arch: &str) -> UpgradeIntent {
    UpgradeIntent::new(
        *Uuid::now_v7().as_bytes(),
        *Uuid::now_v7().as_bytes(),
        ver,
        sha,
        arch,
        [0x55; 32],
    )
    .expect("intent")
}
fn record_mode(path: &Path) -> u32 {
    fs::symlink_metadata(path)
        .expect("meta")
        .permissions()
        .mode()
        & 0o777
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
fn scheduling_idempotency_conflict_lock_and_active_slot() {
    let root = TempRoot::new("sched");
    let intent = new_intent([0x43; 32]);
    assert_eq!(
        root.scheduler().schedule_and_trigger(&intent).expect("1st"),
        ScheduleOutcome::Scheduled
    );
    assert_eq!(
        root.scheduler().schedule_and_trigger(&intent).expect("dup"),
        ScheduleOutcome::AlreadyScheduled
    );
    let op_dir = root.ops().join(intent.operation_id.to_string());
    assert_eq!(
        (
            record_mode(&op_dir.join("intent")),
            record_mode(&op_dir.join("state"))
        ),
        (0o600, 0o600)
    );
    assert!(fs::symlink_metadata(&op_dir).expect("dir").is_dir());

    let mut conflict = intent.clone();
    conflict.package_sha256 = [0x44; 32];
    assert!(matches!(
        root.scheduler().schedule_and_trigger(&conflict),
        Err(UpgradeStoreError::IdentityConflict)
    ));
    assert_eq!(
        load_intent(&op_dir).expect("persisted").package_sha256,
        [0x43; 32]
    );

    // Exact replay while execution lock is held; another active op is rejected until terminal.
    {
        let _exec = UpgradeLock::acquire_execution(&root.ops()).expect("lock");
        assert_eq!(
            root.scheduler()
                .schedule_and_trigger(&intent)
                .expect("replay"),
            ScheduleOutcome::AlreadyScheduled
        );
    }
    let second = new_intent([0x45; 32]);
    assert!(matches!(
        root.scheduler().schedule_and_trigger(&second),
        Err(UpgradeStoreError::ActiveConflict)
    ));
    root.force_state(&intent.operation_id, OperationState::Succeeded);
    root.scheduler()
        .schedule_and_trigger(&second)
        .expect("freed");
    root.force_state(&second.operation_id, OperationState::Succeeded);

    // Large terminal history does not block scheduling.
    for i in 0..65 {
        let h = new_intent([i; 32]);
        root.scheduler().schedule_and_trigger(&h).expect("hist");
        root.force_state(&h.operation_id, OperationState::Succeeded);
    }
    root.scheduler()
        .schedule_and_trigger(&new_intent([0xff; 32]))
        .expect("after history");
}

#[test]
fn partial_operations_do_not_block_new_work_or_result_reads() {
    let root = TempRoot::new("partial-sibling");
    let partial = new_intent([1; 32]);
    let partial_dir = root.ops().join(partial.operation_id.to_string());
    ensure_operations_root(&partial_dir).expect("operation directory");
    assert!(
        read_recent_results(&root.ops(), 1)
            .expect("read partial")
            .is_empty()
    );
    let current = new_intent([2; 32]);
    root.scheduler()
        .schedule_and_trigger(&current)
        .expect("schedule past partial sibling");
    write_atomic(
        &partial_dir.join("intent"),
        partial.render().as_bytes(),
        TempOwner::Schedule,
    )
    .expect("persist partial intent");
    assert!(matches!(
        root.scheduler().schedule_and_trigger(&partial),
        Err(UpgradeStoreError::ActiveConflict)
    ));
    assert!(
        read_recent_results(&root.ops(), 1)
            .expect("read partial intent")
            .is_empty()
    );
}

#[test]
fn scheduling_crash_recovery_and_temp_ownership() {
    let root = TempRoot::new("partial");
    let first = new_intent([1; 32]);
    fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(root.ops().join(first.operation_id.to_string()))
        .expect("d1");
    assert_eq!(
        root.scheduler()
            .schedule_and_trigger(&first)
            .expect("resume"),
        ScheduleOutcome::Scheduled
    );
    root.force_state(&first.operation_id, OperationState::Succeeded);

    let second = new_intent([2; 32]);
    let d2 = root.ops().join(second.operation_id.to_string());
    fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(&d2)
        .expect("d2");
    write_atomic(
        &d2.join("intent"),
        second.render().as_bytes(),
        TempOwner::Schedule,
    )
    .expect("intent");
    let stale = d2.join(format!(".state.schedule.tmp.{}", Uuid::now_v7()));
    plant_tmp(&stale, b"accepted\n");
    assert_eq!(
        root.scheduler()
            .schedule_and_trigger(&second)
            .expect("state"),
        ScheduleOutcome::Scheduled
    );
    assert!(!stale.exists());
    assert_eq!(load_state(&d2).expect("st"), OperationState::Accepted);

    let mut conflict = second.clone();
    conflict.target_version = "1.2.4".into();
    assert!(matches!(
        root.scheduler().schedule_and_trigger(&conflict),
        Err(UpgradeStoreError::IdentityConflict)
    ));

    // Runner temporaries are preserved while scheduler temporaries are cleaned on replay.
    let r_state = d2.join(format!(".state.run.tmp.{}", Uuid::now_v7()));
    let r_res = d2.join(format!(".result.run.tmp.{}", Uuid::now_v7()));
    let s_state = d2.join(format!(".state.schedule.tmp.{}", Uuid::now_v7()));
    plant_tmp(&r_state, b"running\n");
    plant_tmp(&r_res, b"partial");
    plant_tmp(&s_state, b"accepted\n");
    assert_eq!(
        root.scheduler()
            .schedule_and_trigger(&second)
            .expect("replay"),
        ScheduleOutcome::AlreadyScheduled
    );
    assert!(r_state.exists() && r_res.exists() && !s_state.exists());
    root.force_state(&second.operation_id, OperationState::Succeeded);

    // Orphan state without intent fails closed.
    let orphan = new_intent([3; 32]);
    let od = root.ops().join(orphan.operation_id.to_string());
    fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(&od)
        .expect("od");
    write_atomic(&od.join("state"), b"accepted\n", TempOwner::Schedule).expect("os");
    assert!(matches!(
        root.scheduler().schedule_and_trigger(&orphan),
        Err(UpgradeStoreError::Unsafe(_))
    ));
}

#[test]
fn concurrent_scheduling_enforces_single_active_and_exact_replay() {
    for same_intent in [false, true] {
        let root = TempRoot::new(if same_intent { "conc-rep" } else { "conc-act" });
        let a = new_intent([0x41; 32]);
        let b = if same_intent {
            a.clone()
        } else {
            new_intent([0x42; 32])
        };
        let barrier = Arc::new(Barrier::new(3));
        let handles = [a, b].map(|intent| {
            let ops = root.ops();
            let br = Arc::clone(&barrier);
            std::thread::spawn(move || {
                br.wait();
                UpgradeScheduler::new(ops, UpgradeTrigger::Disabled).schedule_and_trigger(&intent)
            })
        });
        barrier.wait();
        let outs = handles.map(|h| h.join().expect("join"));
        assert_eq!(
            outs.iter()
                .filter(|o| matches!(o, Ok(ScheduleOutcome::Scheduled)))
                .count(),
            1
        );
        if same_intent {
            assert_eq!(
                outs.iter()
                    .filter(|o| matches!(o, Ok(ScheduleOutcome::AlreadyScheduled)))
                    .count(),
                1
            );
        } else {
            assert_eq!(
                outs.iter()
                    .filter(|o| matches!(o, Err(UpgradeStoreError::ActiveConflict)))
                    .count(),
                1
            );
        }
        let active = fs::read_dir(root.ops())
            .expect("rd")
            .filter_map(Result::ok)
            .filter(|e| Uuid::parse_str(&e.file_name().to_string_lossy()).is_ok())
            .filter(|e| {
                matches!(
                    load_state(&e.path()),
                    Ok(OperationState::Accepted | OperationState::Running)
                )
            })
            .count();
        assert_eq!(active, 1);
    }
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

    fs::set_permissions(root.ops(), fs::Permissions::from_mode(0o770)).expect("770");
    assert!(matches!(
        root.scheduler().schedule_and_trigger(&new_intent([8; 32])),
        Err(UpgradeStoreError::Unsafe(_))
    ));
    fs::set_permissions(root.ops(), fs::Permissions::from_mode(0o700)).expect("700");
}

#[test]
fn runner_input_validation_terminal_replay_and_spool_failure_recovery() {
    let _guard = lock_runner_tests();
    let sys_runner = UpgradeRunner::new(PathBuf::from("/"));
    for bad in [
        "../etc/shadow",
        "018f0c2e-7b1a-7c3d-8e9f-0123456789aB",
        "018f0c2e7b1a7c3d8e9f0123456789ab",
        "not-a-uuid",
        "",
    ] {
        assert!(
            matches!(sys_runner.run(bad), Err(UpgradeStoreError::Invalid(_))),
            "{bad}"
        );
    }

    let root = TempRoot::new("term-spool");
    let t_intent = new_intent([9; 32]);
    root.scheduler().schedule_and_trigger(&t_intent).expect("s");
    for state in [OperationState::Succeeded, OperationState::RolledBack] {
        root.force_state(&t_intent.operation_id, state);
        assert_eq!(
            root.runner()
                .run(&t_intent.operation_id.to_string())
                .expect("rep"),
            state
        );
    }

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
    assert!(
        fs::read_to_string(op_dir.join("result"))
            .expect("res")
            .contains("state=failed")
    );
}

#[test]
fn native_install_replays_preserve_rollback_package_and_probe_each_service() {
    let _guard = lock_runner_tests();
    let arch = ocservia_contracts::agent_upgrade::runtime_architecture().expect("architecture");
    let root = TempRoot::new("native");
    let runner = root.runner();
    let spool = runner.spool_dir();
    ensure_operations_root(&spool).expect("cache");
    let status = root.path().join("var/lib/dpkg/status");
    fs::create_dir_all(status.parent().unwrap()).expect("dpkg db");
    fs::write(&status, b"1.0.0").expect("installed");
    let bin = root.path().join("usr/bin");
    fs::create_dir_all(&bin).expect("bin");
    for (name, commands) in [
        (
            "dpkg-query",
            "printf 'install ok installed %s-1' \"$(cat \"$status\")\"",
        ),
        ("dpkg", "test \"$1\" = -i && cat \"$2\" > \"$status\""),
        (
            "systemctl",
            "test \"$1\" != restart; test \"$1\" != is-active || test \"$3\" != ocservia-agent.service || test ! -f \"$status.fail\" || test \"$(cat \"$status\")\" = 1.0.0",
        ),
    ] {
        let path = bin.join(name);
        fs::write(
            &path,
            format!(
                "#!/bin/sh\nset -eu\nstatus='{}'\n{commands}\n",
                status.display()
            ),
        )
        .expect("test tool");
        fs::set_permissions(&path, fs::Permissions::from_mode(0o755)).expect("executable");
    }
    let old = spool.join(NativePackage::Deb.filename("1.0.0", arch));
    let target = spool.join(NativePackage::Deb.filename("1.2.3", arch));
    fs::write(&old, b"1.0.0").unwrap();
    fs::write(&target, b"1.2.3").unwrap();
    fs::set_permissions(&old, fs::Permissions::from_mode(0o600)).unwrap();
    fs::set_permissions(&target, fs::Permissions::from_mode(0o600)).unwrap();
    let intent = new_intent(
        sha256_reader(&mut open_spool_file(&target).expect("target")).expect("authorized digest"),
    );
    root.scheduler()
        .schedule_and_trigger(&intent)
        .expect("schedule");
    let operation = intent.operation_id.to_string();
    assert_eq!(
        runner.run(&operation).expect("upgrade"),
        OperationState::Succeeded
    );
    assert_eq!(fs::read_to_string(&status).unwrap(), "1.2.3");
    root.force_state(&intent.operation_id, OperationState::Running);
    fs::write(status.with_extension("fail"), b"").expect("fail target health");
    assert_eq!(
        runner.run(&operation).expect("rollback"),
        OperationState::RolledBack
    );
    assert_eq!(fs::read_to_string(&status).unwrap(), "1.0.0");
    assert_eq!(
        runner.run(&operation).expect("terminal replay"),
        OperationState::RolledBack
    );

    let unauthorized = new_intent([0; 32]);
    root.scheduler()
        .schedule_and_trigger(&unauthorized)
        .expect("schedule");
    assert!(matches!(
        runner.run(&unauthorized.operation_id.to_string()),
        Err(UpgradeStoreError::Package(_))
    ));
    assert_eq!(fs::read_to_string(&status).unwrap(), "1.0.0");
    assert_eq!(
        NativePackage::Rpm.filename("2.0.0-rc.1", "arm64"),
        "ocservia-agent-2.0.0-rc.1-1.aarch64.rpm"
    );
}

#[test]
fn read_recent_results_ordering_drift_and_permissions() {
    let root = TempRoot::new("recent");
    let (a, b, inflight) = (
        new_intent([0x43; 32]),
        new_intent([0x44; 32]),
        new_intent([0x45; 32]),
    );
    root.scheduler().schedule_and_trigger(&a).expect("a");
    write_terminal(
        &root.ops().join(a.operation_id.to_string()),
        &a,
        OperationState::Succeeded,
        "activated release",
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
    root.scheduler().schedule_and_trigger(&inflight).expect("c");

    let res = read_recent_results(&root.ops(), 8).expect("res");
    assert_eq!(res.len(), 2);
    assert_eq!(
        (res[0].operation_id, res[0].state),
        (b.operation_id, OperationState::Failed)
    );
    assert!(res[0].completed_unix > 0);
    assert_eq!(
        (
            res[1].operation_id,
            res[1].state,
            res[1].target_version.as_str(),
            res[1].detail.as_str()
        ),
        (
            a.operation_id,
            OperationState::Succeeded,
            "1.2.3",
            "activated release"
        )
    );
    let bounded = read_recent_results(&root.ops(), 1).expect("b");
    assert_eq!(
        (bounded.len(), bounded[0].operation_id),
        (1, b.operation_id)
    );

    // Unsafe record mode fails closed.
    let a_res = root.ops().join(a.operation_id.to_string()).join("result");
    fs::set_permissions(&a_res, fs::Permissions::from_mode(0o644)).expect("644");
    assert!(matches!(
        read_recent_results(&root.ops(), 8),
        Err(UpgradeStoreError::Unsafe(_))
    ));
    fs::set_permissions(&a_res, fs::Permissions::from_mode(0o600)).expect("600");

    // Identity drift fails closed.
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

    // Resilient reader isolates the drifted sibling and still returns valid b.
    let (resilient, skipped) = read_recent_results_resilient(&root.ops(), 8).expect("resilient");
    assert_eq!(skipped, 1);
    assert_eq!(resilient.len(), 1);
    assert_eq!(resilient[0].operation_id, b.operation_id);
}

#[test]
fn upgrade_base_url_override_validation() {
    assert_eq!(
        validate_upgrade_base_url("https://mirror.example.com:8443/ocservia/").expect("valid"),
        "https://mirror.example.com:8443/ocservia"
    );
    for bad in [
        "http://mirror.example.com/ocservia",
        "https://user:pass@mirror.example.com/ocservia",
        "https://mirror.example.com/ocservia?token=x",
        "https://mirror.example.com/ocservia#frag",
        "https://mirror.example.com/ocservia/../other",
        "https://mirror.example.com/ocservia\n",
        "https://",
    ] {
        assert!(
            matches!(
                validate_upgrade_base_url(bad),
                Err(UpgradeStoreError::Unsafe(_))
            ),
            "unexpectedly accepted {bad}"
        );
    }
}

#[test]
fn transient_download_retries_before_terminal_and_cleans_orphan_downloads() {
    let _guard = lock_runner_tests();
    let arch = ocservia_contracts::agent_upgrade::runtime_architecture().expect("architecture");
    let root = TempRoot::new("dl-retry");
    let runner = root
        .runner()
        .with_base_url_override(Some("https://mirror.example.test/releases".to_owned()));
    let spool = runner.spool_dir();
    ensure_operations_root(&spool).expect("spool");
    let stale_dl = spool.join(format!(".download-{}", Uuid::now_v7()));
    plant_tmp(&stale_dl, b"partial-bytes");

    let status = root.path().join("var/lib/dpkg/status");
    fs::create_dir_all(status.parent().unwrap()).expect("dpkg");
    fs::write(&status, b"1.0.0").expect("status");
    let bin = root.path().join("usr/bin");
    fs::create_dir_all(&bin).expect("bin");
    for (name, script) in [
        (
            "dpkg-query",
            format!(
                "#!/bin/sh\nprintf 'install ok installed %s-1' \"$(cat '{}')\"\n",
                status.display()
            ),
        ),
        (
            "curl",
            "#!/bin/sh\necho 'network timeout' >&2\nexit 28\n".to_owned(),
        ),
    ] {
        let path = bin.join(name);
        fs::write(&path, script).expect("tool");
        fs::set_permissions(&path, fs::Permissions::from_mode(0o755)).expect("chmod");
    }
    let old = spool.join(NativePackage::Deb.filename("1.0.0", arch));
    fs::write(&old, b"1.0.0").unwrap();
    fs::set_permissions(&old, fs::Permissions::from_mode(0o600)).unwrap();

    let intent = new_intent([0x77; 32]);
    root.scheduler()
        .schedule_and_trigger(&intent)
        .expect("sched");
    let op_str = intent.operation_id.to_string();
    let op_dir = root.ops().join(&op_str);

    // Attempt 1 & 2 stay non-terminal (accepted) so systemd Restart=on-failure retries.
    assert!(matches!(
        runner.run(&op_str),
        Err(UpgradeStoreError::Lifecycle(_))
    ));
    assert!(!stale_dl.exists(), "orphan .download-* must be cleaned");
    assert_eq!(load_state(&op_dir).expect("s1"), OperationState::Accepted);

    assert!(matches!(
        runner.run(&op_str),
        Err(UpgradeStoreError::Lifecycle(_))
    ));
    assert_eq!(load_state(&op_dir).expect("s2"), OperationState::Accepted);

    // Attempt 3 exhausts retry budget and writes terminal failed.
    assert!(matches!(
        runner.run(&op_str),
        Err(UpgradeStoreError::Lifecycle(_))
    ));
    assert_eq!(load_state(&op_dir).expect("s3"), OperationState::Failed);

    // Bad digest after download is immediate terminal failed (no retries).
    let bad_curl = bin.join("curl");
    fs::write(
        &bad_curl,
        "#!/bin/sh\nout=''\nwhile [ $# -gt 0 ]; do if [ \"$1\" = '--output' ]; then out=\"$2\"; shift 2; else shift; fi; done\nprintf 'tampered' > \"$out\"\n",
    )
    .expect("curl write");
    fs::set_permissions(&bad_curl, fs::Permissions::from_mode(0o755)).expect("chmod");
    let digest_intent = new_intent([0x88; 32]);
    root.scheduler()
        .schedule_and_trigger(&digest_intent)
        .expect("sched2");
    let digest_dir = root.ops().join(digest_intent.operation_id.to_string());
    assert!(matches!(
        runner.run(&digest_intent.operation_id.to_string()),
        Err(UpgradeStoreError::Package(_))
    ));
    assert_eq!(
        load_state(&digest_dir).expect("digest terminal"),
        OperationState::Failed
    );
}

#[test]
fn install_and_probe_verifies_socket_and_enabled_network_audit() {
    let _guard = lock_runner_tests();
    let arch = ocservia_contracts::agent_upgrade::runtime_architecture().expect("architecture");
    let root = TempRoot::new("probe-extra");
    let sock_path = root.path().join("privd.sock");
    let spool = root.runner().spool_dir();
    ensure_operations_root(&spool).expect("spool");
    let status = root.path().join("var/lib/dpkg/status");
    fs::create_dir_all(status.parent().unwrap()).expect("dpkg");
    fs::write(&status, b"1.0.0").expect("status");
    let audit_fail = root.path().join("audit-fail");
    let bin = root.path().join("usr/bin");
    fs::create_dir_all(&bin).expect("bin");
    let systemctl_script = format!(
        "#!/bin/sh\nset -eu\nstatus='{}'\naudit_fail='{}'\ncmd=\"$1\"\ncase \"$cmd\" in\n  start) exit 0 ;;\n  is-enabled) test -f \"$audit_fail\" ;;\n  is-active)\n    if [ \"$3\" = ocservia-network-audit.service ] && [ -f \"$audit_fail\" ]; then\n      test \"$(cat \"$status\")\" = 1.0.0\n    fi\n    exit 0 ;;\n  *) exit 1 ;;\nesac\n",
        status.display(),
        audit_fail.display()
    );
    for (name, script) in [
        (
            "dpkg-query",
            format!(
                "#!/bin/sh\nprintf 'install ok installed %s-1' \"$(cat '{}')\"\n",
                status.display()
            ),
        ),
        (
            "dpkg",
            format!(
                "#!/bin/sh\ntest \"$1\" = -i && cat \"$2\" > '{}'\n",
                status.display()
            ),
        ),
        ("systemctl", systemctl_script),
    ] {
        let path = bin.join(name);
        fs::write(&path, script).expect("tool");
        fs::set_permissions(&path, fs::Permissions::from_mode(0o755)).expect("chmod");
    }
    let old = spool.join(NativePackage::Deb.filename("1.0.0", arch));
    let target = spool.join(NativePackage::Deb.filename("1.2.3", arch));
    fs::write(&old, b"1.0.0").unwrap();
    fs::write(&target, b"1.2.3").unwrap();
    fs::set_permissions(&old, fs::Permissions::from_mode(0o600)).unwrap();
    fs::set_permissions(&target, fs::Permissions::from_mode(0o600)).unwrap();
    let sha = sha256_reader(&mut open_spool_file(&target).expect("target")).expect("sha");

    // Bind a real UDS listener so the UDS connect probe passes; enabled audit missing fails -> rollback.
    let listener = std::os::unix::net::UnixListener::bind(&sock_path).expect("bind");
    let runner = root
        .runner()
        .with_probe_schedule(2, Duration::from_millis(5))
        .with_privd_socket(Some(sock_path.clone()));

    fs::write(&audit_fail, b"1").unwrap();
    let fail_intent = new_intent(sha);
    root.scheduler()
        .schedule_and_trigger(&fail_intent)
        .expect("sched1");
    assert_eq!(
        runner
            .run(&fail_intent.operation_id.to_string())
            .expect("rb"),
        OperationState::RolledBack
    );

    // Disabled network-audit + reachable UDS -> Succeeded.
    fs::remove_file(&audit_fail).unwrap();
    let ok_intent = new_intent(sha);
    root.scheduler()
        .schedule_and_trigger(&ok_intent)
        .expect("sched2");
    assert_eq!(
        runner.run(&ok_intent.operation_id.to_string()).expect("ok"),
        OperationState::Succeeded
    );
    drop(listener);
}
