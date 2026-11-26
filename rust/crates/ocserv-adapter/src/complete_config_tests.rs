use super::*;
use crate::{FixedResources, Limits};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CompleteConfigDirective, NodeLocalTlsReference,
};
use ocservia_test_support::PrivateTempDir;
use std::os::unix::fs::PermissionsExt;
use std::path::PathBuf;

struct Fixture {
    directory: PrivateTempDir,
    adapter: Adapter,
    plan: CompleteConfigPlan,
    original: Vec<u8>,
}

fn write(path: &Path, bytes: impl AsRef<[u8]>, mode: u32) {
    if path.exists() {
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600)).expect("writable");
    }
    if mode & 0o111 != 0 {
        super::super::tests::write_executable(path, bytes);
    } else {
        std::fs::write(path, bytes).expect("write");
    }
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(mode)).expect("chmod");
}

fn openssl(args: &[&str]) -> Vec<u8> {
    let result = std::process::Command::new("openssl")
        .args(args)
        .output()
        .expect("openssl");
    assert!(
        result.status.success(),
        "{}",
        String::from_utf8_lossy(&result.stderr)
    );
    result.stdout
}

impl Fixture {
    #[allow(clippy::too_many_lines)]
    fn new() -> Self {
        let directory = PrivateTempDir::new("cc");
        let tls = directory.create_subdir_all("tls");
        let node = Uuid::now_v7();
        let reference = Uuid::now_v7();
        let bundle = directory
            .create_subdir_all(PathBuf::from("tls").join(reference.to_string()).join("v1"));
        write(&tls.join(".lock"), [], 0o600);
        let (cert, key) = (
            bundle.join("server-cert.pem"),
            bundle.join("server-key.pem"),
        );
        openssl(&[
            "req",
            "-x509",
            "-newkey",
            "ec",
            "-pkeyopt",
            "ec_paramgen_curve:P-256",
            "-nodes",
            "-days",
            "1",
            "-subj",
            "/CN=complete-config.test",
            "-addext",
            "basicConstraints=critical,CA:FALSE",
            "-addext",
            "extendedKeyUsage=serverAuth",
            "-keyout",
            key.to_str().expect("key"),
            "-out",
            cert.to_str().expect("cert"),
        ]);
        let certificate_sha256 = Sha256::digest(openssl(&[
            "x509",
            "-in",
            cert.to_str().expect("c"),
            "-outform",
            "DER",
        ]))
        .to_vec();
        let spki_sha256 = Sha256::digest(openssl(&[
            "pkey",
            "-in",
            key.to_str().expect("k"),
            "-pubout",
            "-outform",
            "DER",
        ]))
        .to_vec();
        for path in [&cert, &key] {
            std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o400)).expect("400");
        }
        write(&bundle.join("manifest.json"), serde_json::to_vec(&serde_json::json!({
            "node_id": node.to_string(), "secret_ref_id": reference.to_string(), "version": "v1",
            "certificate_sha256": hex::encode(&certificate_sha256), "spki_sha256": hex::encode(&spki_sha256), "ca_sha256": ""
        })).expect("manifest"), 0o400);
        let binding = NodeLocalTlsReference {
            secret_ref_id: reference.as_bytes().to_vec(),
            version: "v1".to_owned(),
            certificate_sha256,
            spki_sha256,
            ca_sha256: vec![],
        };
        let mut candidate = CompleteConfigCandidate {
            node_id: node.as_bytes().to_vec(),
            expected_revision: 0,
            directives: vec![],
        };
        for (name, value) in [
            ("auth", "plain[passwd=/etc/ocserv/ocpasswd]"),
            ("cookie-timeout", "300"),
            ("device", "vpns"),
            ("dns", "1.1.1.1"),
            ("ipv4-network", "192.0.2.0/24"),
            ("max-clients", "64"),
            ("max-same-clients", "2"),
            ("socket-file", "/run/ocserv.socket"),
            ("tcp-port", "443"),
            ("udp-port", "0"),
        ] {
            candidate.directives.push(CompleteConfigDirective {
                name: name.to_owned(),
                value: Some(Value::Literal(value.to_owned())),
            });
        }
        for name in ["server-cert", "server-key"] {
            candidate.directives.push(CompleteConfigDirective {
                name: name.to_owned(),
                value: Some(Value::Tls(binding.clone())),
            });
        }
        candidate.directives.sort_by(|a, b| a.name.cmp(&b.name));
        let plan = CompleteConfigPlan {
            candidate_hash: Sha256::digest(
                config_profile::canonical(&candidate).expect("canonical"),
            )
            .to_vec(),
            candidate: Some(candidate),
        };
        let parser = directory.join("ocserv");
        write(&parser, b"#!/bin/sh\n[ \"$1\" = -t ] && [ \"$2\" = -c ] || exit 1\ncase \"$3\" in *.ocservia-*) grep -q 'server-key = /' \"$3\" && grep -q 'device = vpns' \"$3\" ;; *) exit 0 ;; esac\n", 0o700);
        let systemctl = directory.join("systemctl");
        write(&systemctl, b"#!/bin/sh\nif [ \"$1\" = show ]; then printf 'LoadState=loaded\\nActiveState=active\\nSubState=running\\n'; fi\n", 0o700);
        let occtl = directory.join("occtl");
        write(&occtl, b"#!/bin/sh\nprintf '[]'\n", 0o700);
        let config = directory.join("ocserv.conf");
        let original = format!(
            "# old config\nauth = \"plain[passwd=/etc/ocserv/ocpasswd]\"\ntcp-port = 443\nudp-port = 0\nrun-as-user = ocservia-vpn\nrun-as-group = ocservia-vpn\nsocket-file = /run/ocserv.socket\nserver-cert = {}\nserver-key = {}\nmax-clients = 32\n",
            cert.display(), key.display()
        ).into_bytes();
        write(&config, &original, 0o600);
        let mut resources = FixedResources::new(
            systemctl,
            parser,
            occtl,
            config,
            PathBuf::from("/proc/sys/kernel/random/boot_id"),
        )
        .expect("resources")
        .with_effect_store(
            directory.join("effects.sqlite3"),
            directory.join("effects.key"),
        )
        .expect("effects");
        resources.config_tls_root = tls;
        Self {
            directory,
            adapter: Adapter::new(resources, Limits::default()),
            plan,
            original,
        }
    }

    async fn apply(&self) -> CompleteConfigApply {
        let result = self
            .adapter
            .complete_config_plan(&self.plan)
            .await
            .expect("plan");
        assert!(result.current_unchanged && result.staging_cleaned && result.warnings.is_empty());
        assert!(
            !result
                .diff_redacted
                .contains(self.directory.path().to_str().expect("dir"))
        );
        let mut req = CompleteConfigApply {
            candidate: self.plan.candidate.clone(),
            candidate_hash: self.plan.candidate_hash.clone(),
            expected_current_hash: result.current_hash,
            materialized_hash: result.materialized_hash,
            desired_revision: 1,
            plan_id: Uuid::now_v7().as_bytes().to_vec(),
            plan_expires_at: None,
        };
        req.plan_expires_at.get_or_insert_default().seconds = i64::MAX;
        req
    }
}

