#![allow(
    clippy::many_single_char_names,
    clippy::similar_names,
    clippy::too_many_lines
)]

use std::os::unix::fs::PermissionsExt;
use std::process::{Command, Stdio};

use super::*;

const TEST_COMMAND_ID: [u8; 16] = [1, 2, 3, 4, 5, 6, 0x70, 8, 0x80, 10, 11, 12, 13, 14, 15, 16];
const TEST_IDEMPOTENCY_KEY: [u8; 16] = [16, 15, 14, 13, 12, 11, 0x70, 9, 0x80, 7, 6, 5, 4, 3, 2, 1];
const TEST_PAYLOAD_HASH: [u8; 32] = [9; 32];
const TEST_ENDPOINT_ID: [u8; 32] = [7; 32];

pub(super) fn test_effect() -> EffectIdentity<'static> {
    EffectIdentity {
        command_id: &TEST_COMMAND_ID,
        idempotency_key: &TEST_IDEMPOTENCY_KEY,
        semantic_payload_sha256: &TEST_PAYLOAD_HASH,
        expires_at_unix_seconds: i64::MAX,
    }
}

pub(super) fn write_executable(path: &Path, bytes: impl AsRef<[u8]>) {
    let mut w = Command::new("tee")
        .arg(path)
        .stdin(Stdio::piped())
        .stdout(Stdio::null())
        .spawn()
        .expect("w");
    w.stdin
        .take()
        .expect("stdin")
        .write_all(bytes.as_ref())
        .expect("write");
    assert!(w.wait().expect("wait").success());
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700)).expect("0700");
}

struct TempDir(ocservia_test_support::PrivateTempDir);
impl TempDir {
    fn new(prefix: &str) -> Self {
        Self(ocservia_test_support::PrivateTempDir::new(prefix))
    }
    fn path(&self) -> &Path {
        self.0.path()
    }
    fn join(&self, name: impl AsRef<Path>) -> PathBuf {
        self.0.join(name)
    }
    fn effects(&self) -> FixedResources {
        FixedResources::default()
            .with_effect_store(self.join("effects.sqlite3"), self.join("effects.key"))
            .expect("eff")
    }
}

fn openssl(args: &[&str]) {
    assert!(
        Command::new("/usr/bin/openssl")
            .args(args)
            .status()
            .expect("openssl")
            .success()
    );
}

#[test]
fn effect_store_rollback_tamper_and_receipt_sequence() {
    let (_dir, res) = {
        let d = TempDir::new("effects-core");
        let r = d.effects();
        (d, r)
    };
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
    assert_eq!(
        store
            .observe(
                "user_password_rotate",
                "alice",
                2,
                EffectIdentity {
                    expires_at_unix_seconds: 1,
                    ..test_effect()
                },
                b"p2"
            )
            .expect("obs")
            .state,
        DesiredEffectState::AppliedExact as i32
    );
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
    assert_eq!(
        store
            .observe("user_password_rotate", "alice", 2, test_effect(), b"p3")
            .expect("stale")
            .state,
        DesiredEffectState::SupersededByNewerRevision as i32
    );
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

    let (node_id, op_id) = (Uuid::now_v7(), Uuid::now_v7());
    let auth = AuthorizedEffectIdentity {
        node_id: node_id.as_bytes(),
        command_id: &TEST_COMMAND_ID,
        operation_id: op_id.as_bytes(),
        idempotency_key: &TEST_IDEMPOTENCY_KEY,
        semantic_payload_sha256: &TEST_PAYLOAD_HASH,
        action: "service.reload",
        authorization_revision: 1,
        effect_kind: "service",
        resource_key: "ocserv",
        effect_revision: 1,
        expires_at_unix_seconds: i64::MAX,
        retry_if_absent: false,
    };
    let fence = OwnerFenceIdentity {
        node_id: node_id.as_bytes(),
        endpoint_id: &TEST_ENDPOINT_ID,
        owner_epoch: 1,
    };
    assert!(matches!(
        store.prepare_authorized(auth, fence).expect("p"),
        AuthorizedEffectDecision::Execute
    ));
    store.complete_authorized(auth, b"ok").expect("complete");
    assert_eq!(
        store.replay_authorized(auth).expect("rep").as_deref(),
        Some(b"ok".as_slice())
    );

    let adapter = Adapter::new(res.clone(), Limits::default());
    assert_eq!(adapter.next_receipt_sequence().expect("seq 1"), 1);
    let reopened = Adapter::new(res, Limits::default());
    assert_eq!(reopened.next_receipt_sequence().expect("seq 2"), 2);
}

