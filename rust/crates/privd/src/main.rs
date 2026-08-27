use std::env;
use std::io;
use std::os::unix::fs::{MetadataExt, PermissionsExt};
use std::path::{Component, Path, PathBuf};
use std::process::Command;
use std::sync::Arc;
use std::time::Duration;

use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use ocservia_command_authorization::{ControllerCommandKeyring, load_verification_key};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    PrivdAttestationRegistrationV1, PrivdReceiptVersion,
};
use ocservia_ocserv_adapter::{Adapter, FixedResources, Limits};
use ocservia_privd::{ServerConfig, bind_socket, remove_socket, serve};
use ocservia_privd_attestation::{key_id, load_or_create_signing_key, sign_registration};
use ocservia_upgrader::{DEFAULT_OPERATIONS_DIR, UpgradeScheduler, UpgradeTrigger};
use sha2::{Digest, Sha256};
use uuid::Uuid;

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let mut args = env::args().skip(1);
    if args.next().as_deref() == Some("attestation-registration") {
        println!("{}", attestation_registration(args)?);
        return Ok(());
    }
    ocservia_observability::init("ocservia-privd")?;
    let (config, resources, limits) = parse_args()?;
    let adapter = Adapter::new(resources, limits);
    adapter.cleanup_stale_user_staging().await?;
    adapter.cleanup_stale_config_plans().await?;
    adapter.cleanup_stale_config_apply_staging().await?;
    adapter.cleanup_stale_certificate_artifacts().await?;
    let listener = bind_socket(&config)?;
    let cleanup_adapter = adapter.clone();
    let cleanup_task = tokio::spawn(async move {
        let mut interval = tokio::time::interval(Duration::from_mins(5));
        interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        interval.tick().await;
        loop {
            interval.tick().await;
            if let Err(error) = cleanup_adapter.cleanup_stale_certificate_artifacts().await {
                tracing::warn!(error = %error, "certificate artifact cleanup failed");
            }
        }
    });
    tracing::info!(socket = %config.socket.display(), agent_uid = config.agent_uid, "privd serving on AF_UNIX");
    let result = serve(listener, config.clone(), adapter, shutdown()).await;
    cleanup_task.abort();
    let cleanup = remove_socket(&config.socket);
    result?;
    cleanup?;
    Ok(())
}

fn attestation_registration(
    mut args: impl Iterator<Item = String>,
) -> Result<serde_json::Value, io::Error> {
    let key_path = PathBuf::from(required(&mut args, "attestation key path")?);
    let node_id = Uuid::parse_str(&required(&mut args, "node UUIDv7")?)
        .map_err(|_| invalid("node ID invalid"))?;
    if node_id.get_version_num() != 7 {
        return Err(invalid("node ID must be UUIDv7"));
    }
    let nonce = decode_registration_digest(&required(&mut args, "Controller nonce hex")?)?;
    let context =
        decode_registration_digest(&required(&mut args, "credential context SHA-256 hex")?)?;
    if args.next().is_some() {
        return Err(invalid("unexpected registration argument"));
    }
    let owner = rustix::process::geteuid().as_raw();
    let key = load_or_create_signing_key(&key_path, owner)?;
    let registration = sign_registration(
        PrivdAttestationRegistrationV1 {
            version: PrivdReceiptVersion::V1.into(),
            node_id: node_id.as_bytes().to_vec(),
            privd_attestation_key_id: key_id(&key.verifying_key()),
            public_key: key.verifying_key().as_bytes().to_vec(),
            controller_nonce: nonce.to_vec(),
            credential_context_sha256: context.to_vec(),
            signature: Vec::new(),
        },
        &key,
    )
    .map_err(|_| invalid("attestation registration is malformed"))?;
    Ok(serde_json::json!({
        "version": registration.version,
        "key_id": registration.privd_attestation_key_id,
        "public_key": URL_SAFE_NO_PAD.encode(registration.public_key),
        "controller_nonce": URL_SAFE_NO_PAD.encode(registration.controller_nonce),
        "credential_context_sha256": URL_SAFE_NO_PAD.encode(registration.credential_context_sha256),
        "signature": URL_SAFE_NO_PAD.encode(registration.signature),
    }))
}

