#![allow(
    clippy::format_collect,
    clippy::many_single_char_names,
    clippy::similar_names,
    clippy::too_many_lines
)]

use std::os::unix::fs::PermissionsExt;
use std::process::{Command, Stdio};

use super::effect_store::*;
use super::user_file::*;
use super::*;

const TEST_COMMAND_ID: [u8; 16] = [1, 2, 3, 4, 5, 6, 0x70, 8, 0x80, 10, 11, 12, 13, 14, 15, 16];
const TEST_IDEMPOTENCY_KEY: [u8; 16] = [16, 15, 14, 13, 12, 11, 0x70, 9, 0x80, 7, 6, 5, 4, 3, 2, 1];
const TEST_PAYLOAD_HASH: [u8; 32] = [9; 32];

pub(super) fn test_effect() -> EffectIdentity<'static> {
    EffectIdentity {
        command_id: &TEST_COMMAND_ID,
        idempotency_key: &TEST_IDEMPOTENCY_KEY,
        semantic_payload_sha256: &TEST_PAYLOAD_HASH,
        expires_at_unix_seconds: i64::MAX,
    }
}

pub(super) fn write_executable(path: &Path, bytes: impl AsRef<[u8]>) {
    // Avoid retaining writable script descriptors across concurrent forks (ETXTBSY).
    let mut writer = Command::new("tee")
        .arg(path)
        .stdin(Stdio::piped())
        .stdout(Stdio::null())
        .spawn()
        .expect("start fixture writer");
    writer
        .stdin
        .take()
        .expect("writer stdin")
        .write_all(bytes.as_ref())
        .expect("write fixture program");
    assert!(writer.wait().expect("wait writer").success());
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700))
        .expect("chmod fixture executable");
}

struct TempDir(PathBuf);

impl TempDir {
    fn new(prefix: &str) -> Self {
        let path = std::env::temp_dir().join(format!("ocservia-{prefix}-{}", Uuid::now_v7()));
        std::fs::create_dir(&path).expect("create temp dir");
        Self(path)
    }

    fn path(&self) -> &Path {
        &self.0
    }

    fn join(&self, name: impl AsRef<Path>) -> PathBuf {
        self.0.join(name)
    }

    fn exec(&self, name: &str, body: &str) -> PathBuf {
        let path = self.join(name);
        write_executable(&path, format!("#!/bin/sh\n{body}\n"));
        path
    }

    fn effect_resources(&self) -> FixedResources {
        FixedResources::default()
            .with_effect_store(self.join("effects.sqlite3"), self.join("effects.key"))
            .expect("effect resources")
    }

    fn user_resources(&self, ocpasswd: PathBuf, openssl: PathBuf) -> FixedResources {
        FixedResources::default()
            .with_user_resources(
                ocpasswd,
                self.join("ocpasswd"),
                openssl,
                self.join("key.pem"),
                "test-key".to_owned(),
            )
            .expect("user resources")
            .with_effect_store(self.join("effects.sqlite3"), self.join("effects.key"))
            .expect("effect resources")
    }

    fn user_adapter(&self, ocpasswd: PathBuf, openssl: PathBuf, limits: Limits) -> Adapter {
        Adapter::new(self.user_resources(ocpasswd, openssl), limits)
    }
}

impl Drop for TempDir {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

fn write_user_fixture(path: &Path, bytes: impl AsRef<[u8]>) {
    std::fs::write(path, bytes).expect("write ocpasswd fixture");
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(USER_FILE_MODE))
        .expect("chmod ocpasswd fixture");
}

fn staging_files(directory: &Path) -> Vec<PathBuf> {
    std::fs::read_dir(directory)
        .expect("read staging directory")
        .filter_map(Result::ok)
        .map(|e| e.path())
        .filter(|p| {
            p.file_name()
                .and_then(|n| n.to_str())
                .is_some_and(|n| n.starts_with(".ocpasswd.ocservia-"))
        })
        .collect()
}

fn openssl_ok(args: &[&std::ffi::OsStr]) {
    assert!(
        Command::new("/usr/bin/openssl")
            .args(args)
            .status()
            .expect("run openssl")
            .success()
    );
}

fn seal_rsa_oaep(public_key: &Path, secret: &[u8]) -> Vec<u8> {
    let mut child = Command::new("/usr/bin/openssl")
        .args(["pkeyutl", "-encrypt", "-pubin", "-inkey"])
        .arg(public_key)
        .args([
            "-pkeyopt",
            "rsa_padding_mode:oaep",
            "-pkeyopt",
            "rsa_oaep_md:sha256",
        ])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .spawn()
        .expect("spawn openssl pkeyutl");
    child
        .stdin
        .take()
        .expect("stdin")
        .write_all(secret)
        .expect("write secret");
    let out = child.wait_with_output().expect("wait openssl pkeyutl");
    assert!(out.status.success());
    out.stdout
}

fn expire_artifact_grant(
    resources: &FixedResources,
    artifact_id: &Uuid,
    expired_at: i64,
) -> EffectStore {
    let store = EffectStore::open_for_mutation(resources).expect("open artifact store");
    let mut rec = query_artifact(&store.connection, artifact_id.as_bytes())
        .expect("query artifact")
        .expect("artifact record");
    rec.grant_expires_at = Some(expired_at);
    rec.record_hmac = authenticate_artifact_record(&store.hmac_key, &rec);
    store
        .connection
        .execute(
            "UPDATE certificate_artifacts SET grant_expires_at=?2,record_hmac=?3 WHERE artifact_id=?1",
            params![rec.artifact_id.as_slice(), expired_at, rec.record_hmac.as_slice()],
        )
        .expect("expire grant");
    store
}

#[test]
fn version_fixtures_and_malformed_output() {
    for (raw, want) in [
        (b"ocserv 1.2.4\n".as_slice(), "1.2.4"),
        (
            b"OpenConnect VPN Server 1.3.0\n\nCompiled with: seccomp, PAM\nGnuTLS version: 3.8.9\n",
            "1.3.0",
        ),
        (b"ocserv 1.4.1\n", "1.4.1"),
        (b"OpenConnect VPN Server 1.5.0\n", "1.5.0"),
    ] {
        assert_eq!(parse_version(raw).expect("valid version").version, want);
    }
    for prefix in ["ocserv", "OpenConnect VPN Server"] {
        assert_eq!(
            parse_version(format!("{prefix} 1.6.0\n").as_bytes())
                .expect("1.6.0")
                .version,
            "1.6.0"
        );
        for bad in [
            "1.5.1",
            "1.6.1",
            "1.7.0",
            "2.0.0",
            "1.6",
            "1.6.0.1",
            "1.6.0-rc.1",
            "1.6.00",
            "1.06.0",
        ] {
            assert!(
                parse_version(format!("{prefix} {bad}\n").as_bytes()).is_err(),
                "unreviewed version {bad} accepted"
            );
        }
    }
    assert!(parse_version(b"ocserv development\n").is_err());
    assert!(parse_version(&[0xff]).is_err());
}

#[tokio::test]
async fn host_preflight_reuses_snapshot_checks_without_mutating_host() {
    let dir = TempDir::new("preflight");
    let program = dir.exec(
        "snapshot-command",
        "case \"$1\" in\n--version) echo 'ocserv 1.3.0';;\nshow) printf 'LoadState=loaded\\nActiveState=active\\nSubState=running\\n';;\n--json) echo '[]';;\n*) exit 99;;\nesac",
    );
    let config = dir.join("ocserv.conf");
    std::fs::write(&config, b"tcp-port = 443\n").expect("config");
    let users = dir.join("ocpasswd");
    write_user_fixture(&users, b"alice::private-hash");
    let resources = FixedResources::new(
        program.clone(),
        program.clone(),
        program,
        config.clone(),
        PathBuf::from("/proc/sys/kernel/random/boot_id"),
    )
    .expect("resources")
    .with_user_resources(
        PathBuf::from("/bin/false"),
        users.clone(),
        PathBuf::from("/bin/false"),
        dir.join("absent-key.pem"),
        "user-key".into(),
    )
    .expect("user resources")
    .with_effect_store(dir.join("absent.sqlite3"), dir.join("absent.key"))
    .expect("store");
    let adapter = Adapter::new(resources, Limits::default());
    adapter.host_preflight().await.expect("healthy snapshot");
    assert_eq!(std::fs::read(&config).expect("config"), b"tcp-port = 443\n");
    assert_eq!(
        std::fs::read(&users).expect("users"),
        b"alice::private-hash"
    );
    for absent in ["absent.sqlite3", "absent.key", "absent-key.pem"] {
        assert!(!dir.join(absent).exists());
    }
    std::fs::set_permissions(&users, std::fs::Permissions::from_mode(0o644)).expect("mode");
    assert!(matches!(
        adapter.host_preflight().await,
        Err(AdapterError::ResourceMetadata { reason: "mode", .. })
    ));
}

#[tokio::test]
async fn reads_packaged_version_banner_from_stderr() {
    let dir = TempDir::new("version");
    let program = dir.exec(
        "ocserv-version",
        "printf 'OpenConnect VPN Server 1.3.0\\n' >&2",
    );
    let resources = FixedResources::new(
        PathBuf::from("/bin/false"),
        program,
        PathBuf::from("/bin/false"),
        PathBuf::from("/etc/hosts"),
        PathBuf::from("/proc/sys/kernel/random/boot_id"),
    )
    .expect("resources");
    let version = Adapter::new(resources, Limits::default())
        .ocserv_version()
        .await
        .expect("stderr version banner");
    assert_eq!(version.version, "1.3.0");
}

#[tokio::test]
async fn stale_config_plan_staging_files_are_cleaned_on_startup() {
    let dir = TempDir::new("plan-clean");
    let config = dir.join("ocserv.conf");
    std::fs::write(&config, b"tcp-port = 444\n").expect("config");
    let resources = FixedResources::new(
        PathBuf::from("/bin/false"),
        PathBuf::from("/bin/false"),
        PathBuf::from("/bin/false"),
        config,
        PathBuf::from("/proc/sys/kernel/random/boot_id"),
    )
    .expect("resources");
    let stale = dir.join(format!(".ocservia-plan-{}", Uuid::now_v7()));
    let lookalike = dir.join(".ocservia-plan-not-a-uuid");
    std::fs::write(&stale, b"stale").expect("stale");
    std::fs::write(&lookalike, b"preserve").expect("lookalike");
    Adapter::new(resources, Limits::default())
        .cleanup_stale_config_plans()
        .await
        .expect("cleanup");
    assert!(!stale.exists() && lookalike.exists());
}