#[tokio::test]
async fn certificate_csr_and_p12_are_local_encrypted_and_replayable() {
    let dir = TempDir::new("p12");
    std::fs::set_permissions(dir.path(), std::fs::Permissions::from_mode(0o700)).expect("0700");
    let (user_priv, user_pub) = (
        dir.join("user-sealing-private.pem"),
        dir.join("user-sealing-public.pem"),
    );
    let (p12_priv, p12_pub) = (
        dir.join("p12-sealing-private.pem"),
        dir.join("p12-sealing-public.pem"),
    );
    for (pr_p, pu_p) in [(&user_priv, &user_pub), (&p12_priv, &p12_pub)] {
        let (pr, pu) = (pr_p.to_str().expect("p"), pu_p.to_str().expect("p"));
        openssl(&[
            "genpkey",
            "-algorithm",
            "RSA",
            "-pkeyopt",
            "rsa_keygen_bits:2048",
            "-out",
            pr,
        ]);
        std::fs::set_permissions(pr_p, std::fs::Permissions::from_mode(0o600)).expect("0600");
        openssl(&["pkey", "-in", pr, "-pubout", "-out", pu]);
    }
    let res = dir
        .effects()
        .with_user_resources(
            PathBuf::from("/usr/bin/true"),
            dir.join("unused"),
            PathBuf::from("/usr/bin/openssl"),
            user_priv,
            "fixture-user-key".into(),
        )
        .expect("u")
        .with_p12_resources(p12_priv, "fixture-key".into())
        .expect("p12")
        .with_certificate_key_dir(dir.path().to_path_buf())
        .expect("cert");
    let adapter = Adapter::new(res, Limits::default());

    let cert_id = Uuid::now_v7();
    let csr = adapter
        .certificate_csr(cert_id.as_bytes(), "vpn.example.test", &[], 2048)
        .await
        .expect("csr");
    assert!(!String::from_utf8_lossy(&csr.csr_der).contains("PRIVATE KEY"));
    let key_path = dir.join(format!("{cert_id}.key.pem"));
    assert_eq!(
        std::fs::metadata(&key_path)
            .expect("key")
            .permissions()
            .mode()
            & 0o777,
        0o600
    );

    let (csr_der, csr_pem, cert_pem) = (
        dir.join("req.der"),
        dir.join("req.pem"),
        dir.join("cert.pem"),
    );
    std::fs::write(&csr_der, csr.csr_der).expect("csr der");
    let (d_s, p_s, k_s, c_s) = (
        csr_der.to_str().unwrap(),
        csr_pem.to_str().unwrap(),
        key_path.to_str().unwrap(),
        cert_pem.to_str().unwrap(),
    );
    openssl(&["req", "-inform", "DER", "-in", d_s, "-out", p_s]);
    openssl(&[
        "x509", "-req", "-in", p_s, "-signkey", k_s, "-days", "1", "-out", c_s,
    ]);

    let pw = b"unique-random-password-fixture";
    let mut p12_ct = if let Some(helper) = std::env::var_os("OCSERV_SIGNER_INTEROP_HELPER") {
        assert!(
            Command::new(helper)
                .arg("-test.run=^TestSealInteropHelper$")
                .env("SIGNER_INTEROP_DIR", dir.path())
                .status()
                .expect("interop")
                .success()
        );
        let user_ct = std::fs::read(dir.join("signer-0.bin")).expect("user ct");
        let dec = adapter
            .execute_with_input(
                &adapter.resources.openssl,
                &[
                    "pkeyutl",
                    "-decrypt",
                    "-inkey",
                    adapter.resources.user_secret_key.to_str().expect("k"),
                    "-pkeyopt",
                    "rsa_padding_mode:oaep",
                    "-pkeyopt",
                    "rsa_oaep_md:sha256",
                ],
                &user_ct,
            )
            .await
            .expect("dec");
        assert_eq!(dec.stdout, pw);
        std::fs::read(dir.join("signer-1.bin")).expect("p12 ct")
    } else {
        let mut c = Command::new("/usr/bin/openssl")
            .args(["pkeyutl", "-encrypt", "-pubin", "-inkey"])
            .arg(&p12_pub)
            .args([
                "-pkeyopt",
                "rsa_padding_mode:oaep",
                "-pkeyopt",
                "rsa_oaep_md:sha256",
            ])
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .spawn()
            .expect("enc");
        c.stdin.take().expect("in").write_all(pw).expect("pw");
        let o = c.wait_with_output().expect("out");
        assert!(o.status.success());
        o.stdout
    };
    let sealed_p12 = SealedSecretV1 {
        version: 1,
        purpose: SealedSecretPurpose::CertificateP12Password as i32,
        key_id: "fixture-key".into(),
        ciphertext: std::mem::take(&mut p12_ct),
    };
    let (chain, exp) = (
        std::fs::read(&cert_pem).expect("chain"),
        effect_now().expect("now") + 600,
    );
    let bad = SealedSecretV1 {
        purpose: SealedSecretPurpose::UserPassword as i32,
        key_id: "fixture-user-key".into(),
        ..sealed_p12.clone()
    };
    assert!(
        adapter
            .certificate_p12(
                cert_id.as_bytes(),
                Uuid::now_v7().as_bytes(),
                &chain,
                &bad,
                1,
                Uuid::now_v7().as_bytes(),
                exp
            )
            .await
            .is_err()
    );

    let (art_id, op_id) = (Uuid::now_v7(), Uuid::now_v7());
    let res = adapter
        .certificate_p12(
            cert_id.as_bytes(),
            art_id.as_bytes(),
            &chain,
            &sealed_p12,
            1,
            op_id.as_bytes(),
            exp,
        )
        .await
        .expect("p12");
    let artifact = dir.join("artifacts").join(format!("{art_id}.p12"));
    openssl(&[
        "pkcs12",
        "-in",
        artifact.to_str().unwrap(),
        "-passin",
        &format!("pass:{}", String::from_utf8_lossy(pw)),
        "-noout",
    ]);
    assert_eq!(
        adapter
            .certificate_p12(
                cert_id.as_bytes(),
                art_id.as_bytes(),
                &chain,
                &sealed_p12,
                1,
                op_id.as_bytes(),
                exp
            )
            .await
            .expect("rep")
            .artifact_sha256,
        res.artifact_sha256
    );

    let grant_id = Uuid::now_v7();
    let lease = ArtifactLeaseIdentity {
        artifact_id: art_id.as_bytes(),
        certificate_id: cert_id.as_bytes(),
        certificate_version: 1,
        operation_id: op_id.as_bytes(),
        authorized_subject: "user:alice",
        max_bytes: res.artifact_size,
        grant_id: grant_id.as_bytes(),
        expires_at_unix_seconds: exp,
        owner_fence: OwnerFenceIdentity {
            node_id: &TEST_COMMAND_ID,
            endpoint_id: &TEST_ENDPOINT_ID,
            owner_epoch: 1,
        },
    };
    assert_eq!(
        adapter.artifact_read(lease).await.expect("read").sha256,
        res.artifact_sha256
    );
    adapter
        .artifact_consume(lease, &res.artifact_sha256, res.artifact_size)
        .await
        .expect("consume");
    assert!(!artifact.exists());
}