fn decode_registration_digest(value: &str) -> io::Result<[u8; 32]> {
    hex::decode(value)
        .ok()
        .and_then(|bytes| bytes.try_into().ok())
        .filter(|_| value.len() == 64 && value.bytes().all(|byte| byte.is_ascii_hexdigit()))
        .ok_or_else(|| invalid("digest must be 32-byte hex"))
}

async fn shutdown() {
    let _ = tokio::signal::ctrl_c().await;
}

fn parse_args() -> Result<(ServerConfig, FixedResources, Limits), io::Error> {
    parse_args_from(env::args().skip(1))
}

#[allow(clippy::too_many_lines)]
fn parse_args_from(
    mut args: impl Iterator<Item = String>,
) -> Result<(ServerConfig, FixedResources, Limits), io::Error> {
    let mut socket = PathBuf::from("/run/ocserv-platform/privd.sock");
    let mut agent_uid = None;
    let mut node_id = None;
    let mut command_key_files = Vec::new();
    let mut attestation_key_file = PathBuf::from("/var/lib/ocservia-privd/attestation.key");
    let mut user_seal_key_file = None;
    let mut user_seal_key_id = None;
    let mut user_seal_public_key_sha256 = None;
    let mut p12_seal_key_file = None;
    let mut p12_seal_key_id = None;
    let mut p12_seal_public_key_sha256 = None;
    let mut upgrade_operations_dir = PathBuf::from(DEFAULT_OPERATIONS_DIR);
    while let Some(argument) = args.next() {
        match argument.as_str() {
            "--socket" => socket = PathBuf::from(required(&mut args, "--socket")?),
            "--agent-uid" => {
                agent_uid = Some(
                    required(&mut args, "--agent-uid")?
                        .parse()
                        .map_err(|_| invalid("agent UID invalid"))?,
                );
            }
            "--node-id" => {
                let value = required(&mut args, "--node-id")?;
                let parsed = Uuid::parse_str(&value).map_err(|_| invalid("node ID invalid"))?;
                if parsed.get_version_num() != 7 {
                    return Err(invalid("node ID must be UUIDv7"));
                }
                node_id = Some(*parsed.as_bytes());
            }
            "--controller-command-key-file" => {
                if command_key_files.len() == 8 {
                    return Err(invalid(
                        "at most eight --controller-command-key-file values are allowed",
                    ));
                }
                command_key_files.push(PathBuf::from(required(
                    &mut args,
                    "--controller-command-key-file",
                )?));
            }
            "--attestation-key-file" => {
                attestation_key_file =
                    PathBuf::from(required(&mut args, "--attestation-key-file")?);
            }
            "--user-password-seal-key-file" => {
                user_seal_key_file = Some(PathBuf::from(required(
                    &mut args,
                    "--user-password-seal-key-file",
                )?));
            }
            "--user-password-seal-key-id" => {
                user_seal_key_id = Some(required(&mut args, "--user-password-seal-key-id")?);
            }
            "--user-password-seal-public-key-sha256" => {
                user_seal_public_key_sha256 = Some(required(
                    &mut args,
                    "--user-password-seal-public-key-sha256",
                )?);
            }
            "--p12-password-seal-key-file" => {
                p12_seal_key_file = Some(PathBuf::from(required(
                    &mut args,
                    "--p12-password-seal-key-file",
                )?));
            }
            "--p12-password-seal-key-id" => {
                p12_seal_key_id = Some(required(&mut args, "--p12-password-seal-key-id")?);
            }
            "--p12-password-seal-public-key-sha256" => {
                p12_seal_public_key_sha256 = Some(required(
                    &mut args,
                    "--p12-password-seal-public-key-sha256",
                )?);
            }
            "--upgrade-operations-dir" => {
                let value = required(&mut args, "--upgrade-operations-dir")?;
                let path = PathBuf::from(&value);
                if !path.is_absolute()
                    || path.components().any(|component| {
                        matches!(component, Component::CurDir | Component::ParentDir)
                    })
                {
                    return Err(invalid("upgrade operations directory path invalid"));
                }
                upgrade_operations_dir = path;
            }
            _ => return Err(invalid("unknown privd argument")),
        }
    }
    if command_key_files.is_empty() {
        return Err(invalid("--controller-command-key-file is required"));
    }
    let owner = rustix::process::geteuid().as_raw();
    let group = rustix::process::getegid().as_raw();
    let keys = command_key_files
        .iter()
        .map(|path| load_verification_key(path, owner, group))
        .collect::<Result<Vec<_>, _>>()?;
    let command_keys = ControllerCommandKeyring::new(keys)
        .map_err(|_| invalid("Controller command verification keyring invalid"))?;
    let user_seal_key_file =
        user_seal_key_file.ok_or_else(|| invalid("--user-password-seal-key-file is required"))?;
    let p12_seal_key_file =
        p12_seal_key_file.ok_or_else(|| invalid("--p12-password-seal-key-file is required"))?;
    let user_seal_public_key_sha256 = user_seal_public_key_sha256
        .ok_or_else(|| invalid("--user-password-seal-public-key-sha256 is required"))?;
    let p12_seal_public_key_sha256 = p12_seal_public_key_sha256
        .ok_or_else(|| invalid("--p12-password-seal-public-key-sha256 is required"))?;
    let user_public_key =
        validate_private_key_file(&user_seal_key_file, &user_seal_public_key_sha256)?;
    let p12_public_key =
        validate_private_key_file(&p12_seal_key_file, &p12_seal_public_key_sha256)?;
    if user_public_key == p12_public_key {
        return Err(invalid(
            "password sealing keys must use distinct RSA key pairs",
        ));
    }
    let attestation_key = Arc::new(load_or_create_signing_key(&attestation_key_file, owner)?);
    tracing::info!(
        attestation_key_id = %key_id(&attestation_key.verifying_key()),
        "loaded root-owned privd attestation key"
    );
    let resources = FixedResources::default()
        .with_password_sealing_keys(
            user_seal_key_file,
            user_seal_key_id.ok_or_else(|| invalid("--user-password-seal-key-id is required"))?,
            p12_seal_key_file,
            p12_seal_key_id.ok_or_else(|| invalid("--p12-password-seal-key-id is required"))?,
        )
        .map_err(|_| invalid("password sealing key configuration invalid"))?;
    Ok((
        ServerConfig {
            socket,
            agent_uid: agent_uid.ok_or_else(|| invalid("--agent-uid is required"))?,
            node_id: node_id.ok_or_else(|| invalid("--node-id is required"))?,
            command_keys,
            attestation_key,
            upgrades: UpgradeScheduler::new(upgrade_operations_dir, UpgradeTrigger::Systemd),
        },
        resources,
        Limits::default(),
    ))
}