#[test]
fn parses_session_ban_user_and_group_fixtures() {
    for (users, bans) in [
        (
            include_bytes!("../fixtures/ocserv-1.2/users.json").as_slice(),
            include_bytes!("../fixtures/ocserv-1.2/bans.json").as_slice(),
        ),
        (
            include_bytes!("../fixtures/ocserv-1.3/users.json").as_slice(),
            include_bytes!("../fixtures/ocserv-1.3/bans.json").as_slice(),
        ),
        (
            include_bytes!("../fixtures/ocserv-1.4/users.json").as_slice(),
            include_bytes!("../fixtures/ocserv-1.4/bans.json").as_slice(),
        ),
    ] {
        let sessions = parse_sessions(users).expect("sessions");
        assert_eq!(sessions.sessions.len(), 2);
        assert_eq!(sessions.sessions[1].remote_ip, "2001:db8::1");
        let parsed_bans = parse_ip_bans(bans).expect("bans");
        assert_eq!(parsed_bans.bans.len(), 2);
        assert_eq!(parsed_bans.bans[1].seconds_remaining, None);
    }
    let parsed = parse_sessions(br#"[{"ID":12,"Username":"alice","Remote IP":"192.0.2.1","IPv4":"10.0.0.2","Connected at":"2026-08-03 10:00","RX":"123","TX":"456"}]"#).unwrap();
    let item = &parsed.sessions[0];
    assert_eq!(
        item.connected_at.unwrap().to_string(),
        "2026-08-03T10:00:00Z"
    );
    assert_eq!((item.bytes_in, item.bytes_out), (Some(123), Some(456)));
    assert!(parse_ip_bans(b"[{\"IP\":\"192.0.2.1\"},]").is_err());

    let users = parse_user_file(b"alice:*:$6$secret-hash\nbob:x:!$6$disabled\n").expect("users");
    assert_eq!(users.users.len(), 2);
    assert!(users.users[0].enabled && !users.users[1].enabled);
    assert!(!format!("{users:?}").contains("secret-hash"));
    let groups = parse_groups_from_user_file(b"alice:staff:$6$alice\nbob:admins,staff:$6$bob\n")
        .expect("groups");
    assert_eq!(
        groups
            .groups
            .iter()
            .find(|g| g.group_name == "staff")
            .expect("staff")
            .members,
        ["alice", "bob"]
    );
    assert!(parse_user_file(b"../root:hash\n").is_err());
    assert!(parse_groups_from_user_file(b"alice:staff;id:$6$hash\n").is_err());

    let status = parse_service_status(b"LoadState=loaded\nActiveState=active\nSubState=running\n")
        .expect("valid status");
    assert_eq!(status.active_state, "active");
    assert!(parse_service_status(b"ActiveState=active\n").is_err());
}

#[test]
fn user_file_diagnostics_preserve_physical_lines_and_redact_records() {
    for (input, reason, line, fields, first_line) in [
        (
            "\nalice:staff:private-hash:bob:other-hash",
            "field_count",
            2,
            5,
            None,
        ),
        (
            "alice::private-hash\n\nalice:x:other-hash",
            "duplicate_username",
            3,
            3,
            Some(1),
        ),
        ("\nalice::", "empty_hash", 2, 3, None),
        ("../alice::private-hash", "invalid_username", 1, 3, None),
        ("alice:staff;id:private-hash", "invalid_group", 1, 3, None),
    ] {
        let error = parse_user_file(input.as_bytes()).expect_err("invalid record");
        assert!(matches!(
            &error,
            AdapterError::UserFileFormat { reason: r, line: l, fields: f, first_line: first }
            if *r == reason && *l == line && *f == fields && *first == first_line
        ));
        let debug = format!("{error:?}");
        let wire = PrivdError::from(error);
        assert_eq!(wire.kind, i32::from(ErrorKind::MalformedOutput));
        for out in [debug, wire.detail] {
            for secret in ["private-hash", "other-hash", "alice"] {
                assert!(!out.contains(secret));
            }
        }
    }
    for ok in [
        "",
        "\n\n",
        "alice::x",
        "alice::arbitrary-length-hash\n\nbob:x:!",
    ] {
        parse_user_file(ok.as_bytes()).expect("supported records");
    }
}

#[tokio::test]
async fn resource_diagnostics_and_ocpasswd_publish_enforce_metadata_and_parent() {
    let dir = TempDir::new("resource-meta");
    let path = dir.join("ocpasswd");
    assert!(
        read_optional_user_file(&path)
            .await
            .expect("absent")
            .is_empty()
    );
    assert!(matches!(
        read_user_file(&path, false).await,
        Err(AdapterError::ResourceIo {
            reason: "open_failed",
            ..
        })
    ));

    let adapter = dir.user_adapter(
        PathBuf::from("/bin/false"),
        PathBuf::from("/bin/false"),
        Limits::default(),
    );
    let mut staging = adapter
        .stage_user_file(b"alice:staff:$6$hash\n")
        .await
        .expect("stage");
    adapter
        .commit_desired_staging(&mut staging, b"", "user_create", "alice", 1, test_effect())
        .await
        .expect("publish");
    let meta = std::fs::metadata(&path).expect("meta");
    let (uid, gid) = authoritative_user_owner();
    assert_eq!(
        (
            meta.uid(),
            meta.gid(),
            meta.permissions().mode() & 0o777,
            meta.nlink()
        ),
        (uid, gid, USER_FILE_MODE, 1)
    );

    let file = File::open(&path).expect("open");
    assert!(matches!(
        validate_authoritative_user_file(&file, uid + 1, gid),
        Err(AdapterError::ResourceMetadata {
            reason: "owner_uid",
            ..
        })
    ));
    assert!(matches!(
        validate_authoritative_user_file(&file, uid, gid + 1),
        Err(AdapterError::ResourceMetadata {
            reason: "group_gid",
            ..
        })
    ));
    assert!(matches!(
        validate_authoritative_user_file(&File::open(dir.path()).expect("dir"), uid, gid),
        Err(AdapterError::ResourceMetadata {
            reason: "file_type_regular",
            ..
        })
    ));

    std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).expect("0644");
    let err = read_user_file(&path, false).await.expect_err("0644");
    assert!(matches!(
        err,
        AdapterError::ResourceMetadata {
            reason: "mode",
            actual: 0o644,
            expected: 0o600,
            ..
        }
    ));
    assert!(err.to_string().contains("actual=0644 expected=0600"));
    assert!(matches!(
        adapter.user_list().await,
        Err(AdapterError::ResourceMetadata { .. })
    ));
    std::fs::set_permissions(&path, std::fs::Permissions::from_mode(USER_FILE_MODE)).expect("0600");

    let link = dir.join("link");
    std::fs::hard_link(&path, &link).expect("hardlink");
    assert!(matches!(
        read_user_file(&path, false).await,
        Err(AdapterError::ResourceMetadata {
            reason: "link_count",
            ..
        })
    ));
    assert!(matches!(
        adapter.user_list().await,
        Err(AdapterError::ResourceMetadata { .. })
    ));
    std::fs::remove_file(&link).expect("rm hardlink");

    std::os::unix::fs::symlink(&path, &link).expect("symlink");
    assert!(matches!(
        read_user_file(&link, true).await,
        Err(AdapterError::ResourceIo {
            reason: "symlink",
            ..
        })
    ));
    let real = dir.join("ocpasswd-real");
    std::fs::rename(&path, &real).expect("mv");
    std::os::unix::fs::symlink(&real, &path).expect("symlink target");
    assert!(adapter.user_list().await.is_err());
    std::fs::remove_file(&path).expect("rm symlink");
    std::fs::rename(&real, &path).expect("restore");

    std::fs::set_permissions(dir.path(), std::fs::Permissions::from_mode(0o770)).expect("0770");
    assert!(matches!(
        adapter.user_list().await,
        Err(AdapterError::ResourceMetadata { .. })
    ));
    std::fs::set_permissions(dir.path(), std::fs::Permissions::from_mode(0o700)).expect("0700");
}

