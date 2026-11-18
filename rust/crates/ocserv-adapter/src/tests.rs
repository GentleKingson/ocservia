use super::effect_store::*;
use super::user_file::*;

use std::os::unix::fs::PermissionsExt;

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

fn executable(name: &str, body: &str) -> PathBuf {
    let directory = std::env::temp_dir().join(format!("ocservia-adapter-{}", uuid::Uuid::now_v7()));
    std::fs::create_dir_all(&directory).expect("create fixture directory");
    let path = directory.join(name);
    write_executable(&path, format!("#!/bin/sh\n{body}\n"));
    path
}

pub(super) fn write_executable(path: &Path, bytes: impl AsRef<[u8]>) {
    // Keep writable script descriptors out of this multithreaded process:
    // a parallel fork can retain them until exec and cause ETXTBSY.
    let mut writer = std::process::Command::new("tee")
        .arg(path)
        .stdin(Stdio::piped())
        .stdout(Stdio::null())
        .spawn()
        .expect("start fixture writer");
    writer
        .stdin
        .take()
        .expect("fixture writer stdin")
        .write_all(bytes.as_ref())
        .expect("write fixture program");
    assert!(writer.wait().expect("wait for fixture writer").success());
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700))
        .expect("make fixture executable");
}

fn write_user_fixture(path: &Path, bytes: impl AsRef<[u8]>) {
    std::fs::write(path, bytes).expect("write ocpasswd fixture");
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(USER_FILE_MODE))
        .expect("secure ocpasswd fixture");
}

#[test]
fn version_fixtures_and_malformed_output() {
    assert_eq!(
        parse_version(b"ocserv 1.2.4\n")
            .expect("1.2 fixture")
            .version,
        "1.2.4"
    );
    assert_eq!(
        parse_version(
            b"OpenConnect VPN Server 1.3.0\n\nCompiled with: seccomp, PAM\nGnuTLS version: 3.8.9\n"
        )
        .expect("1.3 fixture")
        .version,
        "1.3.0"
    );
    assert_eq!(
        parse_version(b"ocserv 1.4.1\n")
            .expect("1.4 fixture")
            .version,
        "1.4.1"
    );
    assert_eq!(
        parse_version(b"OpenConnect VPN Server 1.5.0\n")
            .expect("1.5.0 fixture")
            .version,
        "1.5.0"
    );
    for prefix in ["ocserv", "OpenConnect VPN Server"] {
        assert_eq!(
            parse_version(format!("{prefix} 1.6.0\n").as_bytes())
                .expect("1.6.0 fixture")
                .version,
            "1.6.0"
        );
        for version in [
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
                parse_version(format!("{prefix} {version}\n").as_bytes()).is_err(),
                "unreviewed version {version} accepted"
            );
        }
    }
    assert!(parse_version(b"ocserv development\n").is_err());
    assert!(parse_version(&[0xff]).is_err());
}

#[tokio::test]
async fn host_preflight_reuses_snapshot_checks_without_mutating_host() {
    let directory = std::env::temp_dir().join(format!("ocservia-preflight-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("directory");
    let program = directory.join("snapshot-command");
    write_executable(&program, b"#!/bin/sh\ncase \"$1\" in\n--version) echo 'ocserv 1.3.0';;\nshow) printf 'LoadState=loaded\\nActiveState=active\\nSubState=running\\n';;\n--json) echo '[]';;\n*) exit 99;;\nesac\n");
    let config = directory.join("ocserv.conf");
    std::fs::write(&config, b"tcp-port = 443\n").expect("config");
    let users = directory.join("ocpasswd");
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
        directory.join("absent-key.pem"),
        "user-key".into(),
    )
    .expect("user resources")
    .with_effect_store(
        directory.join("absent.sqlite3"),
        directory.join("absent.key"),
    )
    .expect("store");
    let adapter = Adapter::new(resources, Limits::default());
    adapter.host_preflight().await.expect("healthy snapshot");
    assert_eq!(std::fs::read(&config).expect("config"), b"tcp-port = 443\n");
    assert_eq!(
        std::fs::read(&users).expect("users"),
        b"alice::private-hash"
    );
    assert!(!directory.join("absent.sqlite3").exists());
    assert!(!directory.join("absent.key").exists());
    assert!(!directory.join("absent-key.pem").exists());
    std::fs::set_permissions(&users, std::fs::Permissions::from_mode(0o644)).expect("mode");
    assert!(matches!(
        adapter.host_preflight().await,
        Err(AdapterError::ResourceMetadata { reason: "mode", .. })
    ));
    std::fs::remove_dir_all(directory).expect("cleanup");
}

#[tokio::test]
async fn reads_packaged_version_banner_from_stderr() {
    let version_program = executable(
        "ocserv-version",
        "printf 'OpenConnect VPN Server 1.3.0\\n' >&2",
    );
    let fixture_directory = version_program.parent().expect("fixture parent").to_owned();
    let resources = FixedResources::new(
        PathBuf::from("/bin/false"),
        version_program,
        PathBuf::from("/bin/false"),
        PathBuf::from("/etc/hosts"),
        PathBuf::from("/proc/sys/kernel/random/boot_id"),
    )
    .expect("fixed resources");
    let version = Adapter::new(resources, Limits::default())
        .ocserv_version()
        .await
        .expect("stderr version banner");
    assert_eq!(version.version, "1.3.0");
    std::fs::remove_dir_all(fixture_directory).expect("remove version fixture");
}

#[tokio::test]
async fn stale_config_plan_staging_files_are_cleaned_on_startup() {
    let directory = std::env::temp_dir().join(format!("ocservia-plan-clean-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("create fixture directory");
    let config = directory.join("ocserv.conf");
    std::fs::write(&config, b"tcp-port = 444\n").expect("write config");
    let resources = FixedResources::new(
        PathBuf::from("/bin/false"),
        PathBuf::from("/bin/false"),
        PathBuf::from("/bin/false"),
        config,
        PathBuf::from("/proc/sys/kernel/random/boot_id"),
    )
    .expect("fixed resources");
    let adapter = Adapter::new(resources, Limits::default());
    let stale = directory.join(format!(".ocservia-plan-{}", Uuid::now_v7()));
    let lookalike = directory.join(".ocservia-plan-not-a-uuid");
    std::fs::write(&stale, b"stale").expect("stale staging");
    std::fs::write(&lookalike, b"preserve").expect("lookalike");
    adapter
        .cleanup_stale_config_plans()
        .await
        .expect("startup cleanup");
    assert!(!stale.exists());
    assert!(lookalike.exists());
    std::fs::remove_dir_all(directory).expect("remove plan fixture");
}

#[test]
fn parses_session_and_ban_fixtures() {
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
        let sessions = parse_sessions(users).expect("sessions fixture");
        assert_eq!(sessions.sessions.len(), 2);
        assert_eq!(sessions.sessions[1].remote_ip, "2001:db8::1");
        let bans = parse_ip_bans(bans).expect("bans fixture");
        assert_eq!(bans.bans.len(), 2);
        assert_eq!(bans.bans[1].seconds_remaining, None);
    }
    assert!(parse_ip_bans(b"[{\"IP\":\"192.0.2.1\"},]").is_err());
}

#[test]
fn user_and_group_parsers_drop_hashes_and_reject_unsafe_names() {
    let users = parse_user_file(b"alice:*:$6$secret-hash\nbob:x:!$6$disabled\n").expect("users");
    assert_eq!(users.users.len(), 2);
    assert!(users.users[0].enabled);
    assert!(!users.users[1].enabled);
    assert!(!format!("{users:?}").contains("secret-hash"));
    let groups = parse_groups_from_user_file(b"alice:staff:$6$alice\nbob:admins,staff:$6$bob\n")
        .expect("groups");
    let staff = groups
        .groups
        .iter()
        .find(|group| group.group_name == "staff")
        .expect("staff");
    assert_eq!(staff.members, ["alice", "bob"]);
    assert!(parse_user_file(b"../root:hash\n").is_err());
    assert!(parse_groups_from_user_file(b"alice:staff;id:$6$hash\n").is_err());
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
        assert!(matches!(&error, AdapterError::UserFileFormat {
                reason: r, line: l, fields: f, first_line: first,
            } if *r == reason && *l == line && *f == fields && *first == first_line));
        let debug = format!("{error:?}");
        let wire = PrivdError::from(error);
        assert_eq!(wire.kind, i32::from(ErrorKind::MalformedOutput));
        for output in [debug, wire.detail] {
            assert!(!output.contains("private-hash"));
            assert!(!output.contains("other-hash"));
            assert!(!output.contains("alice"));
        }
    }
    for input in [
        "",
        "\n\n",
        "alice::x",
        "alice::arbitrary-length-hash\n\nbob:x:!",
    ] {
        parse_user_file(input.as_bytes()).expect("previously supported records");
    }
}

#[tokio::test]
async fn resource_diagnostics_distinguish_mode_owner_group_links_and_missing() {
    let directory =
        std::env::temp_dir().join(format!("ocservia-resource-diagnostic-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("directory");
    let path = directory.join("ocpasswd");
    assert!(
        read_optional_user_file(&path)
            .await
            .expect("optional absence")
            .is_empty()
    );
    assert!(matches!(
        read_user_file(&path, false).await,
        Err(AdapterError::ResourceIo {
            reason: "open_failed",
            ..
        })
    ));
    write_user_fixture(&path, b"alice::private-hash");
    read_user_file(&path, false).await.expect("0600 accepted");
    std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).expect("mode");
    let error = read_user_file(&path, false)
        .await
        .expect_err("0644 rejected");
    assert!(matches!(
        error,
        AdapterError::ResourceMetadata {
            reason: "mode",
            actual: 0o644,
            expected: 0o600,
            ..
        }
    ));
    assert!(error.to_string().contains("actual=0644 expected=0600"));
    std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)).expect("mode");
    let file = File::open(&path).expect("file");
    let (uid, gid) = authoritative_user_owner();
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
    let link = directory.join("link");
    std::fs::hard_link(&path, &link).expect("hardlink");
    assert!(matches!(
        read_user_file(&path, false).await,
        Err(AdapterError::ResourceMetadata {
            reason: "link_count",
            ..
        })
    ));
    std::fs::remove_file(&link).expect("remove hardlink");
    std::os::unix::fs::symlink(&path, &link).expect("symlink");
    assert!(matches!(
        read_user_file(&link, true).await,
        Err(AdapterError::ResourceIo {
            reason: "symlink",
            ..
        })
    ));
    assert!(matches!(
        validate_authoritative_user_file(&File::open(&directory).expect("directory"), uid, gid),
        Err(AdapterError::ResourceMetadata {
            reason: "file_type_regular",
            ..
        })
    ));
    std::fs::remove_dir_all(directory).expect("cleanup");
}