fn validate_private_key_file(
    path: &Path,
    expected_public_key_sha256: &str,
) -> io::Result<[u8; 32]> {
    if !path.is_absolute()
        || path
            .components()
            .any(|component| matches!(component, Component::CurDir | Component::ParentDir))
    {
        return Err(invalid("password sealing key path invalid"));
    }
    let expected_owner = rustix::process::geteuid().as_raw();
    let metadata = std::fs::symlink_metadata(path)?;
    if !metadata.is_file()
        || metadata.file_type().is_symlink()
        || metadata.uid() != expected_owner
        || metadata.nlink() != 1
        || !matches!(metadata.permissions().mode() & 0o777, 0o400 | 0o600)
    {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "password sealing key metadata invalid",
        ));
    }
    let mut current = path.parent();
    while let Some(directory) = current {
        let metadata = std::fs::symlink_metadata(directory)?;
        if !metadata.is_dir()
            || metadata.file_type().is_symlink()
            || (metadata.uid() != expected_owner && metadata.uid() != 0)
            || metadata.permissions().mode() & 0o022 != 0
        {
            return Err(io::Error::new(
                io::ErrorKind::PermissionDenied,
                "password sealing key ancestry invalid",
            ));
        }
        current = directory.parent();
    }
    let bytes = std::fs::read(path)?;
    if bytes.len() < 256
        || bytes.len() > 32 * 1024
        || !bytes.starts_with(b"-----BEGIN ")
        || !bytes
            .windows(b"PRIVATE KEY-----".len())
            .any(|value| value == b"PRIVATE KEY-----")
    {
        return Err(invalid("password sealing private key invalid"));
    }
    let expected = hex::decode(expected_public_key_sha256)
        .ok()
        .and_then(|value| <[u8; 32]>::try_from(value).ok())
        .filter(|_| {
            expected_public_key_sha256.len() == 64
                && expected_public_key_sha256
                    .bytes()
                    .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
        })
        .ok_or_else(|| invalid("password sealing public key fingerprint invalid"))?;
    let output = Command::new("/usr/bin/openssl")
        .args(["rsa", "-in"])
        .arg(path)
        .args(["-pubout", "-outform", "DER"])
        .output()?;
    if !output.status.success() || output.stdout.is_empty() || output.stdout.len() > 32 * 1024 {
        return Err(invalid(
            "password sealing private key is not a valid RSA key",
        ));
    }
    let actual: [u8; 32] = Sha256::digest(output.stdout).into();
    if actual != expected {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "password sealing private key does not match its pinned public fingerprint",
        ));
    }
    Ok(actual)
}