#[tokio::test]
async fn complete_plan_apply_and_durable_replay() {
    let fixture = Fixture::new();
    let apply = fixture.apply().await;
    let outcome = fixture
        .adapter
        .complete_config_apply(&apply, super::super::tests::test_effect())
        .await
        .expect("apply");
    assert!(outcome.healthy && !outcome.rolled_back && !outcome.failed_critical);
    assert_eq!(
        (outcome.candidate_hash, outcome.observed_hash),
        (
            apply.candidate_hash.clone(),
            apply.materialized_hash.clone()
        )
    );
    assert_eq!(
        std::fs::metadata(&fixture.adapter.resources.config)
            .expect("cfg")
            .mode()
            & 0o7777,
        0o600
    );
    let restarted = Adapter::new(fixture.adapter.resources.clone(), Limits::default());
    let replay = restarted
        .complete_config_apply(&apply, super::super::tests::test_effect())
        .await
        .expect("replay");
    assert_eq!(replay.observed_hash, apply.materialized_hash);
}

#[tokio::test]
async fn complete_apply_rejects_binding_change_and_rolls_back_on_reload_failure() {
    let fixture = Fixture::new();
    let apply = fixture.apply().await;
    let mut bad_binding = apply.clone();
    bad_binding.expected_current_hash = Sha256::digest(b"stale").to_vec();
    assert!(
        fixture
            .adapter
            .complete_config_apply(&bad_binding, super::super::tests::test_effect())
            .await
            .is_err()
    );

    write(
        &fixture.adapter.resources.systemctl,
        format!(
            "#!/bin/sh\nif [ \"$1\" = reload ] && grep -qx '# generated by ocservia complete-config/v1' '{}' ; then exit 1; fi\nif [ \"$1\" = show ]; then printf 'LoadState=loaded\\nActiveState=active\\nSubState=running\\n'; fi\n",
            fixture.adapter.resources.config.display()
        ),
        0o700,
    );
    let outcome = fixture
        .adapter
        .complete_config_apply(&apply, super::super::tests::test_effect())
        .await
        .expect("rollback");
    assert!(outcome.healthy && outcome.rolled_back && !outcome.failed_critical);
    assert_eq!(
        std::fs::read(&fixture.adapter.resources.config).expect("restored"),
        fixture.original
    );

    let fixture = Fixture::new();
    let apply = fixture.apply().await;
    fixture.adapter.inject_config_apply_fault(4);
    assert!(
        fixture
            .adapter
            .complete_config_apply(&apply, super::super::tests::test_effect())
            .await
            .is_err()
    );
    let restarted = Adapter::new(fixture.adapter.resources.clone(), Limits::default());
    let recovered = restarted
        .complete_config_apply(&apply, super::super::tests::test_effect())
        .await
        .expect("recover");
    assert!(recovered.healthy && !recovered.rolled_back);
}