#[tokio::test]
async fn ocpasswd_publish_enforces_owner_mode_links_and_real_parent() {
    let directory =
        std::env::temp_dir().join(format!("ocservia-ocpasswd-metadata-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("fixture directory");
    let users = directory.join("ocpasswd");
    let resources = FixedResources::default()
        .with_user_resources(
            PathBuf::from("/bin/false"),
            users.clone(),
            PathBuf::from("/bin/false"),
            directory.join("key.pem"),
            String::from("test-key"),
        )
        .expect("resources")
        .with_effect_store(
            directory.join("effects.sqlite3"),
            directory.join("effects.key"),
        )
        .expect("effect resources");
    let adapter = Adapter::new(resources, Limits::default());
    let desired = b"alice:staff:$6$hash\n";
    let mut staging = adapter
        .stage_user_file(desired)
        .await
        .expect("secure staging");
    adapter
        .commit_desired_staging(&mut staging, b"", "user_create", "alice", 1, test_effect())
        .await
        .expect("secure publish");
    let metadata = std::fs::metadata(&users).expect("published metadata");
    let (uid, gid) = authoritative_user_owner();
    assert_eq!(metadata.uid(), uid);
    assert_eq!(metadata.gid(), gid);
    assert_eq!(metadata.permissions().mode() & 0o777, USER_FILE_MODE);
    assert_eq!(metadata.nlink(), 1);

    std::fs::set_permissions(&users, std::fs::Permissions::from_mode(0o660))
        .expect("make legacy file unsafe");
    assert!(matches!(
        adapter.user_list().await,
        Err(AdapterError::ResourceMetadata { .. })
    ));
    std::fs::set_permissions(&users, std::fs::Permissions::from_mode(USER_FILE_MODE))
        .expect("restore secure mode");

    let second_link = directory.join("ocpasswd-copy");
    std::fs::hard_link(&users, &second_link).expect("hard link fixture");
    assert!(matches!(
        adapter.user_list().await,
        Err(AdapterError::ResourceMetadata { .. })
    ));
    std::fs::remove_file(&second_link).expect("remove hard link fixture");

    let real_users = directory.join("ocpasswd-real");
    std::fs::rename(&users, &real_users).expect("preserve real file");
    std::os::unix::fs::symlink(&real_users, &users).expect("symlink fixture");
    assert!(adapter.user_list().await.is_err());
    std::fs::remove_file(&users).expect("remove symlink fixture");
    std::fs::rename(&real_users, &users).expect("restore real file");

    std::fs::set_permissions(&directory, std::fs::Permissions::from_mode(0o770))
        .expect("make parent unsafe");
    assert!(matches!(
        adapter.user_list().await,
        Err(AdapterError::ResourceMetadata { .. })
    ));
    std::fs::set_permissions(&directory, std::fs::Permissions::from_mode(0o700))
        .expect("restore fixture directory");
    std::fs::remove_dir_all(directory).expect("cleanup");
}

#[tokio::test]
async fn group_apply_is_atomic_when_replacement_fails() {
    let directory = std::env::temp_dir().join(format!("ocservia-group-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("directory");
    let users = directory.join("ocpasswd");
    write_user_fixture(&users, b"alice:staff:$6$alice-hash\nbob:*:!$6$bob-hash\n");
    let resources = FixedResources::default()
        .with_user_resources(
            PathBuf::from("/bin/false"),
            users.clone(),
            PathBuf::from("/bin/false"),
            directory.join("key.pem"),
            String::from("test-key"),
        )
        .expect("resources")
        .with_effect_store(
            directory.join("effects.sqlite3"),
            directory.join("effects.key"),
        )
        .expect("effect resources");
    let adapter = Adapter::new(resources, Limits::default());
    assert!(
        adapter
            .group_apply(
                "staff",
                &["alice".to_owned(), "bob".to_owned()],
                1,
                test_effect(),
            )
            .await
            .is_ok()
    );
    assert_eq!(
        adapter.group_list().await.expect("groups").groups[0].members,
        ["alice", "bob"]
    );
    assert_eq!(
        adapter
            .desired_effect_observe("group_apply", "staff", 1, test_effect())
            .await
            .expect("group effect")
            .state,
        DesiredEffectState::AppliedExact as i32
    );
    assert_eq!(
        adapter
            .desired_effect_observe("group_apply", "staff", 2, test_effect())
            .await
            .expect("stale group effect")
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
    assert_eq!(std::fs::read(&users).expect("unchanged"), updated);
    std::fs::remove_dir_all(directory).expect("cleanup");
}

#[tokio::test]
async fn maximum_group_apply_succeeds_and_overflow_is_side_effect_free() {
    let directory = std::env::temp_dir().join(format!("ocservia-group-max-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("directory");
    let users = directory.join("ocpasswd");
    let members = (0..MAX_MANAGED_RESOURCES)
        .map(|index| format!("user{index:03}"))
        .collect::<Vec<_>>();
    let mut original = String::new();
    for member in &members {
        original.push_str(member);
        original.push_str(":*:$6$test-hash\n");
    }
    write_user_fixture(&users, original);
    let resources = FixedResources::default()
        .with_user_resources(
            PathBuf::from("/bin/false"),
            users.clone(),
            PathBuf::from("/bin/false"),
            directory.join("key.pem"),
            String::from("test-key"),
        )
        .expect("resources")
        .with_effect_store(
            directory.join("effects.sqlite3"),
            directory.join("effects.key"),
        )
        .expect("effect resources");
    let adapter = Adapter::new(resources, Limits::default());
    adapter
        .group_apply("staff", &members, 1, test_effect())
        .await
        .expect("maximum group apply");
    assert_eq!(
        adapter.group_list().await.expect("groups").groups[0]
            .members
            .len(),
        384
    );
    let applied = std::fs::read(&users).expect("applied users");
    let mut overflow = members;
    overflow.push("overflow".to_owned());
    assert!(matches!(
        adapter
            .group_apply("staff", &overflow, 2, test_effect())
            .await,
        Err(AdapterError::InvalidRequest)
    ));
    assert_eq!(std::fs::read(&users).expect("unchanged users"), applied);
    std::fs::remove_dir_all(directory).expect("cleanup");
}

#[tokio::test]
async fn aggregate_group_capacity_rejection_is_terminal_and_pre_effect() {
    let directory =
        std::env::temp_dir().join(format!("ocservia-group-aggregate-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("directory");
    let users = directory.join("ocpasswd");
    let mut original = Vec::new();
    for index in 0..MAX_MANAGED_RESOURCES {
        original.extend_from_slice(format!("user{index:03}:legacy:$6$test-hash\n").as_bytes());
    }
    write_user_fixture(&users, &original);
    let effect_database = directory.join("effects.sqlite3");
    let effect_key = directory.join("effects.key");
    let resources = FixedResources::default()
        .with_user_resources(
            PathBuf::from("/bin/false"),
            users.clone(),
            PathBuf::from("/bin/false"),
            directory.join("key.pem"),
            String::from("test-key"),
        )
        .expect("resources")
        .with_effect_store(effect_database.clone(), effect_key.clone())
        .expect("effect resources");
    let adapter = Adapter::new(resources, Limits::default());

    let result = adapter
        .group_apply("new-group", &["user000".to_owned()], 1, test_effect())
        .await;

    assert!(
        matches!(result, Err(AdapterError::CapacityExceeded)),
        "unexpected result: {result:?}"
    );
    assert_eq!(std::fs::read(&users).expect("unchanged users"), original);
    assert!(!effect_database.exists());
    assert!(!effect_key.exists());
    std::fs::remove_dir_all(directory).expect("cleanup");
}

#[test]
fn capacity_error_has_a_distinct_privilege_boundary_kind() {
    let error = PrivdError::from(AdapterError::CapacityExceeded);
    assert_eq!(error.kind, i32::from(ErrorKind::CapacityExceeded));
    assert_eq!(error.detail, "authoritative state capacity exceeded");
}

#[tokio::test]
async fn failed_password_rotation_preserves_original_record() {
    let directory = std::env::temp_dir().join(format!("ocservia-secret-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("directory");
    let users = directory.join("ocpasswd");
    let original = b"alice:admins,staff:!$6$original-hash\n";
    write_user_fixture(&users, original);
    let openssl = executable("openssl-fixture", "printf rotated-password");
    let openssl_directory = openssl.parent().expect("openssl parent").to_owned();
    let ocpasswd = executable("ocpasswd-fixture", "printf corrupted > \"$2\"; exit 1");
    let ocpasswd_directory = ocpasswd.parent().expect("ocpasswd parent").to_owned();
    let resources = FixedResources::default()
        .with_user_resources(
            ocpasswd,
            users.clone(),
            openssl,
            directory.join("key.pem"),
            String::from("test-key"),
        )
        .expect("resources")
        .with_effect_store(
            directory.join("effects.sqlite3"),
            directory.join("effects.key"),
        )
        .expect("effect resources");
    let adapter = Adapter::new(resources, Limits::default());
    assert!(
        adapter
            .user_password_rotate("alice", "test-key", &[7_u8; 64], 2, test_effect())
            .await
            .is_err()
    );
    assert_eq!(std::fs::read(&users).expect("preserved"), original);
    std::fs::remove_dir_all(directory).expect("cleanup directory");
    std::fs::remove_dir_all(openssl_directory).expect("cleanup openssl");
    std::fs::remove_dir_all(ocpasswd_directory).expect("cleanup ocpasswd");
}

fn staging_files(directory: &Path) -> Vec<PathBuf> {
    std::fs::read_dir(directory)
        .expect("read staging directory")
        .filter_map(Result::ok)
        .map(|entry| entry.path())
        .filter(|path| {
            path.file_name()
                .and_then(|name| name.to_str())
                .is_some_and(|name| name.starts_with(".ocpasswd.ocservia-"))
        })
        .collect()
}

#[tokio::test]
#[allow(clippy::too_many_lines)]
async fn staging_files_are_removed_on_timeout_cancellation_and_startup() {
    let directory = std::env::temp_dir().join(format!("ocservia-staging-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("directory");
    let users = directory.join("ocpasswd");
    let original = b"alice:staff:!$6$original-hash\n";
    write_user_fixture(&users, original);
    let openssl = executable("openssl-fast", "cat >/dev/null; printf rotated-password");
    let openssl_directory = openssl.parent().expect("openssl parent").to_owned();
    let slow_ocpasswd = executable(
        "ocpasswd-slow",
        "cat >/dev/null; cp \"$2\" \"$2.tmp\"; sleep 5",
    );
    let slow_directory = slow_ocpasswd
        .parent()
        .expect("slow ocpasswd parent")
        .to_owned();
    let resources = FixedResources::default()
        .with_user_resources(
            slow_ocpasswd,
            users.clone(),
            openssl.clone(),
            directory.join("key.pem"),
            String::from("test-key"),
        )
        .expect("resources")
        .with_effect_store(
            directory.join("effects.sqlite3"),
            directory.join("effects.key"),
        )
        .expect("effect resources");
    let adapter = Adapter::new(
        resources,
        Limits {
            timeout: Duration::from_millis(500),
            output_bytes: DEFAULT_OUTPUT_BYTES,
        },
    );
    let guard = adapter.user_file_lock.lock().await;
    assert!(
        tokio::time::timeout(
            Duration::from_millis(20),
            adapter.user_password_rotate("alice", "test-key", &[7_u8; 64], 2, test_effect()),
        )
        .await
        .is_err()
    );
    drop(guard);
    assert!(staging_files(&directory).is_empty());
    assert_eq!(
        std::fs::read(&users).expect("queued cancellation"),
        original
    );
    let timeout_result = adapter
        .user_password_rotate("alice", "test-key", &[7_u8; 64], 2, test_effect())
        .await;
    assert!(
        matches!(timeout_result, Err(AdapterError::DeadlineExceeded)),
        "password staging should reach its deadline"
    );
    assert!(staging_files(&directory).is_empty());
    assert_eq!(
        std::fs::read(&users).expect("preserved after timeout"),
        original
    );

    let slow_openssl = executable("openssl-slow", "sleep 5");
    let slow_openssl_directory = slow_openssl
        .parent()
        .expect("slow openssl parent")
        .to_owned();
    let decrypt_adapter = Adapter::new(
        adapter
            .resources
            .clone()
            .with_user_resources(
                PathBuf::from("/bin/false"),
                users.clone(),
                slow_openssl,
                directory.join("key.pem"),
                String::from("test-key"),
            )
            .expect("slow decrypt resources"),
        Limits {
            timeout: Duration::from_millis(500),
            output_bytes: DEFAULT_OUTPUT_BYTES,
        },
    );
    let decrypt_timeout_result = decrypt_adapter
        .user_password_rotate("alice", "test-key", &[7_u8; 64], 2, test_effect())
        .await;
    assert!(
        matches!(decrypt_timeout_result, Err(AdapterError::DeadlineExceeded)),
        "password decryption should reach its deadline"
    );
    assert!(staging_files(&directory).is_empty());
    assert_eq!(
        std::fs::read(&users).expect("preserved after decrypt timeout"),
        original
    );

    let cancellation_marker = directory.join("cancellation-started");
    let cancellation_ocpasswd = executable(
        "ocpasswd-cancel",
        &format!(
            "cp \"$2\" \"$2.tmp\"\ntouch '{}'\nsleep 10",
            cancellation_marker.display()
        ),
    );
    let cancellation_directory = cancellation_ocpasswd
        .parent()
        .expect("cancellation ocpasswd parent")
        .to_owned();
    let cancellation_adapter = Adapter::new(
        adapter
            .resources
            .clone()
            .with_user_resources(
                cancellation_ocpasswd,
                users.clone(),
                openssl.clone(),
                directory.join("key.pem"),
                String::from("test-key"),
            )
            .expect("cancellation resources"),
        Limits {
            timeout: Duration::from_secs(30),
            output_bytes: DEFAULT_OUTPUT_BYTES,
        },
    );
    let task = tokio::spawn(async move {
        cancellation_adapter
            .user_password_rotate("alice", "test-key", &[7_u8; 64], 2, test_effect())
            .await
    });
    for _ in 0..500 {
        if cancellation_marker.exists() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    assert!(cancellation_marker.exists());
    assert!(!staging_files(&directory).is_empty());
    task.abort();
    let _ = task.await;
    assert!(staging_files(&directory).is_empty());

    // Consume password input before exiting; only the subsequent lock command times out.
    let lock_ocpasswd = executable(
        "ocpasswd-lock-slow",
        "if [ \"$3\" = \"-l\" ]; then sleep 5; exit 0; fi\ncat >/dev/null\nprintf '%s\\n' 'alice:staff:$6$new-hash' > \"$2\"",
    );
    let lock_directory = lock_ocpasswd
        .parent()
        .expect("lock ocpasswd parent")
        .to_owned();
    let lock_resources = adapter
        .resources
        .clone()
        .with_user_resources(
            lock_ocpasswd,
            users.clone(),
            openssl,
            directory.join("key.pem"),
            String::from("test-key"),
        )
        .expect("lock resources");
    let lock_adapter = Adapter::new(
        lock_resources,
        Limits {
            timeout: Duration::from_millis(500),
            output_bytes: DEFAULT_OUTPUT_BYTES,
        },
    );
    let lock_timeout_result = lock_adapter
        .user_password_rotate("alice", "test-key", &[7_u8; 64], 2, test_effect())
        .await;
    assert!(
        matches!(lock_timeout_result, Err(AdapterError::DeadlineExceeded)),
        "password locking should reach its deadline"
    );
    assert!(staging_files(&directory).is_empty());
    assert_eq!(
        std::fs::read(&users).expect("preserved after lock timeout"),
        original
    );

    let stale = directory.join(format!(".ocpasswd.ocservia-{}", Uuid::now_v7()));
    let stale_scratch = directory.join(format!(".ocpasswd.ocservia-{}.tmp", Uuid::now_v7()));
    let lookalike = directory.join(".ocpasswd.ocservia-not-a-uuid");
    write_user_fixture(&stale, b"secret hash staging");
    write_user_fixture(&stale_scratch, b"incomplete child scratch");
    std::fs::write(&lookalike, b"preserve").expect("lookalike");
    lock_adapter
        .cleanup_stale_user_staging()
        .await
        .expect("startup cleanup");
    assert!(!stale.exists());
    assert!(!stale_scratch.exists());
    assert!(lookalike.exists());

    std::fs::remove_dir_all(directory).expect("cleanup directory");
    std::fs::remove_dir_all(openssl_directory).expect("cleanup openssl");
    std::fs::remove_dir_all(slow_openssl_directory).expect("cleanup slow openssl");
    std::fs::remove_dir_all(slow_directory).expect("cleanup slow ocpasswd");
    std::fs::remove_dir_all(cancellation_directory).expect("cleanup cancellation ocpasswd");
    std::fs::remove_dir_all(lock_directory).expect("cleanup lock ocpasswd");
}

#[tokio::test]
async fn startup_cleanup_accepts_private_child_group_before_normalization() {
    let directory = std::env::temp_dir().join(format!("ocservia-child-group-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("directory");
    let users = directory.join("ocpasswd");
    let resources = FixedResources::default()
        .with_user_resources(
            PathBuf::from("/usr/bin/ocpasswd"),
            users,
            PathBuf::from("/usr/bin/openssl"),
            directory.join("key.pem"),
            String::from("test-key"),
        )
        .expect("resources");
    let staging = directory.join(format!(".ocpasswd.ocservia-{}", Uuid::now_v7()));
    let scratch = directory.join(format!(".ocpasswd.ocservia-{}.tmp", Uuid::now_v7()));
    for path in [&staging, &scratch] {
        write_user_fixture(path, b"incomplete child output");
        assert_eq!(
            std::fs::metadata(path).expect("child metadata").gid(),
            rustix::process::getegid().as_raw()
        );
    }
    Adapter::new(resources, Limits::default())
        .cleanup_stale_user_staging()
        .await
        .expect("cleanup before normalization");
    assert!(!staging.exists() && !scratch.exists());
    std::fs::remove_dir_all(directory).expect("cleanup fixture");
}

#[tokio::test]
async fn create_and_rotate_enforce_authoritative_existence_preconditions() {
    let directory = std::env::temp_dir().join(format!("ocservia-existence-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("directory");
    let users = directory.join("ocpasswd");
    let original = b"alice:admins,staff:!$6$original-hash\n";
    write_user_fixture(&users, original);
    let resources = FixedResources::default()
        .with_user_resources(
            PathBuf::from("/bin/false"),
            users.clone(),
            PathBuf::from("/bin/false"),
            directory.join("key.pem"),
            String::from("test-key"),
        )
        .expect("resources")
        .with_effect_store(
            directory.join("effects.sqlite3"),
            directory.join("effects.key"),
        )
        .expect("effect resources");
    let adapter = Adapter::new(resources, Limits::default());
    assert!(matches!(
        adapter
            .user_create("alice", "test-key", &[7_u8; 64], 1, test_effect())
            .await,
        Err(AdapterError::InvalidRequest)
    ));
    assert_eq!(std::fs::read(&users).expect("create conflict"), original);
    assert!(matches!(
        adapter
            .user_password_rotate("bob", "test-key", &[7_u8; 64], 2, test_effect())
            .await,
        Err(AdapterError::InvalidRequest)
    ));
    assert_eq!(std::fs::read(&users).expect("rotate missing"), original);
    std::fs::remove_dir_all(directory).expect("cleanup");
}

#[tokio::test]
async fn user_create_rejects_authoritative_capacity_before_side_effects() {
    let directory = std::env::temp_dir().join(format!("ocservia-user-capacity-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("directory");
    let users = directory.join("ocpasswd");
    let mut original = Vec::new();
    for index in 0..MAX_MANAGED_RESOURCES {
        original.extend_from_slice(format!("user{index:03}:*:$6$test-hash\n").as_bytes());
    }
    write_user_fixture(&users, &original);

    let openssl_marker = directory.join("openssl-called");
    let ocpasswd_marker = directory.join("ocpasswd-called");
    let openssl = executable(
        "openssl-fixture",
        &format!("touch '{}'; printf password", openssl_marker.display()),
    );
    let openssl_directory = openssl.parent().expect("openssl parent").to_owned();
    let ocpasswd = executable(
        "ocpasswd-fixture",
        &format!("touch '{}'; exit 1", ocpasswd_marker.display()),
    );
    let ocpasswd_directory = ocpasswd.parent().expect("ocpasswd parent").to_owned();
    let effect_database = directory.join("effects.sqlite3");
    let effect_key = directory.join("effects.key");
    let resources = FixedResources::default()
        .with_user_resources(
            openssl,
            users.clone(),
            ocpasswd,
            directory.join("key.pem"),
            String::from("test-key"),
        )
        .expect("resources")
        .with_effect_store(effect_database.clone(), effect_key.clone())
        .expect("effect resources");
    let adapter = Adapter::new(resources, Limits::default());

    assert!(matches!(
        adapter
            .user_create("overflow", "test-key", &[7_u8; 64], 1, test_effect())
            .await,
        Err(AdapterError::CapacityExceeded)
    ));
    assert!(!openssl_marker.exists());
    assert!(!ocpasswd_marker.exists());
    assert!(!effect_database.exists());
    assert!(!effect_key.exists());
    assert_eq!(std::fs::read(&users).expect("unchanged users"), original);
    assert_eq!(
        adapter.user_list().await.expect("user list").users.len(),
        384
    );

    std::fs::remove_dir_all(directory).expect("cleanup directory");
    std::fs::remove_dir_all(openssl_directory).expect("cleanup openssl");
    std::fs::remove_dir_all(ocpasswd_directory).expect("cleanup ocpasswd");
}

#[tokio::test]
async fn oversized_password_staging_is_rejected_before_effect_prepare() {
    let directory =
        std::env::temp_dir().join(format!("ocservia-staging-capacity-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("directory");
    let users = directory.join("ocpasswd");
    let mut original = Vec::new();
    for index in 0..(MAX_MANAGED_RESOURCES - 1) {
        original.extend_from_slice(format!("user{index:03}:*:$6$test-hash\n").as_bytes());
    }
    write_user_fixture(&users, &original);
    let effect_database = directory.join("effects.sqlite3");
    let effect_key = directory.join("effects.key");
    let resources = FixedResources::default()
        .with_user_resources(
            PathBuf::from("/bin/false"),
            users.clone(),
            PathBuf::from("/bin/false"),
            directory.join("key.pem"),
            String::from("test-key"),
        )
        .expect("resources")
        .with_effect_store(effect_database.clone(), effect_key.clone())
        .expect("effect resources");
    let adapter = Adapter::new(resources, Limits::default());
    let mut staging = adapter
        .stage_user_file(&original)
        .await
        .expect("staging file");
    let mut oversized = Vec::new();
    for index in 0..=MAX_MANAGED_RESOURCES {
        oversized.extend_from_slice(format!("staged{index:03}:*:$6$test-hash\n").as_bytes());
    }
    tokio::fs::write(staging.path(), oversized)
        .await
        .expect("oversized staging");

    let result = adapter
        .commit_desired_staging(
            &mut staging,
            &original,
            "user_create",
            "overflow",
            1,
            test_effect(),
        )
        .await;
    assert!(
        matches!(result, Err(AdapterError::OutputLimit)),
        "unexpected result: {result:?}"
    );
    assert!(!effect_database.exists());
    assert!(!effect_key.exists());
    assert_eq!(std::fs::read(&users).expect("unchanged users"), original);
    assert_eq!(
        adapter.user_list().await.expect("user list").users.len(),
        383
    );

    std::fs::remove_dir_all(directory).expect("cleanup directory");
}

#[test]
fn effect_store_supports_ten_thousand_historical_records() {
    let directory = std::env::temp_dir().join(format!("ocservia-effects-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("directory");
    let resources = FixedResources::default()
        .with_effect_store(
            directory.join("effects.sqlite3"),
            directory.join("effects.key"),
        )
        .expect("effect resources");
    let mut store = EffectStore::open_for_mutation(&resources).expect("open effect store");
    let now = effect_now().expect("clock");
    let transaction = store
        .connection
        .transaction_with_behavior(TransactionBehavior::Immediate)
        .expect("scale transaction");
    for index in 0..10_000_u64 {
        let resource_key = format!("user{index:05}");
        let before_hmac = authenticate_file(&store.hmac_key, b"before");
        let after_hmac = authenticate_file(&store.hmac_key, b"after");
        let identity = validate_effect_context("user_create", &resource_key, 1, test_effect(), now)
            .expect("scale identity");
        let record_hmac = authenticate_record(
            &store.hmac_key,
            "user_create",
            &resource_key,
            1,
            identity,
            "applied",
            &before_hmac,
            &after_hmac,
        );
        transaction
                .execute(
                    "INSERT INTO desired_effects(mutation_kind,resource_key,revision,command_id,idempotency_key,payload_sha256,expires_at,state,before_hmac,after_hmac,record_hmac,updated_at) VALUES('user_create',?1,1,?2,?3,?4,?5,'applied',?6,?7,?8,?9)",
                    params![resource_key, identity.command_id.as_slice(), identity.idempotency_key.as_slice(), identity.payload_sha256.as_slice(), identity.expires_at, before_hmac.as_slice(), after_hmac.as_slice(), record_hmac.as_slice(), now],
                )
                .expect("scale effect");
    }
    transaction.commit().expect("scale commit");
    assert_eq!(
        store
            .connection
            .query_row("SELECT count(*) FROM desired_effects", [], |row| row
                .get::<_, i64>(0))
            .expect("scale count"),
        10_000
    );
    std::fs::remove_dir_all(directory).expect("cleanup");
}

#[test]
fn user_and_group_snapshot_capacity_is_bounded() {
    let mut users = Vec::new();
    for index in 0..MAX_MANAGED_RESOURCES {
        users.extend_from_slice(
            format!("user{index:03}:group{index:03}:$6$test-hash-{index}\n").as_bytes(),
        );
    }
    assert_eq!(
        parse_user_file(&users).expect("maximum users").users.len(),
        384
    );
    assert_eq!(
        parse_groups_from_user_file(&users)
            .expect("maximum groups")
            .groups
            .len(),
        384
    );
    users.extend_from_slice(b"overflow:overflow:$6$test-hash\n");
    assert!(matches!(
        parse_user_file(&users),
        Err(AdapterError::OutputLimit)
    ));
}

#[test]
fn effect_store_rejects_revision_rollback() {
    let directory = std::env::temp_dir().join(format!("ocservia-effect-order-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("directory");
    let resources = FixedResources::default()
        .with_effect_store(
            directory.join("effects.sqlite3"),
            directory.join("effects.key"),
        )
        .expect("effect resources");
    let mut store = EffectStore::open_for_mutation(&resources).expect("open effect store");
    store
        .prepare(
            "user_password_rotate",
            "alice",
            2,
            test_effect(),
            b"password-one",
            b"password-two",
        )
        .expect("prepare rotate two");
    store
        .mark_applied("user_password_rotate", "alice", 2, test_effect())
        .expect("apply rotate two");
    store
        .prepare(
            "user_password_rotate",
            "alice",
            3,
            test_effect(),
            b"password-two",
            b"password-three",
        )
        .expect("prepare rotate three");
    store
        .mark_applied("user_password_rotate", "alice", 3, test_effect())
        .expect("apply rotate three");
    let observation = store
        .observe(
            "user_password_rotate",
            "alice",
            2,
            test_effect(),
            b"password-three",
        )
        .expect("observe stale rotate");
    assert_eq!(
        observation.state,
        DesiredEffectState::SupersededByNewerRevision as i32
    );
    assert_eq!(observation.observed_revision, 3);
    assert!(matches!(
        store.prepare(
            "user_password_rotate",
            "alice",
            2,
            test_effect(),
            b"password-three",
            b"password-two",
        ),
        Err(AdapterError::InvalidRequest)
    ));
    std::fs::remove_dir_all(directory).expect("cleanup");
}

#[test]
fn authorized_effect_store_prunes_only_authenticated_expired_successes() {
    let directory = std::env::temp_dir().join(format!("ocservia-authorized-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("directory");
    let resources = FixedResources::default()
        .with_effect_store(
            directory.join("effects.sqlite3"),
            directory.join("effects.key"),
        )
        .expect("effect resources");
    let mut store = EffectStore::open_for_mutation(&resources).expect("open effect store");
    let now = effect_now().expect("clock");
    let node_id = *Uuid::now_v7().as_bytes();
    let operation_id = *Uuid::now_v7().as_bytes();
    let commands = [*Uuid::now_v7().as_bytes(), *Uuid::now_v7().as_bytes()];
    let idempotency = [*Uuid::now_v7().as_bytes(), *Uuid::now_v7().as_bytes()];
    let payload_hashes = [Sha256::digest(b"first"), Sha256::digest(b"second")];
    let authorized = |index: usize| AuthorizedEffectIdentity {
        node_id: &node_id,
        command_id: &commands[index],
        operation_id: &operation_id,
        idempotency_key: &idempotency[index],
        semantic_payload_sha256: payload_hashes[index].as_slice(),
        action: "service.reload",
        authorization_revision: 7,
        effect_kind: "service_reload",
        resource_key: "ocserv.service",
        effect_revision: 7,
        expires_at_unix_seconds: now + 60,
        retry_if_absent: false,
    };
    assert_eq!(
        store
            .prepare_authorized(authorized(0))
            .expect("prepare first effect"),
        AuthorizedEffectDecision::Execute
    );
    store
        .complete_authorized(authorized(0), b"result")
        .expect("complete first effect");

    let mut expired = query_authorized_effect(&store.connection, &commands[0])
        .expect("query first effect")
        .expect("first effect");
    expired.expires_at = now - 1;
    expired.record_hmac = authenticate_authorized_record(&store.hmac_key, &expired);
    store
        .connection
        .execute(
            "UPDATE authorized_effects SET expires_at=?2,record_hmac=?3 WHERE command_id=?1",
            params![
                commands[0].as_slice(),
                expired.expires_at,
                expired.record_hmac.as_slice()
            ],
        )
        .expect("expire authenticated effect");

    let expired_identity = AuthorizedEffectIdentity {
        expires_at_unix_seconds: now - 1,
        ..authorized(0)
    };
    assert!(matches!(
        store.prepare_authorized(expired_identity),
        Err(AdapterError::InvalidRequest)
    ));
    assert_eq!(
        store
            .replay_authorized(expired_identity)
            .expect("read expired exact result"),
        Some(b"result".to_vec())
    );

    assert_eq!(
        store
            .prepare_authorized(authorized(1))
            .expect("prepare next effect"),
        AuthorizedEffectDecision::Execute
    );
    assert!(
        query_authorized_effect(&store.connection, &commands[0])
            .expect("query expired effect")
            .is_none()
    );
    assert!(
        query_authorized_effect(&store.connection, &commands[1])
            .expect("query retained effect")
            .is_some()
    );
    drop(store);
    std::fs::remove_dir_all(directory).expect("cleanup");
}

#[test]
fn effect_store_recovers_post_commit_crash_and_rejects_tampering() {
    let directory = std::env::temp_dir().join(format!("ocservia-effect-crash-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("directory");
    let resources = FixedResources::default()
        .with_effect_store(
            directory.join("effects.sqlite3"),
            directory.join("effects.key"),
        )
        .expect("effect resources");
    let mut store = EffectStore::open_for_mutation(&resources).expect("open effect store");
    store
        .prepare(
            "user_password_rotate",
            "alice",
            2,
            test_effect(),
            b"password-one",
            b"password-two",
        )
        .expect("durable prepared record");
    let recovered = store
        .observe(
            "user_password_rotate",
            "alice",
            2,
            EffectIdentity {
                expires_at_unix_seconds: 1,
                ..test_effect()
            },
            b"password-two",
        )
        .expect("recover post-commit crash");
    assert_eq!(recovered.state, DesiredEffectState::AppliedExact as i32);
    assert_eq!(
            store
                .connection
                .query_row(
                    "SELECT state FROM desired_effects WHERE mutation_kind='user_password_rotate' AND resource_key='alice'",
                    [],
                    |row| row.get::<_, String>(0),
                )
                .expect("recovered state"),
            "applied"
        );
    store
            .connection
            .execute(
                "UPDATE desired_effects SET record_hmac=zeroblob(32) WHERE mutation_kind='user_password_rotate' AND resource_key='alice'",
                [],
            )
            .expect("inject tamper");
    assert!(matches!(
        store.observe(
            "user_password_rotate",
            "alice",
            2,
            test_effect(),
            b"password-two",
        ),
        Err(AdapterError::InvalidResource)
    ));
    std::fs::remove_dir_all(directory).expect("cleanup");
}

#[test]
#[allow(clippy::too_many_lines)]
fn effect_store_missing_evidence_fails_closed() {
    let directory = std::env::temp_dir().join(format!("ocservia-effect-loss-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("directory");
    let database = directory.join("effects.sqlite3");
    let key = directory.join("effects.key");
    let snapshot = directory.join("effects.snapshot");
    let resources = FixedResources::default()
        .with_effect_store(database.clone(), key.clone())
        .expect("effect resources");
    let mut store = EffectStore::open_for_mutation(&resources).expect("initialize store");
    let missing = store
        .observe(
            "user_password_rotate",
            "alice",
            2,
            test_effect(),
            b"changed",
        )
        .expect("missing record observation");
    assert_eq!(missing.state, DesiredEffectState::Unknown as i32);
    store
        .prepare(
            "user_password_rotate",
            "alice",
            1,
            test_effect(),
            b"one",
            b"two",
        )
        .expect("prepare old snapshot");
    assert_eq!(
        store
            .observe("user_password_rotate", "alice", 1, test_effect(), b"one",)
            .expect("authenticated before-state observation")
            .state,
        DesiredEffectState::Absent as i32
    );
    assert_eq!(
        store
            .observe(
                "user_password_rotate",
                "alice",
                1,
                test_effect(),
                b"unrelated-change",
            )
            .expect("mismatched file observation")
            .state,
        DesiredEffectState::Unknown as i32
    );
    store
        .mark_applied("user_password_rotate", "alice", 1, test_effect())
        .expect("apply old snapshot");
    drop(store);
    std::fs::copy(&database, &snapshot).expect("snapshot database");

    let mut store = EffectStore::open_existing(&resources).expect("reopen store");
    store
        .prepare(
            "user_password_rotate",
            "alice",
            2,
            test_effect(),
            b"two",
            b"three",
        )
        .expect("prepare current record");
    store
        .mark_applied("user_password_rotate", "alice", 2, test_effect())
        .expect("apply current record");
    store
            .connection
            .execute(
                "DELETE FROM desired_effects WHERE mutation_kind='user_password_rotate' AND resource_key='alice'",
                [],
            )
            .expect("delete one record");
    assert_eq!(
        store
            .observe("user_password_rotate", "alice", 2, test_effect(), b"three",)
            .expect("deleted record observation")
            .state,
        DesiredEffectState::Unknown as i32
    );
    drop(store);

    std::fs::copy(&snapshot, &database).expect("restore old snapshot");
    let mut old = EffectStore::open_existing(&resources).expect("open old snapshot");
    assert_eq!(
        old.observe("user_password_rotate", "alice", 2, test_effect(), b"three",)
            .expect("old snapshot observation")
            .state,
        DesiredEffectState::Unknown as i32
    );
    drop(old);

    std::fs::remove_file(&database).expect("delete database");
    assert!(matches!(
        EffectStore::open_existing(&resources),
        Err(AdapterError::Unavailable)
    ));
    assert!(!database.exists());
    assert!(key.exists());
    std::fs::remove_dir_all(directory).expect("cleanup");
}

#[test]
fn effect_store_resolves_prepared_transition_before_new_mutation() {
    let directory = std::env::temp_dir().join(format!("ocservia-effect-chain-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("directory");
    let resources = FixedResources::default()
        .with_effect_store(
            directory.join("effects.sqlite3"),
            directory.join("effects.key"),
        )
        .expect("effect resources");
    let mut store = EffectStore::open_for_mutation(&resources).expect("initialize store");
    store
        .prepare(
            "user_password_rotate",
            "alice",
            2,
            test_effect(),
            b"file-one",
            b"file-two",
        )
        .expect("prepare alice");
    assert!(matches!(
        store.prepare(
            "user_create",
            "bob",
            1,
            test_effect(),
            b"file-one",
            b"file-bob",
        ),
        Err(AdapterError::Unavailable)
    ));
    drop(store);

    let mut restarted = EffectStore::open_existing(&resources).expect("restart store");
    restarted
        .prepare(
            "user_create",
            "bob",
            1,
            test_effect(),
            b"file-two",
            b"file-three",
        )
        .expect("new mutation promotes prior transition");
    restarted
        .mark_applied("user_create", "bob", 1, test_effect())
        .expect("apply bob");
    assert_eq!(
        restarted
            .observe(
                "user_password_rotate",
                "alice",
                2,
                test_effect(),
                b"file-three",
            )
            .expect("alice remains provably applied")
            .state,
        DesiredEffectState::AppliedExact as i32
    );

    restarted
        .prepare(
            "group_apply",
            "staff",
            3,
            test_effect(),
            b"file-three",
            b"file-four",
        )
        .expect("prepare group transition");
    restarted
        .prepare(
            "user_create",
            "carol",
            1,
            test_effect(),
            b"file-four",
            b"file-five",
        )
        .expect("user mutation promotes group transition");
    assert_eq!(
        restarted
            .observe("group_apply", "staff", 3, test_effect(), b"file-five",)
            .expect("group remains provably applied")
            .state,
        DesiredEffectState::AppliedExact as i32
    );
    drop(restarted);
    std::fs::remove_dir_all(directory).expect("cleanup");
}

#[test]
fn config_effect_store_persists_absent_reconciliation_before_new_revision() {
    let directory = std::env::temp_dir().join(format!("ocservia-config-absent-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("directory");
    let resources = FixedResources::default()
        .with_effect_store(
            directory.join("effects.sqlite3"),
            directory.join("effects.key"),
        )
        .expect("effect resources");
    let commands = [*Uuid::now_v7().as_bytes(), *Uuid::now_v7().as_bytes()];
    let idempotency = [*Uuid::now_v7().as_bytes(), *Uuid::now_v7().as_bytes()];
    let payload_hashes = [Sha256::digest(b"stale"), Sha256::digest(b"next")];
    let effect = |index: usize| EffectIdentity {
        command_id: &commands[index],
        idempotency_key: &idempotency[index],
        semantic_payload_sha256: payload_hashes[index].as_slice(),
        expires_at_unix_seconds: i64::MAX,
    };
    let mut store = EffectStore::open_for_mutation(&resources).expect("initialize store");
    store
        .prepare(
            "config_apply",
            "ocserv.conf",
            7,
            effect(0),
            b"current",
            b"stale-candidate",
        )
        .expect("prepare interrupted config effect");
    drop(store);

    let mut restarted = EffectStore::open_existing(&resources).expect("restart store");
    let observation = restarted
        .observe("config_apply", "ocserv.conf", 7, effect(0), b"current")
        .expect("reconcile confirmed absent effect");
    assert_eq!(observation.state, DesiredEffectState::Absent as i32);
    assert_eq!(
            restarted
                .connection
                .query_row(
                    "SELECT state FROM desired_effects WHERE mutation_kind='config_apply' AND resource_key='ocserv.conf'",
                    [],
                    |row| row.get::<_, String>(0),
                )
                .expect("persisted effect state"),
            "absent"
        );
    assert_eq!(
        restarted
            .prepare(
                "config_apply",
                "ocserv.conf",
                8,
                effect(1),
                b"current",
                b"next-candidate",
            )
            .expect("prepare next revision"),
        PrepareEffect::Proceed
    );
    drop(restarted);
    std::fs::remove_dir_all(directory).expect("cleanup");
}

#[test]
fn config_effect_store_rejects_hash_aba_and_old_authorization_after_restart() {
    let directory = std::env::temp_dir().join(format!("ocservia-config-aba-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("directory");
    let resources = FixedResources::default()
        .with_effect_store(
            directory.join("effects.sqlite3"),
            directory.join("effects.key"),
        )
        .expect("effect resources");
    let commands = [
        *Uuid::now_v7().as_bytes(),
        *Uuid::now_v7().as_bytes(),
        *Uuid::now_v7().as_bytes(),
    ];
    let idempotency = [
        *Uuid::now_v7().as_bytes(),
        *Uuid::now_v7().as_bytes(),
        *Uuid::now_v7().as_bytes(),
    ];
    let hashes = [
        Sha256::digest(b"A1"),
        Sha256::digest(b"B2"),
        Sha256::digest(b"A3"),
    ];
    let effect = |index: usize| EffectIdentity {
        command_id: &commands[index],
        idempotency_key: &idempotency[index],
        semantic_payload_sha256: hashes[index].as_slice(),
        expires_at_unix_seconds: i64::MAX,
    };
    let mut store = EffectStore::open_for_mutation(&resources).expect("initialize store");
    store
        .prepare("config_apply", "ocserv.conf", 1, effect(0), b"X", b"A")
        .expect("prepare A revision 1");
    store
        .mark_applied("config_apply", "ocserv.conf", 1, effect(0))
        .expect("apply A revision 1");
    store
        .prepare("config_apply", "ocserv.conf", 2, effect(1), b"A", b"B")
        .expect("prepare B revision 2");
    store
        .mark_applied("config_apply", "ocserv.conf", 2, effect(1))
        .expect("apply B revision 2");
    store
        .prepare("config_apply", "ocserv.conf", 3, effect(2), b"B", b"A")
        .expect("prepare A revision 3");
    store
        .mark_applied("config_apply", "ocserv.conf", 3, effect(2))
        .expect("apply A revision 3");
    drop(store);

    let mut restarted = EffectStore::open_existing(&resources).expect("restart store");
    let refreshed_effect = EffectIdentity {
        expires_at_unix_seconds: i64::MAX - 1,
        ..effect(2)
    };
    assert_eq!(
        restarted
            .observe("config_apply", "ocserv.conf", 3, refreshed_effect, b"A")
            .expect("reconcile exact effect under refreshed authorization")
            .state,
        DesiredEffectState::AppliedExact as i32
    );
    assert!(matches!(
        restarted.prepare("config_apply", "ocserv.conf", 1, effect(0), b"A", b"A"),
        Err(AdapterError::InvalidRequest)
    ));
    let observation = restarted
        .observe("config_apply", "ocserv.conf", 1, effect(0), b"A")
        .expect("observe old A proof");
    assert_eq!(
        observation.state,
        DesiredEffectState::SupersededByNewerRevision as i32
    );
    assert_eq!(observation.observed_revision, 3);
    drop(restarted);
    std::fs::remove_dir_all(directory).expect("cleanup");
}

#[test]
fn effect_store_schema_migration_preserves_config_revision_fence() {
    let directory = std::env::temp_dir().join(format!("ocservia-effect-schema-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("directory");
    let resources = FixedResources::default()
        .with_effect_store(
            directory.join("effects.sqlite3"),
            directory.join("effects.key"),
        )
        .expect("effect resources");
    let mut store = EffectStore::open_for_mutation(&resources).expect("initialize store");
    store
        .prepare("config_apply", "ocserv.conf", 1, test_effect(), b"X", b"A")
        .expect("prepare legacy config effect");
    store
        .mark_applied("config_apply", "ocserv.conf", 1, test_effect())
        .expect("commit legacy config effect");
    drop(store);

    let legacy = Connection::open(&resources.effect_store).expect("open legacy schema");
    legacy
        .execute_batch(
            "BEGIN IMMEDIATE;
                 ALTER TABLE desired_effects RENAME TO desired_effects_current;
                 CREATE TABLE desired_effects (
                   mutation_kind TEXT NOT NULL,
                   resource_key TEXT NOT NULL,
                   revision INTEGER NOT NULL CHECK(revision > 0),
                   command_id BLOB NOT NULL CHECK(length(command_id)=16),
                   idempotency_key BLOB NOT NULL CHECK(length(idempotency_key)=16),
                   payload_sha256 BLOB NOT NULL CHECK(length(payload_sha256)=32),
                   expires_at INTEGER NOT NULL,
                   state TEXT NOT NULL CHECK(state IN ('prepared','applied')),
                   before_hmac BLOB NOT NULL CHECK(length(before_hmac)=32),
                   after_hmac BLOB NOT NULL CHECK(length(after_hmac)=32),
                   record_hmac BLOB NOT NULL CHECK(length(record_hmac)=32),
                   updated_at INTEGER NOT NULL,
                   PRIMARY KEY(mutation_kind,resource_key)
                 ) STRICT;
                 INSERT INTO desired_effects SELECT * FROM desired_effects_current;
                 DROP TABLE desired_effects_current;
                 COMMIT;",
        )
        .expect("downgrade fixture schema");
    drop(legacy);

    let mut upgraded = EffectStore::open_existing(&resources).expect("migrate effect store");
    assert_eq!(
        upgraded
            .observe("config_apply", "ocserv.conf", 1, test_effect(), b"A")
            .expect("observe preserved effect")
            .state,
        DesiredEffectState::AppliedExact as i32
    );
    upgraded
        .prepare("config_apply", "ocserv.conf", 2, test_effect(), b"A", b"B")
        .expect("prepare next revision after migration");
    upgraded
        .mark_absent("config_apply", "ocserv.conf", 2, test_effect())
        .expect("record rolled-back revision");
    assert_eq!(
        upgraded
            .observe("config_apply", "ocserv.conf", 2, test_effect(), b"A")
            .expect("observe rolled-back revision")
            .state,
        DesiredEffectState::Absent as i32
    );
    drop(upgraded);
    std::fs::remove_dir_all(directory).expect("cleanup");
}

#[cfg(target_os = "linux")]
async fn assert_effect_state(
    adapter: &Adapter,
    mutation_kind: &str,
    resource_key: &str,
    revision: u64,
    expected: bool,
) {
    assert_eq!(
        adapter
            .desired_effect_observe(mutation_kind, resource_key, revision, test_effect())
            .await
            .expect("observe effect state")
            .state
            == DesiredEffectState::AppliedExact as i32,
        expected
    );
}

#[cfg(target_os = "linux")]
#[tokio::test]
#[allow(clippy::too_many_lines)]
#[ignore = "requires native ocpasswd, OpenSSL, and an isolated test directory"]
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
            // This fixture runs a real daemon without systemd. Map only reload
            // and status to its PID; configuration parsing and occtl stay native.
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
            let resources = FixedResources::new(
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
            .expect("config effect store");
            let adapter = Adapter::new(resources, Limits::default());
            let original = std::fs::read(&config).expect("original config");
            let mut invalid = original.clone();
            invalid.extend_from_slice(b"\nauth = \"invalid-auth-backend\"\n");
            // Exercise post-publication parser failure and exact restoration
            // using the full native fixture, outside the legacy directive subset.
            let result = adapter
                .apply_validated_config(
                    &invalid,
                    &Sha256::digest(&invalid),
                    &Sha256::digest(&original),
                    1,
                    test_effect(),
                    false,
                )
                .await
                .expect("native rollback outcome");
            assert!(result.healthy && result.rolled_back && !result.failed_critical);
            assert_eq!(std::fs::read(&config).expect("restored config"), original);
            assert_eq!(
                adapter
                    .desired_effect_observe("config_apply", "ocserv.conf", 1, test_effect())
                    .await
                    .expect("observe native rollback")
                    .state,
                DesiredEffectState::Absent as i32
            );
            assert!(staging_files(&root).is_empty());
        }
        "occtl" => {
            adapter.ocserv_version().await.expect("native version");
            let sessions =
                parse_sessions(&std::fs::read(root.join("sessions.json")).expect("occtl users"))
                    .expect("parse native occtl users");
            assert_eq!(sessions.sessions.len(), 1);
            assert_eq!(sessions.sessions[0].username, "alice");
            assert_eq!(sessions.sessions[0].remote_ip, "127.0.0.1");
            parse_ip_bans(&std::fs::read(root.join("bans.json")).expect("occtl bans"))
                .expect("parse native occtl bans");
            let socket = root.join("occtl.sock");
            let socket = socket.to_str().expect("native socket path");
            adapter
                .execute(
                    &adapter.resources.occtl,
                    &["-s", socket, "terminate", "id", &sessions.sessions[0].id],
                )
                .await
                .expect("terminate native session");
            let mut empty = false;
            for _ in 0..100 {
                let output = adapter
                    .execute(
                        &adapter.resources.occtl,
                        &["-s", socket, "--json", "show", "users"],
                    )
                    .await
                    .expect("native users after terminate");
                empty = parse_sessions(&output.stdout)
                    .expect("empty native users")
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
            let sealed =
                std::fs::read(root.join("sealed-disabled-password.bin")).expect("sealed password");
            let (first, second) = tokio::join!(
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
            first.expect("concurrent bob");
            second.expect("concurrent carol");
            let (first, second) = tokio::join!(
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
            assert_eq!(usize::from(first.is_ok()) + usize::from(second.is_ok()), 1);
            let (locked, rotated) = tokio::join!(
                adapter.user_disable("alice", 6, test_effect()),
                adapter.user_password_rotate(
                    "alice",
                    "native-test-user-password",
                    &sealed,
                    7,
                    test_effect()
                ),
            );
            locked.expect("concurrent disable");
            rotated.expect("concurrent rotation");
            let members = vec!["alice".to_owned(), "bob".to_owned(), "carol".to_owned()];
            let (group, rotated) = tokio::join!(
                adapter.group_apply("staff", &members, 2, test_effect()),
                adapter.user_password_rotate(
                    "alice",
                    "native-test-user-password",
                    &sealed,
                    8,
                    test_effect()
                ),
            );
            group.expect("interleaved group");
            rotated.expect("interleaved password");
            let before = std::fs::read(&users).expect("before replay");
            adapter
                .user_password_rotate(
                    "alice",
                    "native-test-user-password",
                    &sealed,
                    8,
                    test_effect(),
                )
                .await
                .expect("replay rotation");
            assert!(before.eq(&std::fs::read(&users).expect("after replay")));
            let records = parse_secret_user_records(&before).expect("concurrent records");
            assert_eq!(records.len(), 4);
            for record in &records {
                assert!(!record.hash.is_empty());
                assert_eq!(record.hash.starts_with('!'), record.username == "alice");
                assert_eq!(
                    record.groups,
                    if record.username == "dave" {
                        "*"
                    } else {
                        "staff"
                    }
                );
            }
            let metadata = std::fs::metadata(&users).expect("authoritative metadata");
            assert_eq!(
                (
                    metadata.uid(),
                    metadata.gid(),
                    metadata.mode() & 0o777,
                    metadata.nlink()
                ),
                (0, 0, 0o600, 1)
            );
            assert!(staging_files(&root).is_empty());
        }
        "create-user" => {
            let sealed = std::fs::read(root.join("sealed-initial-password.bin"))
                .expect("sealed initial password");
            adapter
                .user_create(
                    "alice",
                    "native-test-user-password",
                    &sealed,
                    1,
                    test_effect(),
                )
                .await
                .expect("create native user");
            assert_effect_state(&adapter, "user_create", "alice", 1, true).await;
            let after_create = std::fs::read(&users).expect("created record");
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
                    .is_err(),
                "rotate must not create a missing user"
            );
            assert_eq!(
                std::fs::read(&users).expect("unchanged create"),
                after_create
            );
            let listed = adapter.user_list().await.expect("list native users");
            assert_eq!(listed.users.len(), 1);
            assert_eq!(listed.users[0].username, "alice");
            assert!(listed.users[0].enabled);
            let (groups, hash) = record();
            assert_eq!(groups, "*");
            assert!(!hash.starts_with('!'));
        }
        "rotate-password" => {
            let sealed = std::fs::read(root.join("sealed-rotated-password.bin"))
                .expect("sealed rotated password");
            let (before_groups, before_hash) = record();
            adapter
                .user_password_rotate(
                    "alice",
                    "native-test-user-password",
                    &sealed,
                    2,
                    test_effect(),
                )
                .await
                .expect("rotate native user password");
            let (after_groups, after_hash) = record();
            assert_eq!(after_groups, before_groups);
            assert_ne!(after_hash, before_hash);
            assert!(!after_hash.starts_with('!'));
            assert_effect_state(&adapter, "user_password_rotate", "alice", 2, true).await;
        }
        "disable" => {
            let before = std::fs::read(&users).expect("before disable");
            let (before_groups, before_hash) = record();
            adapter
                .user_disable("alice", 3, test_effect())
                .await
                .expect("disable native user");
            let (after_groups, after_hash) = record();
            assert_eq!(after_groups, before_groups);
            assert_eq!(after_hash.strip_prefix('!'), Some(before_hash.as_str()));
            assert!(!adapter.user_list().await.expect("list disabled").users[0].enabled);
            let sealed = std::fs::read(root.join("sealed-rotated-password.bin"))
                .expect("sealed rotated password");
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
                    .is_err(),
                "create must not overwrite an existing user"
            );
            assert_ne!(std::fs::read(&users).expect("locked record"), before);
            assert_eq!(record(), (after_groups, after_hash));
            assert_effect_state(&adapter, "user_disable", "alice", 3, true).await;
        }
        "rotate-disabled-password" => {
            let sealed = std::fs::read(root.join("sealed-disabled-password.bin"))
                .expect("sealed disabled password");
            let (before_groups, before_hash) = record();
            assert!(before_hash.starts_with('!'));
            adapter
                .user_password_rotate(
                    "alice",
                    "native-test-user-password",
                    &sealed,
                    4,
                    test_effect(),
                )
                .await
                .expect("rotate disabled native user password");
            let (after_groups, after_hash) = record();
            assert_eq!(after_groups, before_groups);
            assert!(after_hash.starts_with('!'));
            assert_ne!(after_hash, before_hash);
            assert!(
                !adapter
                    .user_list()
                    .await
                    .expect("disabled after rotate")
                    .users[0]
                    .enabled
            );
            assert_effect_state(&adapter, "user_password_rotate", "alice", 4, true).await;
        }
        "enable" => {
            let (before_groups, before_hash) = record();
            adapter
                .user_enable("alice", 5, test_effect())
                .await
                .expect("enable native user");
            let (after_groups, after_hash) = record();
            assert_eq!(after_groups, before_groups);
            assert_eq!(before_hash.strip_prefix('!'), Some(after_hash.as_str()));
            assert!(adapter.user_list().await.expect("enabled user").users[0].enabled);
            assert_effect_state(&adapter, "user_enable", "alice", 5, true).await;
        }
        "group-apply" => {
            let (_, before_hash) = record();
            adapter
                .group_apply("staff", &["alice".to_owned()], 1, test_effect())
                .await
                .expect("apply native group");
            let (after_groups, after_hash) = record();
            assert_eq!(after_groups, "staff");
            assert_eq!(after_hash, before_hash);
            assert_effect_state(&adapter, "group_apply", "staff", 1, true).await;
        }
        _ => panic!("unsupported native scenario"),
    }
}

#[test]
fn status_requires_exact_keys() {
    let status = parse_service_status(b"LoadState=loaded\nActiveState=active\nSubState=running\n")
        .expect("valid status");
    assert_eq!(status.active_state, "active");
    assert!(parse_service_status(b"ActiveState=active\n").is_err());
}

#[tokio::test]
async fn child_timeout_and_output_limit_are_enforced() {
    let timeout_program = executable("slow-ocserv", "sleep 5");
    let timeout_directory = timeout_program.parent().expect("fixture parent").to_owned();
    let resources = FixedResources::new(
        PathBuf::from("/bin/false"),
        timeout_program,
        PathBuf::from("/bin/false"),
        PathBuf::from("/etc/hosts"),
        PathBuf::from("/proc/sys/kernel/random/boot_id"),
    )
    .expect("fixed resources");
    let adapter = Adapter::new(
        resources,
        Limits {
            timeout: Duration::from_millis(500),
            output_bytes: 1024,
        },
    );
    let timeout_result = adapter.ocserv_version().await;
    assert!(
        matches!(timeout_result, Err(AdapterError::DeadlineExceeded)),
        "unexpected child timeout result: {timeout_result:?}"
    );
    std::fs::remove_dir_all(timeout_directory).expect("remove timeout fixture");

    let noisy_program = executable(
        "noisy-ocserv",
        "i=0; while [ $i -lt 2048 ]; do printf xx; i=$((i + 1)); done",
    );
    let noisy_directory = noisy_program.parent().expect("fixture parent").to_owned();
    let resources = FixedResources::new(
        PathBuf::from("/bin/false"),
        noisy_program,
        PathBuf::from("/bin/false"),
        PathBuf::from("/etc/hosts"),
        PathBuf::from("/proc/sys/kernel/random/boot_id"),
    )
    .expect("fixed resources");
    let adapter = Adapter::new(
        resources,
        Limits {
            timeout: Duration::from_secs(30),
            output_bytes: 1024,
        },
    );
    let output_result = adapter.ocserv_version().await;
    assert!(
        matches!(output_result, Err(AdapterError::OutputLimit)),
        "unexpected child output-limit result: {output_result:?}"
    );
    std::fs::remove_dir_all(noisy_directory).expect("remove noisy fixture");
}

#[tokio::test]
async fn child_deadline_covers_blocked_input_and_inherited_output() {
    let adapter = Adapter::new(
        FixedResources::default(),
        Limits {
            timeout: Duration::from_millis(100),
            output_bytes: 1024,
        },
    );
    for (body, input) in [
        ("sleep 5", vec![b'x'; 1024 * 1024]),
        ("sleep 5 & exit 0", Vec::new()),
    ] {
        let program = executable("blocked-io", body);
        let result = tokio::time::timeout(
            Duration::from_secs(2),
            adapter.execute_with_input(&program, &[], &input),
        )
        .await
        .expect("child IO must obey the adapter deadline");
        assert!(matches!(result, Err(AdapterError::DeadlineExceeded)));
        std::fs::remove_dir_all(program.parent().expect("fixture parent"))
            .expect("remove IO fixture");
    }
}

#[tokio::test]
async fn mutations_use_only_fixed_programs_and_arguments() {
    let occtl = executable(
        "occtl",
        "case \"$*\" in 'disconnect id 42'|'terminate id 42'|'unban ip 192.0.2.9') exit 0;; *) exit 9;; esac",
    );
    let directory = occtl.parent().expect("fixture parent").to_owned();
    let systemctl = directory.join("systemctl");
    write_executable(
        &systemctl,
        "#!/bin/sh\n[ \"$*\" = 'reload ocserv.service --no-ask-password' ]\n",
    );
    let boot = directory.join("boot_id");
    let boot_id = Uuid::now_v7().to_string();
    std::fs::write(&boot, &boot_id).expect("write boot fixture");
    let ocserv = directory.join("ocserv");
    let version = directory.join("version");
    std::fs::write(&version, "1.4.2").expect("write version fixture");
    write_executable(
        &ocserv,
        format!(
            "#!/bin/sh\necho \"ocserv $(cat '{}')\" >&2\n",
            version.display()
        ),
    );
    let resources =
        FixedResources::new(systemctl, ocserv, occtl, PathBuf::from("/etc/hosts"), boot)
            .expect("fixed resources");
    let adapter = Adapter::new(resources, Limits::default());
    assert!(adapter.session_disconnect("42", &boot_id).await.is_ok());
    assert!(adapter.session_terminate("42", &boot_id).await.is_ok());
    // Ocserv before 1.4.2 has no `occtl terminate`: reject before any child runs.
    std::fs::write(&version, "1.2.4").expect("write old version fixture");
    assert!(matches!(
        adapter.session_terminate("42", &boot_id).await,
        Err(AdapterError::InvalidRequest)
    ));
    assert!(adapter.ip_ban_remove("192.0.2.9").await.is_ok());
    assert!(adapter.service_reload().await.is_ok());
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
    std::fs::remove_dir_all(directory).expect("remove mutation fixture");
}

#[cfg(target_os = "linux")]
#[tokio::test]
#[ignore = "requires an explicitly prepared native Ocserv test service"]
async fn native_controlled_operations() {
    let disconnect_id =
        std::env::var("OCSERVIA_DISCONNECT_SESSION_ID").expect("disconnect session ID is required");
    let terminate_id =
        std::env::var("OCSERVIA_TERMINATE_SESSION_ID").expect("terminate session ID is required");
    let banned_ip = std::env::var("OCSERVIA_BANNED_IP").expect("banned IP is required");
    let boot_id = std::fs::read_to_string("/proc/sys/kernel/random/boot_id").expect("read boot ID");
    let adapter = Adapter::new(FixedResources::default(), Limits::default());

    adapter
        .session_disconnect(&disconnect_id, boot_id.trim())
        .await
        .expect("disconnect observed session");
    adapter
        .session_terminate(&terminate_id, boot_id.trim())
        .await
        .expect("terminate observed session");
    adapter
        .ip_ban_remove(&banned_ip)
        .await
        .expect("remove observed IP ban");
    adapter
        .service_reload()
        .await
        .expect("reload ocserv.service");
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

#[test]
#[expect(
    clippy::verbose_bit_mask,
    reason = "compare the explicit Unix permission policy with the old predicate"
)]
fn csr_key_permission_predicates_are_equivalent() {
    for high in [0, 0x8000_0000, 0xffff_0000] {
        for low in 0..=0xffff_u32 {
            let mode = std::fs::Permissions::from_mode(high | low).mode();
            assert_eq!(mode.trailing_zeros() >= 6, mode & 0o077 == 0, "{mode:#o}");
        }
    }
}

#[tokio::test]
async fn certificate_csr_keeps_private_key_local() {
    let directory = std::env::temp_dir().join(format!("ocservia-cert-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("certificate directory");
    std::fs::set_permissions(&directory, std::fs::Permissions::from_mode(0o700))
        .expect("certificate directory mode");
    let resources = FixedResources::default()
        .with_certificate_key_dir(directory.clone())
        .expect("certificate resources");
    let adapter = Adapter::new(
        resources,
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
        .expect("generate CSR");
    assert_eq!(result.certificate_id, id.as_bytes());
    assert!(result.csr_der.len() > 64);
    assert_eq!(result.public_key_sha256.len(), 32);
    assert!(!String::from_utf8_lossy(&result.csr_der).contains("PRIVATE KEY"));
    let key = directory.join(format!("{id}.key.pem"));
    let metadata = std::fs::metadata(&key).expect("local private key");
    assert_eq!(metadata.permissions().mode() & 0o777, 0o600);

    let repeated = adapter
        .certificate_csr(
            id.as_bytes(),
            "vpn.example.test",
            &["alt.example.test".to_owned()],
            2048,
        )
        .await
        .expect("repeat CSR with the same private key");
    assert_eq!(repeated.public_key_sha256, result.public_key_sha256);
    for mode in [0o400, 0o600, 0o700] {
        std::fs::set_permissions(&key, std::fs::Permissions::from_mode(mode)).unwrap();
        let reused = adapter
            .certificate_csr(id.as_bytes(), "vpn.example.test", &[], 2048)
            .await
            .expect("owner-only modes retain the existing key");
        assert_eq!(reused.public_key_sha256, result.public_key_sha256);
    }
    std::fs::set_permissions(&key, std::fs::Permissions::from_mode(0o640)).unwrap();
    assert!(matches!(
        adapter
            .certificate_csr(id.as_bytes(), "vpn.example.test", &[], 2048)
            .await,
        Err(AdapterError::InvalidResource)
    ));
    std::fs::remove_dir_all(directory).expect("cleanup certificate directory");
}

#[tokio::test]
#[allow(clippy::too_many_lines)]
async fn certificate_p12_is_encrypted_bounded_and_replayable() {
    use std::io::Write as _;
    use std::process::{Command, Stdio};

    let directory = std::env::temp_dir().join(format!("ocservia-p12-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("certificate directory");
    std::fs::set_permissions(&directory, std::fs::Permissions::from_mode(0o700))
        .expect("certificate directory mode");
    let user_sealing_key = directory.join("user-sealing-private.pem");
    let user_sealing_public = directory.join("user-sealing-public.pem");
    let p12_sealing_key = directory.join("p12-sealing-private.pem");
    let p12_sealing_public = directory.join("p12-sealing-public.pem");
    for (private, public) in [
        (&user_sealing_key, &user_sealing_public),
        (&p12_sealing_key, &p12_sealing_public),
    ] {
        assert!(
            Command::new("/usr/bin/openssl")
                .args([
                    "genpkey",
                    "-algorithm",
                    "RSA",
                    "-pkeyopt",
                    "rsa_keygen_bits:2048",
                    "-out",
                ])
                .arg(private)
                .status()
                .expect("generate sealing key")
                .success()
        );
        std::fs::set_permissions(private, std::fs::Permissions::from_mode(0o600))
            .expect("sealing key mode");
        assert!(
            Command::new("/usr/bin/openssl")
                .args(["pkey", "-in"])
                .arg(private)
                .args(["-pubout", "-out"])
                .arg(public)
                .status()
                .expect("derive sealing public key")
                .success()
        );
    }
    let resources = FixedResources::default()
        .with_user_resources(
            PathBuf::from("/usr/bin/true"),
            directory.join("unused-user-file"),
            PathBuf::from("/usr/bin/openssl"),
            user_sealing_key,
            "fixture-user-key".to_owned(),
        )
        .expect("fixed secret resources")
        .with_p12_resources(p12_sealing_key, "fixture-key".to_owned())
        .expect("fixed P12 resources")
        .with_effect_store(
            directory.join("desired-effects.sqlite3"),
            directory.join("desired-effects.key"),
        )
        .expect("fixed effect store")
        .with_certificate_key_dir(directory.clone())
        .expect("fixed certificate resources");
    let restart_resources = resources.clone();
    let adapter = Adapter::new(
        resources,
        Limits {
            timeout: Duration::from_secs(10),
            output_bytes: DEFAULT_OUTPUT_BYTES,
        },
    );
    let certificate_id = Uuid::now_v7();
    let csr = adapter
        .certificate_csr(certificate_id.as_bytes(), "vpn.example.test", &[], 2048)
        .await
        .expect("generate CSR");
    let csr_path = directory.join("request.der");
    let csr_pem_path = directory.join("request.pem");
    let certificate_path = directory.join("certificate.pem");
    std::fs::write(&csr_path, csr.csr_der).expect("write public CSR fixture");
    let key_path = directory.join(format!("{certificate_id}.key.pem"));
    assert!(
        Command::new("/usr/bin/openssl")
            .args(["req", "-inform", "DER", "-in"])
            .arg(&csr_path)
            .args(["-out"])
            .arg(&csr_pem_path)
            .status()
            .expect("convert fixture CSR")
            .success()
    );
    assert!(
        Command::new("/usr/bin/openssl")
            .args(["x509", "-req", "-in"])
            .arg(&csr_pem_path)
            .args(["-signkey"])
            .arg(&key_path)
            .args(["-days", "1", "-out"])
            .arg(&certificate_path)
            .status()
            .expect("self-sign fixture certificate")
            .success()
    );
    let password = b"unique-random-password-fixture";
    let mut seal = Command::new("/usr/bin/openssl")
        .args(["pkeyutl", "-encrypt", "-pubin", "-inkey"])
        .arg(&p12_sealing_public)
        .args([
            "-pkeyopt",
            "rsa_padding_mode:oaep",
            "-pkeyopt",
            "rsa_oaep_md:sha256",
        ])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .spawn()
        .expect("start sealing fixture");
    seal.stdin
        .take()
        .expect("sealing stdin")
        .write_all(password)
        .expect("seal password input");
    let mut sealed = seal.wait_with_output().expect("seal password");
    assert!(sealed.status.success());
    if let Some(helper) = std::env::var_os("OCSERV_SIGNER_INTEROP_HELPER") {
        assert!(
            Command::new(helper)
                .arg("-test.run=^TestSealInteropHelper$")
                .env("SIGNER_INTEROP_DIR", &directory)
                .status()
                .expect("run Go Signer HTTP sealing helper")
                .success()
        );
        sealed.stdout =
            std::fs::read(directory.join("signer-1.bin")).expect("Go Signer P12 ciphertext");
    }
    let sealed_password = SealedSecretV1 {
        version: SealedSecretVersion::V1 as i32,
        purpose: SealedSecretPurpose::CertificateP12Password as i32,
        key_id: "fixture-key".to_owned(),
        ciphertext: sealed.stdout,
    };
    let mut seal_user = Command::new("/usr/bin/openssl")
        .args(["pkeyutl", "-encrypt", "-pubin", "-inkey"])
        .arg(&user_sealing_public)
        .args([
            "-pkeyopt",
            "rsa_padding_mode:oaep",
            "-pkeyopt",
            "rsa_oaep_md:sha256",
        ])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .spawn()
        .expect("start user-purpose sealing fixture");
    seal_user
        .stdin
        .take()
        .expect("user sealing stdin")
        .write_all(password)
        .expect("user sealing input");
    let mut user_sealed = seal_user.wait_with_output().expect("seal user password");
    assert!(user_sealed.status.success());
    if std::env::var_os("OCSERV_SIGNER_INTEROP_HELPER").is_some() {
        user_sealed.stdout =
            std::fs::read(directory.join("signer-0.bin")).expect("Go Signer user ciphertext");
        let output = adapter
            .execute_with_input(
                &adapter.resources.openssl,
                &[
                    "pkeyutl",
                    "-decrypt",
                    "-inkey",
                    adapter.resources.user_secret_key.to_str().unwrap(),
                    "-pkeyopt",
                    "rsa_padding_mode:oaep",
                    "-pkeyopt",
                    "rsa_oaep_md:sha256",
                ],
                &user_sealed.stdout,
            )
            .await
            .expect("real privd adapter user-key decryption");
        assert_eq!(output.stdout, password);
    }
    let chain = std::fs::read(&certificate_path).expect("read certificate chain");
    let artifact_id = Uuid::now_v7();
    let operation_id = Uuid::now_v7();
    let expires_at = effect_now().expect("current time") + 600;
    let artifact_directory = directory.join("artifacts");
    let mut modified_secret = sealed_password.clone();
    modified_secret.ciphertext[0] ^= 1;
    assert!(
        adapter
            .certificate_p12(
                certificate_id.as_bytes(),
                Uuid::now_v7().as_bytes(),
                &chain,
                &modified_secret,
                1,
                Uuid::now_v7().as_bytes(),
                expires_at,
            )
            .await
            .is_err()
    );
    let mut cross_purpose = sealed_password.clone();
    cross_purpose.purpose = SealedSecretPurpose::UserPassword as i32;
    cross_purpose.key_id = "fixture-user-key".to_owned();
    assert!(matches!(
        adapter
            .certificate_p12(
                certificate_id.as_bytes(),
                Uuid::now_v7().as_bytes(),
                &chain,
                &cross_purpose,
                1,
                Uuid::now_v7().as_bytes(),
                expires_at,
            )
            .await,
        Err(AdapterError::InvalidRequest)
    ));
    let user_ciphertext_in_p12 = SealedSecretV1 {
        version: SealedSecretVersion::V1 as i32,
        purpose: SealedSecretPurpose::CertificateP12Password as i32,
        key_id: "fixture-key".to_owned(),
        ciphertext: user_sealed.stdout,
    };
    assert!(
        adapter
            .certificate_p12(
                certificate_id.as_bytes(),
                Uuid::now_v7().as_bytes(),
                &chain,
                &user_ciphertext_in_p12,
                1,
                Uuid::now_v7().as_bytes(),
                expires_at,
            )
            .await
            .is_err(),
        "user-purpose ciphertext must not decrypt on the P12 key"
    );
    assert!(
        adapter
            .user_create(
                "cross-purpose",
                "fixture-user-key",
                &sealed_password.ciphertext,
                1,
                test_effect(),
            )
            .await
            .is_err(),
        "P12-purpose ciphertext must not decrypt on the user key"
    );
    let result = adapter
        .certificate_p12(
            certificate_id.as_bytes(),
            artifact_id.as_bytes(),
            &chain,
            &sealed_password,
            1,
            operation_id.as_bytes(),
            expires_at,
        )
        .await
        .expect("create encrypted P12");
    let artifact = directory
        .join("artifacts")
        .join(format!("{artifact_id}.p12"));
    assert_eq!(
        std::fs::metadata(&artifact_directory)
            .expect("artifact directory metadata")
            .permissions()
            .mode()
            & 0o777,
        0o700
    );
    let bytes = std::fs::read(&artifact).expect("read encrypted P12");
    assert_eq!(
        std::fs::metadata(&artifact)
            .expect("artifact metadata")
            .permissions()
            .mode()
            & 0o777,
        0o600
    );
    assert_eq!(result.artifact_size, bytes.len() as u64);
    assert_eq!(result.artifact_sha256, Sha256::digest(&bytes).to_vec());
    assert!(
        !bytes
            .windows(password.len())
            .any(|window| window == password)
    );
    assert!(
        Command::new("/usr/bin/openssl")
            .args(["pkcs12", "-in"])
            .arg(&artifact)
            .args([
                "-passin",
                &format!("pass:{}", String::from_utf8_lossy(password)),
                "-noout"
            ])
            .status()
            .expect("open encrypted P12")
            .success()
    );
    let replay = adapter
        .certificate_p12(
            certificate_id.as_bytes(),
            artifact_id.as_bytes(),
            &chain,
            &sealed_password,
            1,
            operation_id.as_bytes(),
            expires_at,
        )
        .await
        .expect("replay P12 creation");
    assert_eq!(replay.artifact_sha256, result.artifact_sha256);
    std::fs::write(&artifact, b"truncated").expect("corrupt final P12 fixture");
    let recovery_error = adapter
        .certificate_p12(
            certificate_id.as_bytes(),
            artifact_id.as_bytes(),
            &chain,
            &sealed_password,
            1,
            operation_id.as_bytes(),
            expires_at,
        )
        .await
        .expect_err("registered artifact corruption must fail closed");
    assert!(matches!(recovery_error, AdapterError::InvalidResource));
    assert!(!artifact.exists());
    let interrupted_artifact = Uuid::now_v7();
    let interrupted_operation = Uuid::now_v7();
    let interrupted_chain = artifact_directory.join(format!(".{interrupted_artifact}.chain.pem"));
    let interrupted_output = artifact_directory.join(format!(".{interrupted_artifact}.p12"));
    std::fs::write(&interrupted_chain, b"interrupted chain fixture")
        .expect("interrupted chain file");
    std::fs::write(&interrupted_output, b"interrupted output fixture")
        .expect("interrupted output file");
    let interrupted_result = adapter
        .certificate_p12(
            certificate_id.as_bytes(),
            interrupted_artifact.as_bytes(),
            &chain,
            &sealed_password,
            1,
            interrupted_operation.as_bytes(),
            expires_at,
        )
        .await
        .expect("recover interrupted P12 staging");
    assert!(!interrupted_chain.exists());
    assert!(!interrupted_output.exists());
    let grant_id = Uuid::now_v7();
    let subject = Uuid::now_v7().to_string();
    let grant_expires = effect_now().expect("current time") + 300;
    let lease = ArtifactLeaseIdentity {
        artifact_id: interrupted_artifact.as_bytes(),
        certificate_id: certificate_id.as_bytes(),
        certificate_version: 1,
        operation_id: interrupted_operation.as_bytes(),
        authorized_subject: &subject,
        max_bytes: interrupted_result.artifact_size,
        grant_id: grant_id.as_bytes(),
        expires_at_unix_seconds: grant_expires,
    };
    let (first_attempt, duplicate_attempt) = tokio::join!(
        adapter.artifact_read(lease, 0),
        adapter.artifact_read(lease, 0)
    );
    assert!(first_attempt.is_ok() ^ duplicate_attempt.is_ok());
    let first_chunk = first_attempt
        .or(duplicate_attempt)
        .expect("one concurrent fetch owns the signed lease");
    assert!(first_chunk.eof);
    assert_eq!(first_chunk.sha256, interrupted_result.artifact_sha256);

    let restarted = Adapter::new(
        restart_resources.clone(),
        Limits {
            timeout: Duration::from_secs(10),
            output_bytes: DEFAULT_OUTPUT_BYTES,
        },
    );
    let other_grant = Uuid::now_v7();
    let conflicting_lease = ArtifactLeaseIdentity {
        grant_id: other_grant.as_bytes(),
        ..lease
    };
    assert!(matches!(
        restarted.artifact_read(conflicting_lease, 0).await,
        Err(AdapterError::Unavailable)
    ));
    let wrong_version = ArtifactLeaseIdentity {
        certificate_version: 2,
        ..lease
    };
    assert!(matches!(
        restarted.artifact_read(wrong_version, 0).await,
        Err(AdapterError::InvalidRequest)
    ));
    assert!(matches!(
        restarted.artifact_read(lease, 0).await,
        Err(AdapterError::InvalidRequest)
    ));
    assert!(
        !restarted
            .artifact_confirm_consumed(
                lease,
                &interrupted_result.artifact_sha256,
                interrupted_result.artifact_size,
            )
            .await
            .expect("confirmation cannot mutate an active lease")
            .applied
    );
    let released_grant_expiry = effect_now().expect("current time") - 1;
    let mut store =
        EffectStore::open_for_mutation(&restart_resources).expect("open leased artifact store");
    let mut released_record = query_artifact(&store.connection, interrupted_artifact.as_bytes())
        .expect("query leased artifact")
        .expect("leased artifact record");
    released_record.grant_expires_at = Some(released_grant_expiry);
    released_record.record_hmac = authenticate_artifact_record(&store.hmac_key, &released_record);
    store
            .connection
            .execute(
                "UPDATE certificate_artifacts SET grant_expires_at=?2,record_hmac=?3 WHERE artifact_id=?1",
                params![
                    released_record.artifact_id.as_slice(),
                    released_grant_expiry,
                    released_record.record_hmac.as_slice()
                ],
            )
            .expect("expire unconsumed root lease fixture");
    store
        .reconcile_artifacts()
        .expect("release expired unconsumed root lease");
    let released_lease = ArtifactLeaseIdentity {
        expires_at_unix_seconds: released_grant_expiry,
        ..lease
    };
    assert!(
        !restarted
            .artifact_confirm_consumed(
                released_lease,
                &interrupted_result.artifact_sha256,
                interrupted_result.artifact_size,
            )
            .await
            .expect("released exact artifact confirms not consumed")
            .applied
    );
    let recovered_grant = Uuid::now_v7();
    let recovered_lease = ArtifactLeaseIdentity {
        grant_id: recovered_grant.as_bytes(),
        expires_at_unix_seconds: effect_now().expect("current time") + 300,
        ..lease
    };
    let recovered_chunk = restarted
        .artifact_read(recovered_lease, 0)
        .await
        .expect("released artifact can be leased again");
    assert!(recovered_chunk.eof);
    restarted
        .artifact_consume(
            recovered_lease,
            &interrupted_result.artifact_sha256,
            interrupted_result.artifact_size,
        )
        .await
        .expect("consume recovered lease once");
    restarted
        .artifact_consume(
            recovered_lease,
            &interrupted_result.artifact_sha256,
            interrupted_result.artifact_size,
        )
        .await
        .expect("repeat finalize reconciles without a second read or delete");
    let elapsed_grant_expiry = effect_now().expect("current time") - 1;
    let store =
        EffectStore::open_for_mutation(&restart_resources).expect("open consumed artifact store");
    let mut consumed_record = query_artifact(&store.connection, interrupted_artifact.as_bytes())
        .expect("query consumed artifact")
        .expect("consumed artifact record");
    consumed_record.grant_expires_at = Some(elapsed_grant_expiry);
    consumed_record.record_hmac = authenticate_artifact_record(&store.hmac_key, &consumed_record);
    store
            .connection
            .execute(
                "UPDATE certificate_artifacts SET grant_expires_at=?2,record_hmac=?3 WHERE artifact_id=?1",
                params![
                    consumed_record.artifact_id.as_slice(),
					elapsed_grant_expiry,
                    consumed_record.record_hmac.as_slice()
                ],
            )
            .expect("expire consumed grant fixture");
    let expired_lease = ArtifactLeaseIdentity {
        expires_at_unix_seconds: elapsed_grant_expiry,
        ..recovered_lease
    };
    assert!(
        restarted
            .artifact_confirm_consumed(
                expired_lease,
                &interrupted_result.artifact_sha256,
                interrupted_result.artifact_size,
            )
            .await
            .expect("expired exact grant confirms consumed record")
            .applied
    );
    assert!(matches!(
        restarted
            .artifact_consume(
                expired_lease,
                &interrupted_result.artifact_sha256,
                interrupted_result.artifact_size,
            )
            .await,
        Err(AdapterError::InvalidRequest)
    ));
    assert!(matches!(
        restarted.artifact_read(recovered_lease, 0).await,
        Err(AdapterError::InvalidRequest)
    ));
    assert!(
        !artifact_directory
            .join(format!("{interrupted_artifact}.p12"))
            .exists()
    );
    let lookalike = directory.join("artifacts").join("not-a-uuid.p12");
    std::fs::write(&lookalike, b"preserve").expect("write cleanup lookalike");
    let legacy_untracked = Uuid::now_v7();
    let legacy_untracked_path = artifact_directory.join(format!("{legacy_untracked}.p12"));
    std::fs::write(&legacy_untracked_path, b"recent legacy artifact")
        .expect("write recent legacy artifact");
    std::fs::set_permissions(&artifact_directory, std::fs::Permissions::from_mode(0o710))
        .expect("set legacy artifact directory mode");
    assert!(
        Command::new("/usr/bin/touch")
            .args(["-t", "200001010000"])
            .arg(&lookalike)
            .status()
            .expect("age artifact fixtures")
            .success()
    );
    adapter
        .cleanup_stale_certificate_artifacts()
        .await
        .expect("clean stale encrypted artifact");
    assert!(!artifact.exists());
    assert!(!legacy_untracked_path.exists());
    assert_eq!(
        std::fs::symlink_metadata(&artifact_directory)
            .expect("hardened artifact directory")
            .mode()
            & 0o777,
        0o700
    );
    assert!(lookalike.exists());
    let available_artifact = Uuid::now_v7();
    let available_operation = Uuid::now_v7();
    let available_result = adapter
        .certificate_p12(
            certificate_id.as_bytes(),
            available_artifact.as_bytes(),
            &chain,
            &sealed_password,
            1,
            available_operation.as_bytes(),
            expires_at,
        )
        .await
        .expect("create artifact revoked before first fetch");
    let leased_artifact = Uuid::now_v7();
    let leased_operation = Uuid::now_v7();
    let leased_result = adapter
        .certificate_p12(
            certificate_id.as_bytes(),
            leased_artifact.as_bytes(),
            &chain,
            &sealed_password,
            1,
            leased_operation.as_bytes(),
            expires_at,
        )
        .await
        .expect("create artifact revoked during lease");
    let revoked_grant = Uuid::now_v7();
    let revoked_subject = Uuid::now_v7().to_string();
    let revoked_lease = ArtifactLeaseIdentity {
        artifact_id: leased_artifact.as_bytes(),
        certificate_id: certificate_id.as_bytes(),
        certificate_version: 1,
        operation_id: leased_operation.as_bytes(),
        authorized_subject: &revoked_subject,
        max_bytes: leased_result.artifact_size,
        grant_id: revoked_grant.as_bytes(),
        expires_at_unix_seconds: effect_now().expect("current time") + 300,
    };
    adapter
        .artifact_read(revoked_lease, 0)
        .await
        .expect("lease artifact before certificate revoke");
    let revoke_staging = [
        artifact_directory.join(format!(".{available_artifact}.p12")),
        artifact_directory.join(format!(".{available_artifact}.chain.pem")),
    ];
    for path in &revoke_staging {
        std::fs::write(path, b"staging").expect("create revocation staging fixture");
    }
    let prepared_artifact = Uuid::now_v7();
    let prepared_operation = Uuid::now_v7();
    EffectStore::open_for_mutation(&restart_resources)
        .expect("open artifact store")
        .prepare_artifact(
            prepared_artifact.as_bytes(),
            certificate_id.as_bytes(),
            1,
            prepared_operation.as_bytes(),
            expires_at,
        )
        .expect("persist prepared artifact mapping");
    let prepared_staging = [
        artifact_directory.join(format!(".{prepared_artifact}.p12")),
        artifact_directory.join(format!(".{prepared_artifact}.chain.pem")),
    ];
    for path in &prepared_staging {
        std::fs::write(path, b"crash staging").expect("create prepared staging fixture");
    }
    assert!(
        adapter
            .certificate_revoke(certificate_id.as_bytes(), 1)
            .await
            .expect("remove node key")
            .key_removed
    );
    assert!(
        adapter
            .certificate_revoke(certificate_id.as_bytes(), 1)
            .await
            .expect("repeat node key removal")
            .key_removed
    );
    for artifact_id in [available_artifact, leased_artifact] {
        assert!(
            !artifact_directory
                .join(format!("{artifact_id}.p12"))
                .exists(),
            "certificate revoke must delete every derived P12"
        );
    }
    assert!(
        revoke_staging
            .iter()
            .chain(prepared_staging.iter())
            .all(|path| !path.exists())
    );
    let unavailable_grant = Uuid::now_v7();
    let unavailable_lease = ArtifactLeaseIdentity {
        artifact_id: available_artifact.as_bytes(),
        certificate_id: certificate_id.as_bytes(),
        certificate_version: 1,
        operation_id: available_operation.as_bytes(),
        authorized_subject: &revoked_subject,
        max_bytes: available_result.artifact_size,
        grant_id: unavailable_grant.as_bytes(),
        expires_at_unix_seconds: effect_now().expect("current time") + 300,
    };
    assert!(matches!(
        adapter.artifact_read(unavailable_lease, 0).await,
        Err(AdapterError::InvalidRequest)
    ));
    assert!(matches!(
        adapter.artifact_read(revoked_lease, 0).await,
        Err(AdapterError::InvalidRequest)
    ));
    std::fs::remove_dir_all(directory).expect("cleanup certificate directory");
}