fn required(args: &mut impl Iterator<Item = String>, name: &str) -> Result<String, io::Error> {
    args.next()
        .ok_or_else(|| invalid(&format!("{name} requires a value")))
}

fn invalid(detail: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, detail)
}

#[cfg(test)]
mod tests {
    use std::fs;

    use ed25519_dalek::pkcs8::spki::der::pem::LineEnding;
    use ed25519_dalek::{
        Signature, SigningKey, Verifier as _, VerifyingKey, pkcs8::EncodePublicKey as _,
    };
    use ocservia_privd_attestation::canonical_registration_v1;

    use super::*;

    #[test]
    fn production_startup_requires_controller_verification_key() {
        let node_id = Uuid::now_v7().to_string();
        let failure = parse_args_from(
            [
                "--agent-uid".to_owned(),
                "997".to_owned(),
                "--node-id".to_owned(),
                node_id,
            ]
            .into_iter(),
        )
        .expect_err("privd must not start without a Controller key");
        assert_eq!(failure.kind(), io::ErrorKind::InvalidInput);
        assert!(failure.to_string().contains("key-file is required"));
    }

    #[test]
    fn installed_binary_emits_root_key_registration_proof() {
        let directory = std::env::current_dir()
            .expect("current directory")
            .join(format!(".privd-registration-test-{}", Uuid::now_v7()));
        fs::create_dir(&directory).expect("create key directory");
        fs::set_permissions(&directory, fs::Permissions::from_mode(0o700))
            .expect("secure key directory mode");
        let node_id = Uuid::now_v7();
        let output = attestation_registration(
            [
                directory.join("attestation.key").display().to_string(),
                node_id.to_string(),
                hex::encode([3_u8; 32]),
                hex::encode([5_u8; 32]),
            ]
            .into_iter(),
        )
        .expect("emit registration");
        let decode = |name: &str| {
            URL_SAFE_NO_PAD
                .decode(output[name].as_str().expect("registration string"))
                .expect("base64url registration field")
        };
        let public_key = decode("public_key");
        let registration = PrivdAttestationRegistrationV1 {
            version: i32::try_from(output["version"].as_i64().expect("registration version"))
                .expect("version fits i32"),
            node_id: node_id.as_bytes().to_vec(),
            privd_attestation_key_id: output["key_id"]
                .as_str()
                .expect("registration key ID")
                .to_owned(),
            public_key: public_key.clone(),
            controller_nonce: decode("controller_nonce"),
            credential_context_sha256: decode("credential_context_sha256"),
            signature: decode("signature"),
        };
        let canonical = canonical_registration_v1(&registration).expect("canonical registration");
        let verifying_key =
            VerifyingKey::from_bytes(&public_key.try_into().expect("32-byte public key"))
                .expect("Ed25519 public key");
        verifying_key
            .verify(
                &canonical,
                &Signature::from_slice(&registration.signature).expect("Ed25519 signature"),
            )
            .expect("root registration signature");
        fs::remove_dir_all(directory).expect("remove key fixture");
    }