#[tokio::test]
async fn group_and_user_capacity_preconditions_and_atomicity() {
    let dir = TempDir::new("group-atomic");
    let users = dir.join("ocpasswd");
    write_user_fixture(&users, b"alice:staff:$6$alice-hash\nbob:*:!$6$bob-hash\n");
    let adapter = dir.user_adapter(
        PathBuf::from("/bin/false"),
        PathBuf::from("/bin/false"),
        Limits::default(),
    );
    adapter
        .group_apply(
            "staff",
            &["alice".to_owned(), "bob".to_owned()],
            1,
            test_effect(),
        )
        .await
        .expect("group apply");
    assert_eq!(
        adapter.group_list().await.expect("groups").groups[0].members,
        ["alice", "bob"]
    );
    assert_eq!(
        adapter
            .desired_effect_observe("group_apply", "staff", 1, test_effect())
            .await
            .expect("observe")
            .state,
        DesiredEffectState::AppliedExact as i32
    );
    assert_eq!(
        adapter
            .desired_effect_observe("group_apply", "staff", 2, test_effect())
            .await
            .expect("observe stale")
            .state,
        DesiredEffectState::Unknown as i32
    );
    let updated = std::fs::read(&users).expect("updated");
    assert!(updated.starts_with(b"alice:staff:$6$alice-hash\nbob:staff:!$6$bob-hash\n"));
    assert!(
        adapter
            .group_apply("../staff", &[], 2, test_effect())
            .await
            .is_err()
    );
    assert!(matches!(
        adapter
            .user_create("alice", "test-key", &[7_u8; 64], 1, test_effect())
            .await,
        Err(AdapterError::InvalidRequest)
    ));
    assert!(matches!(
        adapter
            .user_password_rotate("carol", "test-key", &[7_u8; 64], 2, test_effect())
            .await,
        Err(AdapterError::InvalidRequest)
    ));
    assert_eq!(std::fs::read(&users).expect("unchanged"), updated);

    // Capacity boundaries: 384 users/groups succeed; overflow fails pre-effect.
    let cap_dir = TempDir::new("capacity");
    let cap_users = cap_dir.join("ocpasswd");
    let members: Vec<String> = (0..MAX_MANAGED_RESOURCES)
        .map(|i| format!("user{i:03}"))
        .collect();
    let max_body: String = members
        .iter()
        .map(|m| format!("{m}:legacy:$6$test-hash\n"))
        .collect();
    write_user_fixture(&cap_users, max_body.as_bytes());
    assert_eq!(
        parse_user_file(max_body.as_bytes())
            .expect("max users")
            .users
            .len(),
        384
    );
    assert!(matches!(
        parse_user_file(format!("{max_body}overflow:overflow:$6$test-hash\n").as_bytes()),
        Err(AdapterError::OutputLimit)
    ));

    let openssl_marker = cap_dir.join("openssl-called");
    let ocpasswd_marker = cap_dir.join("ocpasswd-called");
    let openssl = cap_dir.exec(
        "openssl",
        &format!("touch '{}'; printf pw", openssl_marker.display()),
    );
    let ocpasswd = cap_dir.exec(
        "ocpasswd-bin",
        &format!("touch '{}'; exit 1", ocpasswd_marker.display()),
    );
    let cap_adapter = cap_dir.user_adapter(ocpasswd, openssl, Limits::default());
    assert!(matches!(
        cap_adapter
            .user_create("overflow", "test-key", &[7_u8; 64], 1, test_effect())
            .await,
        Err(AdapterError::CapacityExceeded)
    ));
    assert!(matches!(
        cap_adapter
            .group_apply("new-group", &["user000".to_owned()], 1, test_effect())
            .await,
        Err(AdapterError::CapacityExceeded)
    ));
    assert!(!openssl_marker.exists() && !ocpasswd_marker.exists());
    assert!(!cap_dir.join("effects.sqlite3").exists() && !cap_dir.join("effects.key").exists());
    assert_eq!(
        PrivdError::from(AdapterError::CapacityExceeded).kind,
        i32::from(ErrorKind::CapacityExceeded)
    );

    // Maximum single group apply succeeds; 385th member fails without mutation.
    let grp_dir = TempDir::new("group-max");
    let grp_users = grp_dir.join("ocpasswd");
    let grp_body: String = members
        .iter()
        .map(|m| format!("{m}:*:$6$test-hash\n"))
        .collect();
    write_user_fixture(&grp_users, grp_body.as_bytes());
    let grp_adapter = grp_dir.user_adapter(
        PathBuf::from("/bin/false"),
        PathBuf::from("/bin/false"),
        Limits::default(),
    );
    grp_adapter
        .group_apply("staff", &members, 1, test_effect())
        .await
        .expect("max group");
    assert_eq!(
        grp_adapter.group_list().await.expect("groups").groups[0]
            .members
            .len(),
        384
    );
    let applied = std::fs::read(&grp_users).expect("applied");
    let mut overflow = members;
    overflow.push("overflow".to_owned());
    assert!(matches!(
        grp_adapter
            .group_apply("staff", &overflow, 2, test_effect())
            .await,
        Err(AdapterError::InvalidRequest)
    ));
    assert_eq!(std::fs::read(&grp_users).expect("unchanged"), applied);

    // Oversized staging is rejected before preparing an effect.
    let stg_dir = TempDir::new("staging-cap");
    let stg_users = stg_dir.join("ocpasswd");
    let stg_orig: String = (0..MAX_MANAGED_RESOURCES - 1)
        .map(|i| format!("user{i:03}:*:$6$test-hash\n"))
        .collect();
    write_user_fixture(&stg_users, stg_orig.as_bytes());
    let stg_adapter = stg_dir.user_adapter(
        PathBuf::from("/bin/false"),
        PathBuf::from("/bin/false"),
        Limits::default(),
    );
    let mut staging = stg_adapter
        .stage_user_file(stg_orig.as_bytes())
        .await
        .expect("stage");
    let oversized: String = (0..=MAX_MANAGED_RESOURCES)
        .map(|i| format!("staged{i:03}:*:$6$test-hash\n"))
        .collect();
    tokio::fs::write(staging.path(), oversized)
        .await
        .expect("write oversized");
    assert!(matches!(
        stg_adapter
            .commit_desired_staging(
                &mut staging,
                stg_orig.as_bytes(),
                "user_create",
                "overflow",
                1,
                test_effect()
            )
            .await,
        Err(AdapterError::OutputLimit)
    ));
    assert!(!stg_dir.join("effects.sqlite3").exists());
}

#[tokio::test]
async fn failed_password_rotation_and_staging_cleanup_preserve_original() {
    let dir = TempDir::new("staging");
    let users = dir.join("ocpasswd");
    let original = b"alice:staff:!$6$original-hash\n";
    write_user_fixture(&users, original);

    let fast_openssl = dir.exec("openssl-fast", "cat >/dev/null; printf rotated-password");
    let fail_ocpasswd = dir.exec("ocpasswd-fail", "printf corrupted > \"$2\"; exit 1");
    let fail_adapter = dir.user_adapter(fail_ocpasswd, fast_openssl.clone(), Limits::default());
    assert!(
        fail_adapter
            .user_password_rotate("alice", "test-key", &[7_u8; 64], 2, test_effect())
            .await
            .is_err()
    );
    assert_eq!(std::fs::read(&users).expect("preserved"), original);

    // Timeout during ocpasswd staging, openssl decrypt, and ocpasswd -l locking.
    let short_limits = Limits {
        timeout: Duration::from_millis(400),
        output_bytes: DEFAULT_OUTPUT_BYTES,
    };
    let slow_ocpasswd = dir.exec(
        "ocpasswd-slow",
        "cat >/dev/null; cp \"$2\" \"$2.tmp\"; sleep 5",
    );
    let slow_openssl = dir.exec("openssl-slow", "sleep 5");
    let lock_ocpasswd = dir.exec(
        "ocpasswd-lock-slow",
        "if [ \"$3\" = \"-l\" ]; then sleep 5; exit 0; fi\ncat >/dev/null\nprintf '%s\\n' 'alice:staff:$6$new-hash' > \"$2\"",
    );

    let queued_adapter =
        dir.user_adapter(slow_ocpasswd.clone(), fast_openssl.clone(), short_limits);
    let guard = queued_adapter.user_file_lock.lock().await;
    assert!(
        tokio::time::timeout(
            Duration::from_millis(20),
            queued_adapter.user_password_rotate("alice", "test-key", &[7_u8; 64], 2, test_effect()),
        )
        .await
        .is_err()
    );
    drop(guard);
    assert_eq!(staging_files(dir.path()).len(), 0);

    for (ocp, ssl) in [
        (slow_ocpasswd, fast_openssl.clone()),
        (PathBuf::from("/bin/false"), slow_openssl),
        (lock_ocpasswd, fast_openssl.clone()),
    ] {
        let adapter = dir.user_adapter(ocp, ssl, short_limits);
        assert!(matches!(
            adapter
                .user_password_rotate("alice", "test-key", &[7_u8; 64], 2, test_effect())
                .await,
            Err(AdapterError::DeadlineExceeded)
        ));
        assert_eq!(staging_files(dir.path()).len(), 0);
        assert_eq!(std::fs::read(&users).expect("preserved"), original);
    }

    // Abort mid-execution removes staging files.
    let marker = dir.join("cancel-started");
    let cancel_ocpasswd = dir.exec(
        "ocpasswd-cancel",
        &format!(
            "cp \"$2\" \"$2.tmp\"\ntouch '{}'\nsleep 10",
            marker.display()
        ),
    );
    let cancel_adapter = dir.user_adapter(cancel_ocpasswd, fast_openssl, Limits::default());
    let task = tokio::spawn(async move {
        cancel_adapter
            .user_password_rotate("alice", "test-key", &[7_u8; 64], 2, test_effect())
            .await
    });
    for _ in 0..500 {
        if marker.exists() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    assert!(marker.exists() && !staging_files(dir.path()).is_empty());
    task.abort();
    let _ = task.await;
    assert_eq!(staging_files(dir.path()).len(), 0);

    // Startup cleanup removes UUID staging + scratch (including private child group), preserves lookalike.
    let stale = dir.join(format!(".ocpasswd.ocservia-{}", Uuid::now_v7()));
    let scratch = dir.join(format!(".ocpasswd.ocservia-{}.tmp", Uuid::now_v7()));
    let lookalike = dir.join(".ocpasswd.ocservia-not-a-uuid");
    write_user_fixture(&stale, b"secret hash staging");
    write_user_fixture(&scratch, b"incomplete child scratch");
    assert_eq!(
        std::fs::metadata(&stale).expect("meta").gid(),
        rustix::process::getegid().as_raw()
    );
    std::fs::write(&lookalike, b"preserve").expect("lookalike");
    fail_adapter
        .cleanup_stale_user_staging()
        .await
        .expect("cleanup");
    assert!(!stale.exists() && !scratch.exists() && lookalike.exists());
}

#[test]
fn effect_store_scale_rollback_crash_tamper_and_missing_evidence() {
    // 10,000 historical rows.
    let scale_dir = TempDir::new("effects-scale");
    let mut store = EffectStore::open_for_mutation(&scale_dir.effect_resources()).expect("open");
    let now = effect_now().expect("clock");
    let tx = store
        .connection
        .transaction_with_behavior(TransactionBehavior::Immediate)
        .expect("tx");
    let before = authenticate_file(&store.hmac_key, b"before");
    let after = authenticate_file(&store.hmac_key, b"after");
    for i in 0..10_000_u64 {
        let key = format!("user{i:05}");
        let id = validate_effect_context("user_create", &key, 1, test_effect(), now).expect("id");
        let mac = authenticate_record(
            &store.hmac_key,
            "user_create",
            &key,
            1,
            id,
            "applied",
            &before,
            &after,
        );
        tx.execute(
            "INSERT INTO desired_effects(mutation_kind,resource_key,revision,command_id,idempotency_key,payload_sha256,expires_at,state,before_hmac,after_hmac,record_hmac,updated_at) VALUES('user_create',?1,1,?2,?3,?4,?5,'applied',?6,?7,?8,?9)",
            params![key, id.command_id.as_slice(), id.idempotency_key.as_slice(), id.payload_sha256.as_slice(), id.expires_at, before.as_slice(), after.as_slice(), mac.as_slice(), now],
        )
        .expect("insert");
    }
    tx.commit().expect("commit");
    assert_eq!(
        store
            .connection
            .query_row("SELECT count(*) FROM desired_effects", [], |r| r
                .get::<_, i64>(0))
            .expect("count"),
        10_000
    );

    // Revision rollback rejection, post-commit crash recovery, tamper rejection.
    let dir = TempDir::new("effects-core");
    let res = dir.effect_resources();
    let mut store = EffectStore::open_for_mutation(&res).expect("open");
    store
        .prepare(
            "user_password_rotate",
            "alice",
            2,
            test_effect(),
            b"p1",
            b"p2",
        )
        .expect("p2");
    let recovered = store
        .observe(
            "user_password_rotate",
            "alice",
            2,
            EffectIdentity {
                expires_at_unix_seconds: 1,
                ..test_effect()
            },
            b"p2",
        )
        .expect("recover crash");
    assert_eq!(recovered.state, DesiredEffectState::AppliedExact as i32);
    store
        .prepare(
            "user_password_rotate",
            "alice",
            3,
            test_effect(),
            b"p2",
            b"p3",
        )
        .expect("p3");
    store
        .mark_applied("user_password_rotate", "alice", 3, test_effect())
        .expect("apply 3");
    let stale = store
        .observe("user_password_rotate", "alice", 2, test_effect(), b"p3")
        .expect("stale");
    assert_eq!(
        stale.state,
        DesiredEffectState::SupersededByNewerRevision as i32
    );
    assert_eq!(stale.observed_revision, 3);
    assert!(matches!(
        store.prepare(
            "user_password_rotate",
            "alice",
            2,
            test_effect(),
            b"p3",
            b"p2"
        ),
        Err(AdapterError::InvalidRequest)
    ));
    store
        .connection
        .execute(
            "UPDATE desired_effects SET record_hmac=zeroblob(32) WHERE resource_key='alice'",
            [],
        )
        .expect("tamper");
    assert!(matches!(
        store.observe("user_password_rotate", "alice", 3, test_effect(), b"p3"),
        Err(AdapterError::InvalidResource)
    ));

    // Missing evidence / deleted row / stale snapshot / deleted DB all fail closed.
    let loss_dir = TempDir::new("effects-loss");
    let loss_res = loss_dir.effect_resources();
    let db_path = loss_dir.join("effects.sqlite3");
    let snap_path = loss_dir.join("effects.snapshot");
    let mut store = EffectStore::open_for_mutation(&loss_res).expect("init");
    assert_eq!(
        store
            .observe("user_password_rotate", "alice", 2, test_effect(), b"x")
            .expect("miss")
            .state,
        DesiredEffectState::Unknown as i32
    );
    store
        .prepare(
            "user_password_rotate",
            "alice",
            1,
            test_effect(),
            b"one",
            b"two",
        )
        .expect("p1");
    assert_eq!(
        store
            .observe("user_password_rotate", "alice", 1, test_effect(), b"one")
            .expect("before")
            .state,
        DesiredEffectState::Absent as i32
    );
    assert_eq!(
        store
            .observe("user_password_rotate", "alice", 1, test_effect(), b"other")
            .expect("other")
            .state,
        DesiredEffectState::Unknown as i32
    );
    store
        .mark_applied("user_password_rotate", "alice", 1, test_effect())
        .expect("a1");
    drop(store);
    std::fs::copy(&db_path, &snap_path).expect("snapshot");

    let mut store = EffectStore::open_existing(&loss_res).expect("reopen");
    store
        .prepare(
            "user_password_rotate",
            "alice",
            2,
            test_effect(),
            b"two",
            b"three",
        )
        .expect("p2");
    store
        .mark_applied("user_password_rotate", "alice", 2, test_effect())
        .expect("a2");
    store
        .connection
        .execute("DELETE FROM desired_effects WHERE resource_key='alice'", [])
        .expect("delete");
    assert_eq!(
        store
            .observe("user_password_rotate", "alice", 2, test_effect(), b"three")
            .expect("del")
            .state,
        DesiredEffectState::Unknown as i32
    );
    drop(store);

    std::fs::copy(&snap_path, &db_path).expect("restore snap");
    let mut old = EffectStore::open_existing(&loss_res).expect("old snap");
    assert_eq!(
        old.observe("user_password_rotate", "alice", 2, test_effect(), b"three")
            .expect("old")
            .state,
        DesiredEffectState::Unknown as i32
    );
    drop(old);
    std::fs::remove_file(&db_path).expect("rm db");
    assert!(matches!(
        EffectStore::open_existing(&loss_res),
        Err(AdapterError::Unavailable)
    ));
    assert!(!db_path.exists() && loss_dir.join("effects.key").exists());
}

#[test]
fn authorized_effect_store_prunes_only_authenticated_expired_successes() {
    let dir = TempDir::new("authorized");
    let mut store = EffectStore::open_for_mutation(&dir.effect_resources()).expect("open");
    let now = effect_now().expect("clock");
    let node_id = *Uuid::now_v7().as_bytes();
    let op_id = *Uuid::now_v7().as_bytes();
    let cmds = [*Uuid::now_v7().as_bytes(), *Uuid::now_v7().as_bytes()];
    let idems = [*Uuid::now_v7().as_bytes(), *Uuid::now_v7().as_bytes()];
    let hashes = [Sha256::digest(b"first"), Sha256::digest(b"second")];
    let auth = |i: usize| AuthorizedEffectIdentity {
        node_id: &node_id,
        command_id: &cmds[i],
        operation_id: &op_id,
        idempotency_key: &idems[i],
        semantic_payload_sha256: hashes[i].as_slice(),
        action: "service.reload",
        authorization_revision: 7,
        effect_kind: "service_reload",
        resource_key: "ocserv.service",
        effect_revision: 7,
        expires_at_unix_seconds: now + 60,
        retry_if_absent: false,
    };
    assert_eq!(
        store.prepare_authorized(auth(0)).expect("p0"),
        AuthorizedEffectDecision::Execute
    );
    store.complete_authorized(auth(0), b"result").expect("c0");

    let mut expired = query_authorized_effect(&store.connection, &cmds[0])
        .expect("q0")
        .expect("r0");
    expired.expires_at = now - 1;
    expired.record_hmac = authenticate_authorized_record(&store.hmac_key, &expired);
    store
        .connection
        .execute(
            "UPDATE authorized_effects SET expires_at=?2,record_hmac=?3 WHERE command_id=?1",
            params![
                cmds[0].as_slice(),
                expired.expires_at,
                expired.record_hmac.as_slice()
            ],
        )
        .expect("expire");

    let expired_id = AuthorizedEffectIdentity {
        expires_at_unix_seconds: now - 1,
        ..auth(0)
    };
    assert!(matches!(
        store.prepare_authorized(expired_id),
        Err(AdapterError::InvalidRequest)
    ));
    assert_eq!(
        store.replay_authorized(expired_id).expect("replay"),
        Some(b"result".to_vec())
    );
    assert_eq!(
        store.prepare_authorized(auth(1)).expect("p1"),
        AuthorizedEffectDecision::Execute
    );
    assert!(
        query_authorized_effect(&store.connection, &cmds[0])
            .expect("pruned")
            .is_none()
    );
    assert!(
        query_authorized_effect(&store.connection, &cmds[1])
            .expect("kept")
            .is_some()
    );
}

#[test]
fn effect_store_transitions_config_aba_and_schema_migration() {
    // Prepared transition promotion across users and groups.
    let dir = TempDir::new("effect-chain");
    let res = dir.effect_resources();
    let mut store = EffectStore::open_for_mutation(&res).expect("init");
    store
        .prepare(
            "user_password_rotate",
            "alice",
            2,
            test_effect(),
            b"f1",
            b"f2",
        )
        .expect("alice");
    assert!(matches!(
        store.prepare("user_create", "bob", 1, test_effect(), b"f1", b"fbob"),
        Err(AdapterError::Unavailable)
    ));
    drop(store);
    let mut store = EffectStore::open_existing(&res).expect("restart");
    store
        .prepare("user_create", "bob", 1, test_effect(), b"f2", b"f3")
        .expect("promotes alice");
    store
        .mark_applied("user_create", "bob", 1, test_effect())
        .expect("apply bob");
    assert_eq!(
        store
            .observe("user_password_rotate", "alice", 2, test_effect(), b"f3")
            .expect("alice")
            .state,
        DesiredEffectState::AppliedExact as i32
    );
    store
        .prepare("group_apply", "staff", 3, test_effect(), b"f3", b"f4")
        .expect("group");
    store
        .prepare("user_create", "carol", 1, test_effect(), b"f4", b"f5")
        .expect("promotes group");
    assert_eq!(
        store
            .observe("group_apply", "staff", 3, test_effect(), b"f5")
            .expect("group")
            .state,
        DesiredEffectState::AppliedExact as i32
    );

    // Config absent reconciliation + hash ABA fence + schema migration.
    let cfg_dir = TempDir::new("config-effects");
    let cfg_res = cfg_dir.effect_resources();
    let cmds: [[u8; 16]; 4] = std::array::from_fn(|_| *Uuid::now_v7().as_bytes());
    let idems: [[u8; 16]; 4] = std::array::from_fn(|_| *Uuid::now_v7().as_bytes());
    let hashes = [
        Sha256::digest(b"A1"),
        Sha256::digest(b"B2"),
        Sha256::digest(b"A3"),
        Sha256::digest(b"stale"),
    ];
    let eff = |i: usize| EffectIdentity {
        command_id: &cmds[i],
        idempotency_key: &idems[i],
        semantic_payload_sha256: hashes[i].as_slice(),
        expires_at_unix_seconds: i64::MAX,
    };
    let mut store = EffectStore::open_for_mutation(&cfg_res).expect("init");
    for (rev, idx, before, after) in [
        (1, 0, b"X".as_slice(), b"A".as_slice()),
        (2, 1, b"A", b"B"),
        (3, 2, b"B", b"A"),
    ] {
        store
            .prepare("config_apply", "ocserv.conf", rev, eff(idx), before, after)
            .expect("prep");
        store
            .mark_applied("config_apply", "ocserv.conf", rev, eff(idx))
            .expect("apply");
    }
    drop(store);
    let mut restarted = EffectStore::open_existing(&cfg_res).expect("restart");
    let refreshed = EffectIdentity {
        expires_at_unix_seconds: i64::MAX - 1,
        ..eff(2)
    };
    assert_eq!(
        restarted
            .observe("config_apply", "ocserv.conf", 3, refreshed, b"A")
            .expect("exact")
            .state,
        DesiredEffectState::AppliedExact as i32
    );
    assert!(matches!(
        restarted.prepare("config_apply", "ocserv.conf", 1, eff(0), b"A", b"A"),
        Err(AdapterError::InvalidRequest)
    ));
    let old = restarted
        .observe("config_apply", "ocserv.conf", 1, eff(0), b"A")
        .expect("old");
    assert_eq!(
        (old.state, old.observed_revision),
        (DesiredEffectState::SupersededByNewerRevision as i32, 3)
    );
    restarted
        .prepare(
            "config_apply",
            "ocserv.conf",
            4,
            eff(3),
            b"A",
            b"stale-cand",
        )
        .expect("p4");
    drop(restarted);
    let mut restarted = EffectStore::open_existing(&cfg_res).expect("restart2");
    assert_eq!(
        restarted
            .observe("config_apply", "ocserv.conf", 4, eff(3), b"A")
            .expect("absent")
            .state,
        DesiredEffectState::Absent as i32
    );
    assert_eq!(
        restarted
            .prepare("config_apply", "ocserv.conf", 5, eff(1), b"A", b"next")
            .expect("p5"),
        PrepareEffect::Proceed
    );

    // Legacy schema downgrade -> open_existing upgrades and preserves config revision fence.
    let mig_dir = TempDir::new("effect-schema");
    let mig_res = mig_dir.effect_resources();
    let mut store = EffectStore::open_for_mutation(&mig_res).expect("init");
    store
        .prepare("config_apply", "ocserv.conf", 1, test_effect(), b"X", b"A")
        .expect("p1");
    store
        .mark_applied("config_apply", "ocserv.conf", 1, test_effect())
        .expect("a1");
    drop(store);
    let legacy = Connection::open(&mig_res.effect_store).expect("legacy");
    legacy
        .execute_batch(
            "BEGIN IMMEDIATE;
             ALTER TABLE desired_effects RENAME TO desired_effects_current;
             CREATE TABLE desired_effects (
               mutation_kind TEXT NOT NULL, resource_key TEXT NOT NULL,
               revision INTEGER NOT NULL CHECK(revision > 0),
               command_id BLOB NOT NULL CHECK(length(command_id)=16),
               idempotency_key BLOB NOT NULL CHECK(length(idempotency_key)=16),
               payload_sha256 BLOB NOT NULL CHECK(length(payload_sha256)=32),
               expires_at INTEGER NOT NULL,
               state TEXT NOT NULL CHECK(state IN ('prepared','applied')),
               before_hmac BLOB NOT NULL CHECK(length(before_hmac)=32),
               after_hmac BLOB NOT NULL CHECK(length(after_hmac)=32),
               record_hmac BLOB NOT NULL CHECK(length(record_hmac)=32),
               updated_at INTEGER NOT NULL, PRIMARY KEY(mutation_kind,resource_key)
             ) STRICT;
             INSERT INTO desired_effects SELECT * FROM desired_effects_current;
             DROP TABLE desired_effects_current; COMMIT;",
        )
        .expect("downgrade");
    drop(legacy);
    let mut upgraded = EffectStore::open_existing(&mig_res).expect("migrate");
    assert_eq!(
        upgraded
            .observe("config_apply", "ocserv.conf", 1, test_effect(), b"A")
            .expect("keep")
            .state,
        DesiredEffectState::AppliedExact as i32
    );
    upgraded
        .prepare("config_apply", "ocserv.conf", 2, test_effect(), b"A", b"B")
        .expect("p2");
    upgraded
        .mark_absent("config_apply", "ocserv.conf", 2, test_effect())
        .expect("absent");
    assert_eq!(
        upgraded
            .observe("config_apply", "ocserv.conf", 2, test_effect(), b"A")
            .expect("obs2")
            .state,
        DesiredEffectState::Absent as i32
    );
}

#[cfg(target_os = "linux")]
async fn assert_effect_state(
    adapter: &Adapter,
    mutation_kind: &str,
    resource_key: &str,
    revision: u64,
    expected: bool,
) {
    let observed = adapter
        .desired_effect_observe(mutation_kind, resource_key, revision, test_effect())
        .await
        .expect("observe native effect");
    assert_eq!(
        observed.state == DesiredEffectState::AppliedExact as i32,
        expected,
        "unexpected state for {mutation_kind}:{resource_key}@{revision}: {observed:?}"
    );
}

#[cfg(target_os = "linux")]
#[tokio::test]
#[ignore = "requires an explicitly prepared native Ocserv test container"]
#[allow(clippy::too_many_lines)]
async fn native_ocserv_operations() {
    let root = PathBuf::from(std::env::var("OCSERVIA_NATIVE_OCSERV_ROOT").expect("native root"));
    let scenario = std::env::var("OCSERVIA_NATIVE_OCSERV_SCENARIO").expect("native scenario");
    let users = root.join("ocpasswd");
    let resources = FixedResources::default()
        .with_user_resources(
            PathBuf::from("/usr/bin/ocpasswd"),
            users.clone(),
            PathBuf::from("/usr/bin/openssl"),
            root.join("private.pem"),
            String::from("native-test-user-password"),
        )
        .expect("fixed native resources")
        .with_effect_store(
            root.join("privd-state/effects.sqlite3"),
            root.join("privd-state/effects.key"),
        )
        .expect("native effect resources");
    let adapter = Adapter::new(resources, Limits::default());
    let record = || {
        let records = parse_secret_user_records(&std::fs::read(&users).expect("native users"))
            .expect("parse native users");
        assert_eq!(records.len(), 1);
        assert_eq!(records[0].username, "alice");
        (records[0].groups.clone(), records[0].hash.to_string())
    };
    match scenario.as_str() {
        "config-rollback" => {
            let systemctl = root.join("systemctl-fixture");
            write_executable(
                &systemctl,
                format!(
                    "#!/bin/sh\nset -e\npid=$(cat '{}/launcher.pid')\nkill -0 \"$pid\"\ncase \"$1\" in\nreload) kill -HUP \"$pid\";;\nshow) printf 'LoadState=loaded\\nActiveState=active\\nSubState=running\\n';;\n*) exit 1;;\nesac\n",
                    root.display()
                ),
            );
            let occtl = root.join("occtl-fixture");
            write_executable(
                &occtl,
                format!(
                    "#!/bin/sh\nexec /usr/bin/occtl -s '{}/occtl.sock' \"$@\"\n",
                    root.display()
                ),
            );
            let config = root.join("ocserv.conf");
            let cfg_adapter = Adapter::new(
                FixedResources::new(
                    systemctl,
                    PathBuf::from("/usr/sbin/ocserv"),
                    occtl,
                    config.clone(),
                    PathBuf::from("/proc/sys/kernel/random/boot_id"),
                )
                .expect("native config resources")
                .with_effect_store(
                    root.join("config-state/effects.sqlite3"),
                    root.join("config-state/effects.key"),
                )
                .expect("config effect store"),
                Limits::default(),
            );
            let original = std::fs::read(&config).expect("original config");
            let mut invalid = original.clone();
            invalid.extend_from_slice(b"\nauth = \"invalid-auth-backend\"\n");
            let result = cfg_adapter
                .apply_validated_config(
                    &invalid,
                    &Sha256::digest(&invalid),
                    &Sha256::digest(&original),
                    1,
                    test_effect(),
                    false,
                )
                .await
                .expect("native rollback");
            assert!(result.healthy && result.rolled_back && !result.failed_critical);
            assert_eq!(std::fs::read(&config).expect("restored"), original);
            assert_eq!(
                cfg_adapter
                    .desired_effect_observe("config_apply", "ocserv.conf", 1, test_effect())
                    .await
                    .expect("observe")
                    .state,
                DesiredEffectState::Absent as i32
            );
            assert_eq!(staging_files(&root).len(), 0);
        }
        "occtl" => {
            adapter.ocserv_version().await.expect("native version");
            let sessions =
                parse_sessions(&std::fs::read(root.join("sessions.json")).expect("users"))
                    .expect("parse users");
            assert_eq!(
                (
                    sessions.sessions.len(),
                    sessions.sessions[0].username.as_str(),
                    sessions.sessions[0].remote_ip.as_str()
                ),
                (1, "alice", "127.0.0.1")
            );
            parse_ip_bans(&std::fs::read(root.join("bans.json")).expect("bans")).expect("bans");
            let socket = root.join("occtl.sock");
            let sock = socket.to_str().expect("socket");
            adapter
                .execute(
                    &adapter.resources.occtl,
                    &["-s", sock, "terminate", "id", &sessions.sessions[0].id],
                )
                .await
                .expect("terminate");
            let mut empty = false;
            for _ in 0..100 {
                let out = adapter
                    .execute(
                        &adapter.resources.occtl,
                        &["-s", sock, "--json", "show", "users"],
                    )
                    .await
                    .expect("users");
                empty = parse_sessions(&out.stdout)
                    .expect("empty")
                    .sessions
                    .is_empty();
                if empty {
                    break;
                }
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
            assert!(empty, "terminated session must disappear");
        }
        "concurrency" => {
            let sealed = std::fs::read(root.join("sealed-disabled-password.bin")).expect("sealed");
            let (b, c) = tokio::join!(
                adapter.user_create(
                    "bob",
                    "native-test-user-password",
                    &sealed,
                    1,
                    test_effect()
                ),
                adapter.user_create(
                    "carol",
                    "native-test-user-password",
                    &sealed,
                    1,
                    test_effect()
                ),
            );
            b.expect("bob");
            c.expect("carol");
            let (d1, d2) = tokio::join!(
                adapter.user_create(
                    "dave",
                    "native-test-user-password",
                    &sealed,
                    1,
                    test_effect()
                ),
                adapter.user_create(
                    "dave",
                    "native-test-user-password",
                    &sealed,
                    1,
                    test_effect()
                ),
            );
            assert_eq!(usize::from(d1.is_ok()) + usize::from(d2.is_ok()), 1);
            let (l, r) = tokio::join!(
                adapter.user_disable("alice", 6, test_effect()),
                adapter.user_password_rotate(
                    "alice",
                    "native-test-user-password",
                    &sealed,
                    7,
                    test_effect()
                ),
            );
            l.expect("disable");
            r.expect("rotate");
            let members = vec!["alice".to_owned(), "bob".to_owned(), "carol".to_owned()];
            let (g, r2) = tokio::join!(
                adapter.group_apply("staff", &members, 2, test_effect()),
                adapter.user_password_rotate(
                    "alice",
                    "native-test-user-password",
                    &sealed,
                    8,
                    test_effect()
                ),
            );
            g.expect("group");
            r2.expect("rotate2");
            let before = std::fs::read(&users).expect("before");
            adapter
                .user_password_rotate(
                    "alice",
                    "native-test-user-password",
                    &sealed,
                    8,
                    test_effect(),
                )
                .await
                .expect("replay");
            assert_eq!(before, std::fs::read(&users).expect("after"));
            let records = parse_secret_user_records(&before).expect("records");
            assert_eq!(records.len(), 4);
            for rec in &records {
                assert!(
                    !rec.hash.is_empty() && rec.hash.starts_with('!') == (rec.username == "alice")
                );
                assert_eq!(
                    rec.groups,
                    if rec.username == "dave" { "*" } else { "staff" }
                );
            }
            let meta = std::fs::metadata(&users).expect("meta");
            assert_eq!(
                (meta.uid(), meta.gid(), meta.mode() & 0o777, meta.nlink()),
                (0, 0, 0o600, 1)
            );
            assert_eq!(staging_files(&root).len(), 0);
        }
        "create-user" => {
            let sealed = std::fs::read(root.join("sealed-initial-password.bin")).expect("sealed");
            adapter
                .user_create(
                    "alice",
                    "native-test-user-password",
                    &sealed,
                    1,
                    test_effect(),
                )
                .await
                .expect("create");
            assert_effect_state(&adapter, "user_create", "alice", 1, true).await;
            let after = std::fs::read(&users).expect("created");
            assert!(
                adapter
                    .user_password_rotate(
                        "bob",
                        "native-test-user-password",
                        &sealed,
                        2,
                        test_effect()
                    )
                    .await
                    .is_err()
            );
            assert_eq!(std::fs::read(&users).expect("unchanged"), after);
            let listed = adapter.user_list().await.expect("list");
            assert_eq!(listed.users.len(), 1);
            assert!(listed.users[0].username == "alice" && listed.users[0].enabled);
            let (groups, hash) = record();
            assert!(groups == "*" && !hash.starts_with('!'));
        }
        "rotate-password" => {
            let sealed = std::fs::read(root.join("sealed-rotated-password.bin")).expect("sealed");
            let (bg, bh) = record();
            adapter
                .user_password_rotate(
                    "alice",
                    "native-test-user-password",
                    &sealed,
                    2,
                    test_effect(),
                )
                .await
                .expect("rotate");
            let (ag, ah) = record();
            assert!(ag == bg && ah != bh && !ah.starts_with('!'));
            assert_effect_state(&adapter, "user_password_rotate", "alice", 2, true).await;
        }
        "disable" => {
            let before = std::fs::read(&users).expect("before");
            let (bg, bh) = record();
            adapter
                .user_disable("alice", 3, test_effect())
                .await
                .expect("disable");
            let (ag, ah) = record();
            assert_eq!(
                (ag.as_str(), ah.strip_prefix('!')),
                (bg.as_str(), Some(bh.as_str()))
            );
            assert!(!adapter.user_list().await.expect("list").users[0].enabled);
            let sealed = std::fs::read(root.join("sealed-rotated-password.bin")).expect("sealed");
            assert!(
                adapter
                    .user_create(
                        "alice",
                        "native-test-user-password",
                        &sealed,
                        3,
                        test_effect()
                    )
                    .await
                    .is_err()
            );
            assert_ne!(std::fs::read(&users).expect("locked"), before);
            assert_eq!(record(), (ag, ah));
            assert_effect_state(&adapter, "user_disable", "alice", 3, true).await;
        }
        "rotate-disabled-password" => {
            let sealed = std::fs::read(root.join("sealed-disabled-password.bin")).expect("sealed");
            let (bg, bh) = record();
            assert!(bh.starts_with('!'));
            adapter
                .user_password_rotate(
                    "alice",
                    "native-test-user-password",
                    &sealed,
                    4,
                    test_effect(),
                )
                .await
                .expect("rotate");
            let (ag, ah) = record();
            assert!(ag == bg && ah.starts_with('!') && ah != bh);
            assert!(!adapter.user_list().await.expect("list").users[0].enabled);
            assert_effect_state(&adapter, "user_password_rotate", "alice", 4, true).await;
        }
        "enable" => {
            let (bg, bh) = record();
            adapter
                .user_enable("alice", 5, test_effect())
                .await
                .expect("enable");
            let (ag, ah) = record();
            assert_eq!(
                (ag.as_str(), bh.strip_prefix('!')),
                (bg.as_str(), Some(ah.as_str()))
            );
            assert!(adapter.user_list().await.expect("list").users[0].enabled);
            assert_effect_state(&adapter, "user_enable", "alice", 5, true).await;
        }
        "group-apply" => {
            let (_, bh) = record();
            adapter
                .group_apply("staff", &["alice".to_owned()], 1, test_effect())
                .await
                .expect("group");
            let (ag, ah) = record();
            assert_eq!((ag.as_str(), ah.as_str()), ("staff", bh.as_str()));
            assert_effect_state(&adapter, "group_apply", "staff", 1, true).await;
        }
        _ => panic!("unsupported native scenario"),
    }
}

#[tokio::test]
async fn child_timeout_output_limit_and_fixed_mutations() {
    let dir = TempDir::new("child-limits");
    let make_ocserv = |prog: PathBuf, limits: Limits| {
        Adapter::new(
            FixedResources::new(
                PathBuf::from("/bin/false"),
                prog,
                PathBuf::from("/bin/false"),
                PathBuf::from("/etc/hosts"),
                PathBuf::from("/proc/sys/kernel/random/boot_id"),
            )
            .expect("resources"),
            limits,
        )
    };
    let slow = dir.exec("slow-ocserv", "sleep 5");
    assert!(matches!(
        make_ocserv(
            slow,
            Limits {
                timeout: Duration::from_millis(400),
                output_bytes: 1024
            }
        )
        .ocserv_version()
        .await,
        Err(AdapterError::DeadlineExceeded)
    ));
    let noisy = dir.exec(
        "noisy-ocserv",
        "i=0; while [ $i -lt 2048 ]; do printf xx; i=$((i + 1)); done",
    );
    assert!(matches!(
        make_ocserv(
            noisy,
            Limits {
                timeout: Duration::from_secs(30),
                output_bytes: 1024
            }
        )
        .ocserv_version()
        .await,
        Err(AdapterError::OutputLimit)
    ));

    let io_adapter = Adapter::new(
        FixedResources::default(),
        Limits {
            timeout: Duration::from_millis(100),
            output_bytes: 1024,
        },
    );
    for (idx, (body, input)) in [
        ("sleep 5", vec![b'x'; 1024 * 1024]),
        ("sleep 5 & exit 0", Vec::new()),
    ]
    .into_iter()
    .enumerate()
    {
        let prog = dir.exec(&format!("blocked-io-{idx}"), body);
        let res = tokio::time::timeout(
            Duration::from_secs(2),
            io_adapter.execute_with_input(&prog, &[], &input),
        )
        .await
        .expect("obey deadline");
        assert!(matches!(res, Err(AdapterError::DeadlineExceeded)));
    }

    // Fixed programs, fixed arguments, ocserv >= 1.4.2 terminate gate, boot ID check.
    let occtl = dir.exec(
        "occtl",
        "case \"$*\" in 'disconnect id 42'|'terminate id 42'|'unban ip 192.0.2.9') exit 0;; *) exit 9;; esac",
    );
    let systemctl = dir.exec(
        "systemctl",
        "[ \"$*\" = 'reload ocserv.service --no-ask-password' ]",
    );
    let boot = dir.join("boot_id");
    let boot_id = Uuid::now_v7().to_string();
    std::fs::write(&boot, &boot_id).expect("boot");
    let version = dir.join("version");
    std::fs::write(&version, "1.4.2").expect("version");
    let ocserv = dir.exec(
        "ocserv",
        &format!("echo \"ocserv $(cat '{}')\" >&2", version.display()),
    );
    let adapter = Adapter::new(
        FixedResources::new(systemctl, ocserv, occtl, PathBuf::from("/etc/hosts"), boot)
            .expect("res"),
        Limits::default(),
    );
    adapter
        .session_disconnect("42", &boot_id)
        .await
        .expect("disconnect");
    adapter
        .session_terminate("42", &boot_id)
        .await
        .expect("terminate");
    std::fs::write(&version, "1.2.4").expect("old version");
    assert!(matches!(
        adapter.session_terminate("42", &boot_id).await,
        Err(AdapterError::InvalidRequest)
    ));
    adapter.ip_ban_remove("192.0.2.9").await.expect("unban");
    adapter.service_reload().await.expect("reload");
    assert!(matches!(
        adapter.session_disconnect("042", &boot_id).await,
        Err(AdapterError::InvalidRequest)
    ));
    assert!(matches!(
        adapter
            .session_disconnect("42", &Uuid::now_v7().to_string())
            .await,
        Err(AdapterError::StaleBoot)
    ));
}

#[cfg(target_os = "linux")]
#[tokio::test]
#[ignore = "requires an explicitly prepared native Ocserv test service"]
async fn native_controlled_operations() {
    let disconnect_id = std::env::var("OCSERVIA_DISCONNECT_SESSION_ID").expect("disconnect ID");
    let terminate_id = std::env::var("OCSERVIA_TERMINATE_SESSION_ID").expect("terminate ID");
    let banned_ip = std::env::var("OCSERVIA_BANNED_IP").expect("banned IP");
    let boot_id = std::fs::read_to_string("/proc/sys/kernel/random/boot_id").expect("boot ID");
    let adapter = Adapter::new(FixedResources::default(), Limits::default());
    adapter
        .session_disconnect(&disconnect_id, boot_id.trim())
        .await
        .expect("disconnect");
    adapter
        .session_terminate(&terminate_id, boot_id.trim())
        .await
        .expect("terminate");
    adapter.ip_ban_remove(&banned_ip).await.expect("unban");
    adapter.service_reload().await.expect("reload");
}

#[cfg(target_os = "linux")]
#[tokio::test]
#[ignore = "requires an explicitly stopped native Ocserv test service"]
async fn native_reload_failure_is_bounded() {
    let adapter = Adapter::new(FixedResources::default(), Limits::default());
    assert!(matches!(
        adapter.service_reload().await,
        Err(AdapterError::CommandFailed { .. })
    ));
}

#[tokio::test]
async fn certificate_csr_keeps_private_key_local() {
    let dir = TempDir::new("cert");
    std::fs::set_permissions(dir.path(), std::fs::Permissions::from_mode(0o700)).expect("0700");
    let adapter = Adapter::new(
        FixedResources::default()
            .with_certificate_key_dir(dir.path().to_path_buf())
            .expect("cert res"),
        Limits {
            timeout: Duration::from_secs(10),
            output_bytes: DEFAULT_OUTPUT_BYTES,
        },
    );
    let id = Uuid::now_v7();
    let result = adapter
        .certificate_csr(
            id.as_bytes(),
            "vpn.example.test",
            &["alt.example.test".to_owned()],
            2048,
        )
        .await
        .expect("csr");
    assert_eq!(result.certificate_id, id.as_bytes());
    assert!(result.csr_der.len() > 64 && result.public_key_sha256.len() == 32);
    assert!(!String::from_utf8_lossy(&result.csr_der).contains("PRIVATE KEY"));
    let key = dir.join(format!("{id}.key.pem"));
    assert_eq!(
        std::fs::metadata(&key).expect("key").permissions().mode() & 0o777,
        0o600
    );

    for mode in [0o400, 0o600, 0o700] {
        std::fs::set_permissions(&key, std::fs::Permissions::from_mode(mode)).expect("chmod");
        let reused = adapter
            .certificate_csr(id.as_bytes(), "vpn.example.test", &[], 2048)
            .await
            .expect("reuse");
        assert_eq!(reused.public_key_sha256, result.public_key_sha256);
    }
    std::fs::set_permissions(&key, std::fs::Permissions::from_mode(0o640)).expect("0640");
    assert!(matches!(
        adapter
            .certificate_csr(id.as_bytes(), "vpn.example.test", &[], 2048)
            .await,
        Err(AdapterError::InvalidResource)
    ));
}

#[tokio::test]
#[allow(clippy::too_many_lines)]
async fn certificate_p12_is_encrypted_bounded_and_replayable() {
    fn lease_for<'a>(
        cert_id: &'a Uuid,
        art: &'a Uuid,
        op: &'a Uuid,
        subj: &'a str,
        max_bytes: u64,
        grant: &'a Uuid,
        exp: i64,
    ) -> ArtifactLeaseIdentity<'a> {
        ArtifactLeaseIdentity {
            artifact_id: art.as_bytes(),
            certificate_id: cert_id.as_bytes(),
            certificate_version: 1,
            operation_id: op.as_bytes(),
            authorized_subject: subj,
            max_bytes,
            grant_id: grant.as_bytes(),
            expires_at_unix_seconds: exp,
        }
    }

    let dir = TempDir::new("p12");
    std::fs::set_permissions(dir.path(), std::fs::Permissions::from_mode(0o700)).expect("0700");
    let user_priv = dir.join("user-sealing-private.pem");
    let user_pub = dir.join("user-sealing-public.pem");
    let p12_priv = dir.join("p12-sealing-private.pem");
    let p12_pub = dir.join("p12-sealing-public.pem");
    for (priv_p, pub_p) in [(&user_priv, &user_pub), (&p12_priv, &p12_pub)] {
        openssl_ok(&[
            "genpkey".as_ref(),
            "-algorithm".as_ref(),
            "RSA".as_ref(),
            "-pkeyopt".as_ref(),
            "rsa_keygen_bits:2048".as_ref(),
            "-out".as_ref(),
            priv_p.as_os_str(),
        ]);
        std::fs::set_permissions(priv_p, std::fs::Permissions::from_mode(0o600)).expect("0600");
        openssl_ok(&[
            "pkey".as_ref(),
            "-in".as_ref(),
            priv_p.as_os_str(),
            "-pubout".as_ref(),
            "-out".as_ref(),
            pub_p.as_os_str(),
        ]);
    }
    let resources = FixedResources::default()
        .with_user_resources(
            PathBuf::from("/usr/bin/true"),
            dir.join("unused-user-file"),
            PathBuf::from("/usr/bin/openssl"),
            user_priv,
            "fixture-user-key".to_owned(),
        )
        .expect("user res")
        .with_p12_resources(p12_priv, "fixture-key".to_owned())
        .expect("p12 res")
        .with_effect_store(
            dir.join("desired-effects.sqlite3"),
            dir.join("desired-effects.key"),
        )
        .expect("effect res")
        .with_certificate_key_dir(dir.path().to_path_buf())
        .expect("cert res");
    let limits = Limits {
        timeout: Duration::from_secs(10),
        output_bytes: DEFAULT_OUTPUT_BYTES,
    };
    let adapter = Adapter::new(resources.clone(), limits);

    let cert_id = Uuid::now_v7();
    let csr = adapter
        .certificate_csr(cert_id.as_bytes(), "vpn.example.test", &[], 2048)
        .await
        .expect("csr");
    let csr_der = dir.join("request.der");
    let csr_pem = dir.join("request.pem");
    let cert_pem = dir.join("certificate.pem");
    std::fs::write(&csr_der, csr.csr_der).expect("csr der");
    let key_path = dir.join(format!("{cert_id}.key.pem"));
    openssl_ok(&[
        "req".as_ref(),
        "-inform".as_ref(),
        "DER".as_ref(),
        "-in".as_ref(),
        csr_der.as_os_str(),
        "-out".as_ref(),
        csr_pem.as_os_str(),
    ]);
    openssl_ok(&[
        "x509".as_ref(),
        "-req".as_ref(),
        "-in".as_ref(),
        csr_pem.as_os_str(),
        "-signkey".as_ref(),
        key_path.as_os_str(),
        "-days".as_ref(),
        "1".as_ref(),
        "-out".as_ref(),
        cert_pem.as_os_str(),
    ]);

    let password = b"unique-random-password-fixture";
    let mut p12_ct = seal_rsa_oaep(&p12_pub, password);
    let mut user_ct = seal_rsa_oaep(&user_pub, password);
    if let Some(helper) = std::env::var_os("OCSERV_SIGNER_INTEROP_HELPER") {
        assert!(
            Command::new(helper)
                .arg("-test.run=^TestSealInteropHelper$")
                .env("SIGNER_INTEROP_DIR", dir.path())
                .status()
                .expect("run Go Signer interop helper")
                .success()
        );
        p12_ct = std::fs::read(dir.join("signer-1.bin")).expect("Go Signer P12 ct");
        user_ct = std::fs::read(dir.join("signer-0.bin")).expect("Go Signer user ct");
        let dec = adapter
            .execute_with_input(
                &adapter.resources.openssl,
                &[
                    "pkeyutl",
                    "-decrypt",
                    "-inkey",
                    adapter
                        .resources
                        .user_secret_key
                        .to_str()
                        .expect("key path"),
                    "-pkeyopt",
                    "rsa_padding_mode:oaep",
                    "-pkeyopt",
                    "rsa_oaep_md:sha256",
                ],
                &user_ct,
            )
            .await
            .expect("decrypt user key");
        assert_eq!(dec.stdout, password);
    }

    let sealed_p12 = SealedSecretV1 {
        version: SealedSecretVersion::V1 as i32,
        purpose: SealedSecretPurpose::CertificateP12Password as i32,
        key_id: "fixture-key".to_owned(),
        ciphertext: p12_ct,
    };
    let chain = std::fs::read(&cert_pem).expect("chain");
    let expires_at = effect_now().expect("now") + 600;
    let artifact_dir = dir.join("artifacts");

    let make_p12 = async |art: Uuid, op: Uuid, secret: &SealedSecretV1| {
        adapter
            .certificate_p12(
                cert_id.as_bytes(),
                art.as_bytes(),
                &chain,
                secret,
                1,
                op.as_bytes(),
                expires_at,
            )
            .await
    };

    // Cross-purpose and tampered ciphertexts fail closed.
    let mut tampered = sealed_p12.clone();
    tampered.ciphertext[0] ^= 1;
    let mut wrong_purpose = sealed_p12.clone();
    wrong_purpose.purpose = SealedSecretPurpose::UserPassword as i32;
    wrong_purpose.key_id = "fixture-user-key".to_owned();
    let user_ct_in_p12 = SealedSecretV1 {
        ciphertext: user_ct,
        ..sealed_p12.clone()
    };
    for bad in [&tampered, &wrong_purpose, &user_ct_in_p12] {
        assert!(make_p12(Uuid::now_v7(), Uuid::now_v7(), bad).await.is_err());
    }
    assert!(
        adapter
            .user_create(
                "cross-purpose",
                "fixture-user-key",
                &sealed_p12.ciphertext,
                1,
                test_effect()
            )
            .await
            .is_err()
    );

    // Create, verify permissions/hash/password protection, replay, and detect corruption.
    let artifact_id = Uuid::now_v7();
    let op_id = Uuid::now_v7();
    let result = make_p12(artifact_id, op_id, &sealed_p12)
        .await
        .expect("create p12");
    let artifact = artifact_dir.join(format!("{artifact_id}.p12"));
    assert_eq!(
        std::fs::metadata(&artifact_dir)
            .expect("dir")
            .permissions()
            .mode()
            & 0o777,
        0o700
    );
    assert_eq!(
        std::fs::metadata(&artifact)
            .expect("file")
            .permissions()
            .mode()
            & 0o777,
        0o600
    );
    let bytes = std::fs::read(&artifact).expect("p12 bytes");
    assert_eq!(result.artifact_size, bytes.len() as u64);
    assert_eq!(result.artifact_sha256, Sha256::digest(&bytes).to_vec());
    assert!(!bytes.windows(password.len()).any(|w| w == password));
    openssl_ok(&[
        "pkcs12".as_ref(),
        "-in".as_ref(),
        artifact.as_os_str(),
        "-passin".as_ref(),
        format!("pass:{}", String::from_utf8_lossy(password)).as_ref(),
        "-noout".as_ref(),
    ]);
    let replay = make_p12(artifact_id, op_id, &sealed_p12)
        .await
        .expect("replay");
    assert_eq!(replay.artifact_sha256, result.artifact_sha256);
    std::fs::write(&artifact, b"truncated").expect("corrupt");
    assert!(matches!(
        make_p12(artifact_id, op_id, &sealed_p12).await,
        Err(AdapterError::InvalidResource)
    ));
    assert!(!artifact.exists());

    // Interrupted staging recovery, exclusive lease, restart persistence, lease expiry/re-lease, consume, confirm.
    let int_art = Uuid::now_v7();
    let int_op = Uuid::now_v7();
    let int_chain = artifact_dir.join(format!(".{int_art}.chain.pem"));
    let int_out = artifact_dir.join(format!(".{int_art}.p12"));
    std::fs::write(&int_chain, b"stale").expect("stale chain");
    std::fs::write(&int_out, b"stale").expect("stale out");
    let int_res = make_p12(int_art, int_op, &sealed_p12)
        .await
        .expect("recover staging");
    assert!(!int_chain.exists() && !int_out.exists());

    let grant_id = Uuid::now_v7();
    let subject = Uuid::now_v7().to_string();
    let lease = lease_for(
        &cert_id,
        &int_art,
        &int_op,
        &subject,
        int_res.artifact_size,
        &grant_id,
        effect_now().expect("now") + 300,
    );
    let (a, b) = tokio::join!(
        adapter.artifact_read(lease, 0),
        adapter.artifact_read(lease, 0)
    );
    assert!(a.is_ok() ^ b.is_ok());
    let chunk = a.or(b).expect("winner");
    assert!(chunk.eof && chunk.sha256 == int_res.artifact_sha256);

    let restarted = Adapter::new(resources.clone(), limits);
    let other_grant = Uuid::now_v7();
    assert!(matches!(
        restarted
            .artifact_read(
                ArtifactLeaseIdentity {
                    grant_id: other_grant.as_bytes(),
                    ..lease
                },
                0
            )
            .await,
        Err(AdapterError::Unavailable)
    ));
    assert!(matches!(
        restarted
            .artifact_read(
                ArtifactLeaseIdentity {
                    certificate_version: 2,
                    ..lease
                },
                0
            )
            .await,
        Err(AdapterError::InvalidRequest)
    ));
    assert!(matches!(
        restarted.artifact_read(lease, 0).await,
        Err(AdapterError::InvalidRequest)
    ));
    assert!(
        !restarted
            .artifact_confirm_consumed(lease, &int_res.artifact_sha256, int_res.artifact_size)
            .await
            .expect("active lease")
            .applied
    );

    let expired_at = effect_now().expect("now") - 1;
    let mut store = expire_artifact_grant(&resources, &int_art, expired_at);
    store.reconcile_artifacts().expect("release expired lease");
    drop(store);
    assert!(
        !restarted
            .artifact_confirm_consumed(
                ArtifactLeaseIdentity {
                    expires_at_unix_seconds: expired_at,
                    ..lease
                },
                &int_res.artifact_sha256,
                int_res.artifact_size,
            )
            .await
            .expect("released")
            .applied
    );

    let rec_grant = Uuid::now_v7();
    let rec_lease = ArtifactLeaseIdentity {
        grant_id: rec_grant.as_bytes(),
        expires_at_unix_seconds: effect_now().expect("now") + 300,
        ..lease
    };
    assert!(
        restarted
            .artifact_read(rec_lease, 0)
            .await
            .expect("re-lease")
            .eof
    );
    restarted
        .artifact_consume(rec_lease, &int_res.artifact_sha256, int_res.artifact_size)
        .await
        .expect("consume");
    restarted
        .artifact_consume(rec_lease, &int_res.artifact_sha256, int_res.artifact_size)
        .await
        .expect("idempotent");
    let elapsed = effect_now().expect("now") - 1;
    drop(expire_artifact_grant(&resources, &int_art, elapsed));
    let expired_lease = ArtifactLeaseIdentity {
        expires_at_unix_seconds: elapsed,
        ..rec_lease
    };
    assert!(
        restarted
            .artifact_confirm_consumed(
                expired_lease,
                &int_res.artifact_sha256,
                int_res.artifact_size
            )
            .await
            .expect("confirm consumed")
            .applied
    );
    assert!(matches!(
        restarted
            .artifact_consume(
                expired_lease,
                &int_res.artifact_sha256,
                int_res.artifact_size
            )
            .await,
        Err(AdapterError::InvalidRequest)
    ));
    assert!(matches!(
        restarted.artifact_read(rec_lease, 0).await,
        Err(AdapterError::InvalidRequest)
    ));
    assert!(!artifact_dir.join(format!("{int_art}.p12")).exists());

    // Stale/untracked cleanup and mode hardening.
    let lookalike = artifact_dir.join("not-a-uuid.p12");
    std::fs::write(&lookalike, b"preserve").expect("lookalike");
    let legacy = artifact_dir.join(format!("{}.p12", Uuid::now_v7()));
    std::fs::write(&legacy, b"legacy").expect("legacy");
    std::fs::set_permissions(&artifact_dir, std::fs::Permissions::from_mode(0o710)).expect("0710");
    assert!(
        Command::new("/usr/bin/touch")
            .args(["-t", "200001010000"])
            .arg(&lookalike)
            .status()
            .expect("touch")
            .success()
    );
    adapter
        .cleanup_stale_certificate_artifacts()
        .await
        .expect("cleanup");
    assert!(!legacy.exists() && lookalike.exists());
    assert_eq!(
        std::fs::symlink_metadata(&artifact_dir)
            .expect("meta")
            .mode()
            & 0o777,
        0o700
    );

    // Certificate revoke removes available, leased, prepared, and staging artifacts.
    let avail_art = Uuid::now_v7();
    let avail_op = Uuid::now_v7();
    let avail_res = make_p12(avail_art, avail_op, &sealed_p12)
        .await
        .expect("avail");
    let leased_art = Uuid::now_v7();
    let leased_op = Uuid::now_v7();
    let leased_res = make_p12(leased_art, leased_op, &sealed_p12)
        .await
        .expect("leased");
    let rev_grant = Uuid::now_v7();
    let rev_subj = Uuid::now_v7().to_string();
    let rev_lease = lease_for(
        &cert_id,
        &leased_art,
        &leased_op,
        &rev_subj,
        leased_res.artifact_size,
        &rev_grant,
        effect_now().expect("now") + 300,
    );
    adapter
        .artifact_read(rev_lease, 0)
        .await
        .expect("lease before revoke");
    let prep_art = Uuid::now_v7();
    EffectStore::open_for_mutation(&resources)
        .expect("store")
        .prepare_artifact(
            prep_art.as_bytes(),
            cert_id.as_bytes(),
            1,
            Uuid::now_v7().as_bytes(),
            expires_at,
        )
        .expect("prepare");
    let staging_paths = [
        artifact_dir.join(format!(".{avail_art}.p12")),
        artifact_dir.join(format!(".{avail_art}.chain.pem")),
        artifact_dir.join(format!(".{prep_art}.p12")),
        artifact_dir.join(format!(".{prep_art}.chain.pem")),
    ];
    for p in &staging_paths {
        std::fs::write(p, b"staging").expect("staging");
    }
    assert!(
        adapter
            .certificate_revoke(cert_id.as_bytes(), 1)
            .await
            .expect("revoke")
            .key_removed
    );
    assert!(
        adapter
            .certificate_revoke(cert_id.as_bytes(), 1)
            .await
            .expect("repeat")
            .key_removed
    );
    for id in [avail_art, leased_art] {
        assert!(!artifact_dir.join(format!("{id}.p12")).exists());
    }
    assert!(staging_paths.iter().all(|p| !p.exists()));
    let unavail_grant = Uuid::now_v7();
    let unavail_lease = ArtifactLeaseIdentity {
        artifact_id: avail_art.as_bytes(),
        operation_id: avail_op.as_bytes(),
        max_bytes: avail_res.artifact_size,
        grant_id: unavail_grant.as_bytes(),
        ..rev_lease
    };
    assert!(matches!(
        adapter.artifact_read(unavail_lease, 0).await,
        Err(AdapterError::InvalidRequest)
    ));
    assert!(matches!(
        adapter.artifact_read(rev_lease, 0).await,
        Err(AdapterError::InvalidRequest)
    ));
}