    #[test]
    fn production_startup_rejects_reused_or_mismatched_sealing_keys() {
        let directory = std::env::current_dir()
            .expect("current directory")
            .join(format!(".privd-sealing-test-{}", Uuid::now_v7()));
        fs::create_dir(&directory).expect("create secure key directory");
        fs::set_permissions(&directory, fs::Permissions::from_mode(0o700))
            .expect("secure key directory mode");
        let command_key = directory.join("controller.pem");
        fs::write(
            &command_key,
            SigningKey::from_bytes(&[7; 32])
                .verifying_key()
                .to_public_key_pem(LineEnding::LF)
                .expect("encode Controller key"),
        )
        .expect("write Controller key");
        fs::set_permissions(&command_key, fs::Permissions::from_mode(0o600))
            .expect("Controller key mode");
        let missing_sealing = parse_args_from(
            [
                "--agent-uid".to_owned(),
                "997".to_owned(),
                "--node-id".to_owned(),
                Uuid::now_v7().to_string(),
                "--controller-command-key-file".to_owned(),
                command_key.display().to_string(),
            ]
            .into_iter(),
        )
        .expect_err("privd must not start without both password sealing keys");
        assert!(
            missing_sealing
                .to_string()
                .contains("user-password-seal-key-file is required")
        );
        let user_key = directory.join("user.pem");
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
                .arg(&user_key)
                .status()
                .expect("generate RSA key")
                .success()
        );
        fs::set_permissions(&user_key, fs::Permissions::from_mode(0o600)).expect("RSA key mode");
        let p12_key = directory.join("p12.pem");
        fs::copy(&user_key, &p12_key).expect("copy reused RSA key");
        fs::set_permissions(&p12_key, fs::Permissions::from_mode(0o600))
            .expect("copied RSA key mode");
        let public = Command::new("/usr/bin/openssl")
            .args(["rsa", "-in"])
            .arg(&user_key)
            .args(["-pubout", "-outform", "DER"])
            .output()
            .expect("derive RSA public key");
        assert!(public.status.success());
        let fingerprint = hex::encode(Sha256::digest(public.stdout));
        let arguments = |p12_fingerprint: &str| {
            vec![
                "--agent-uid".to_owned(),
                "997".to_owned(),
                "--node-id".to_owned(),
                Uuid::now_v7().to_string(),
                "--controller-command-key-file".to_owned(),
                command_key.display().to_string(),
                "--user-password-seal-key-file".to_owned(),
                user_key.display().to_string(),
                "--user-password-seal-key-id".to_owned(),
                "user-v1".to_owned(),
                "--user-password-seal-public-key-sha256".to_owned(),
                fingerprint.clone(),
                "--p12-password-seal-key-file".to_owned(),
                p12_key.display().to_string(),
                "--p12-password-seal-key-id".to_owned(),
                "p12-v1".to_owned(),
                "--p12-password-seal-public-key-sha256".to_owned(),
                p12_fingerprint.to_owned(),
            ]
        };
        let mismatch = parse_args_from(arguments(&"00".repeat(32)).into_iter())
            .expect_err("mismatched sealing key fingerprint must fail startup");
        assert!(mismatch.to_string().contains("does not match"));
        let reused = parse_args_from(arguments(&fingerprint).into_iter())
            .expect_err("reused RSA pair must fail startup");
        assert!(reused.to_string().contains("distinct RSA key pairs"));
        fs::remove_dir_all(directory).expect("remove key fixtures");
    }
}