#[test]
fn effect_store_concurrent_writers_zero_busy() {
    let dir = TempDir::new("effect-busy");
    let resources = dir.effect_resources();
    let now = effect_now().expect("now");
    let expires_at = now + 300;
    let artifact_id = Uuid::now_v7();
    let cert_id = Uuid::now_v7();
    let artifact_op = Uuid::now_v7();

    // Initialize the store once before spawning concurrent writers, matching
    // production startup where privd initializes its state before serving RPCs.
    {
        let mut init = EffectStore::open_for_mutation(&resources).expect("init effect store");
        init.prepare_artifact(
            artifact_id.as_bytes(),
            cert_id.as_bytes(),
            1,
            artifact_op.as_bytes(),
            expires_at,
        )
        .expect("prepare active artifact");
        init.register_artifact(
            artifact_id.as_bytes(),
            cert_id.as_bytes(),
            1,
            artifact_op.as_bytes(),
            &[0x42; 32],
            128,
            expires_at,
        )
        .expect("register active artifact");
    }

    let node_id = Uuid::now_v7();
    let threads = 4usize;
    let ops_per_thread = 12usize;
    let barrier = std::sync::Arc::new(std::sync::Barrier::new(threads));
    let mut handles = Vec::with_capacity(threads);

    for worker in 0..threads {
        let resources = resources.clone();
        let barrier = std::sync::Arc::clone(&barrier);
        handles.push(std::thread::spawn(move || {
            barrier.wait();
            let mut store = EffectStore::open_for_mutation(&resources)
                .expect("concurrent open_for_mutation must not hit SQLITE_BUSY");
            for step in 0..ops_per_thread {
                let command_id = Uuid::now_v7();
                let idempotency_key = Uuid::now_v7();
                let operation_id = Uuid::now_v7();
                let payload_hash = [u8::try_from(worker * 16 + step).expect("byte"); 32];
                let resource_key = format!("worker-{worker}/item-{step}");
                let identity = AuthorizedEffectIdentity {
                    node_id: node_id.as_bytes(),
                    command_id: command_id.as_bytes(),
                    operation_id: operation_id.as_bytes(),
                    idempotency_key: idempotency_key.as_bytes(),
                    semantic_payload_sha256: &payload_hash,
                    action: "service.reload",
                    authorization_revision: 1,
                    effect_kind: "service",
                    resource_key: &resource_key,
                    effect_revision: u64::try_from(step + 1).expect("rev"),
                    expires_at_unix_seconds: expires_at,
                    retry_if_absent: false,
                };
                let decision = store
                    .prepare_authorized(identity)
                    .expect("prepare_authorized must wait on busy_timeout without SQLITE_BUSY");
                assert!(matches!(decision, AuthorizedEffectDecision::Execute));
                let response = format!("ok-{worker}-{step}");
                store
                    .complete_authorized(identity, response.as_bytes())
                    .expect("complete_authorized must succeed under write contention");
                let replayed = store
                    .replay_authorized(identity)
                    .expect("replay_authorized must validate HMAC");
                assert_eq!(replayed.as_deref(), Some(response.as_bytes()));
            }
        }));
    }

    for handle in handles {
        handle.join().expect("writer thread must not panic");
    }

    let mut verify = EffectStore::open_existing(&resources).expect("reopen effect store");
    let count: i64 = verify
        .connection
        .query_row(
            "SELECT count(*) FROM authorized_effects WHERE state='applied'",
            [],
            |row| row.get(0),
        )
        .expect("count applied effects");
    assert_eq!(
        count,
        i64::try_from(threads * ops_per_thread).expect("expected count")
    );

    // Non-expired available artifacts must remain untouched by reconcile_artifacts
    // without dirtying the row or reporting them terminal.
    let before = query_artifact(&verify.connection, artifact_id.as_bytes())
        .expect("query before")
        .expect("artifact present");
    let terminal = verify.reconcile_artifacts().expect("reconcile_artifacts");
    assert_eq!(terminal, Vec::<[u8; 16]>::new());
    let after = query_artifact(&verify.connection, artifact_id.as_bytes())
        .expect("query after")
        .expect("artifact still present");
    assert_eq!(after.state, "available");
    assert_eq!(after.record_hmac, before.record_hmac);
}
