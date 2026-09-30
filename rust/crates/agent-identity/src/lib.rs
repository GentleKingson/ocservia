//! Durable Agent endpoint identity and controller identity pinning.

#![forbid(unsafe_code)]

use std::fs::{File, OpenOptions};
use std::io::{self, Read, Write};
use std::os::unix::fs::{DirBuilderExt, MetadataExt, OpenOptionsExt};
use std::path::Path;

use iroh::{EndpointId, SecretKey};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    EnrollRequest, EnrollmentProofV1, SealedSecretPurpose, SealedSecretVersion,
};
use sha2::{Digest as _, Sha256};
use zeroize::Zeroizing;

const KEY_FILE: &str = "endpoint.key";
const CONTROLLER_FILE: &str = "controller.endpoint";
const ENROLLMENT_PROOF_DOMAIN_V1: &[u8] = b"ocservia/agent-enrollment/v1\0";
pub const ENROLLMENT_PROOF_VERSION_V1: u32 = 1;
pub const ENROLLMENT_PROTOCOL_MAJOR: u32 = 1;
pub const ENROLLMENT_PROTOCOL_MINOR: u32 = 1;

/// A long-lived Agent endpoint key paired with its immutable controller pin.
#[derive(Clone)]
pub struct Identity {
    key: SecretKey,
    controller: EndpointId,
}

impl std::fmt::Debug for Identity {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("Identity")
            .field("endpoint_id", &self.key.public())
            .field("controller", &self.controller)
            .finish()
    }
}

impl Identity {
    /// Loads an already enrolled identity without ever generating missing state.
    ///
    /// # Errors
    /// Refuses absent, unsafe or mismatched identity and Controller material.
    pub fn load_existing(
        directory: &Path,
        controller: EndpointId,
        expected_endpoint: EndpointId,
    ) -> io::Result<Self> {
        validate_existing_directory(directory)?;
        let key = read_key(&mut secure_open(&directory.join(KEY_FILE))?)?;
        let pinned = read_endpoint(&mut secure_open(&directory.join(CONTROLLER_FILE))?)?;
        if key.public() != expected_endpoint || pinned != controller {
            return Err(invalid("enrolled identity or Controller pin mismatch"));
        }
        Ok(Self { key, controller })
    }

    /// Stages the same endpoint identity for an explicitly selected Controller.
    ///
    /// This does not activate the target or alter the source pin. The privileged
    /// lifecycle must switch the complete binding only after enrollment and
    /// approval; a staged identity alone grants no privileged mutation authority.
    /// The caller must serialize lifecycle operations and supply an owner-only
    /// destination parent. Existing destinations and incomplete staging are
    /// refused, never overwritten or repaired by generating a new endpoint key.
    ///
    /// # Errors
    ///
    /// Rejects missing/insecure source state, identity or pin mismatches, an
    /// unchanged Controller, existing staging/destination, or persistence errors.
    pub fn stage_rebind(
        source: &Path,
        destination: &Path,
        expected_endpoint: EndpointId,
        expected_controller: EndpointId,
        target_controller: EndpointId,
    ) -> Result<Self, io::Error> {
        if expected_controller == target_controller || !destination.is_absolute() {
            return Err(invalid(
                "rebind requires a different Controller and absolute destination",
            ));
        }
        // Unlike provision, rebind must never create missing source material.
        validate_existing_directory(source)?;
        let key = read_key(&mut secure_open(&source.join(KEY_FILE))?)?;
        let controller = read_endpoint(&mut secure_open(&source.join(CONTROLLER_FILE))?)?;
        if key.public() != expected_endpoint || controller != expected_controller {
            return Err(io::Error::new(
                io::ErrorKind::PermissionDenied,
                "rebind source identity or Controller does not match",
            ));
        }
        let parent = destination
            .parent()
            .ok_or_else(|| invalid("missing rebind parent"))?;
        validate_existing_directory(parent)?;
        let name = destination
            .file_name()
            .ok_or_else(|| invalid("missing rebind name"))?;
        if path_exists(destination)? {
            return Err(io::Error::new(
                io::ErrorKind::AlreadyExists,
                "rebind destination exists",
            ));
        }
        let mut staging_name = name.to_os_string();
        staging_name.push(".staging");
        let staging = parent.join(&staging_name);
        std::fs::DirBuilder::new().mode(0o700).create(&staging)?;
        let bytes = Zeroizing::new(key.to_bytes());
        create_secret_file(&staging.join(KEY_FILE), bytes.as_ref())?;
        create_secret_file(
            &staging.join(CONTROLLER_FILE),
            hex::encode(target_controller.as_bytes()).as_bytes(),
        )?;
        File::open(&staging)?.sync_all()?;
        let parent_file = File::open(parent)?;
        rustix::fs::renameat_with(
            &parent_file,
            &staging_name,
            &parent_file,
            name,
            rustix::fs::RenameFlags::NOREPLACE,
        )?;
        parent_file.sync_all()?;
        Ok(Self {
            key,
            controller: target_controller,
        })
    }

    /// Creates the endpoint key once and refuses any later controller substitution.
    ///
    /// # Errors
    ///
    /// Returns an error for insecure paths, invalid identity data, or a changed pin.
    pub fn provision(directory: &Path, controller: EndpointId) -> Result<Self, io::Error> {
        ensure_directory(directory)?;
        let key_path = directory.join(KEY_FILE);
        let pin_path = directory.join(CONTROLLER_FILE);
        let key_exists = path_exists(&key_path)?;
        let pin_exists = path_exists(&pin_path)?;
        if key_exists != pin_exists {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "identity directory is partially initialized",
            ));
        }
        let key = match secure_open(&key_path) {
            Ok(mut file) => read_key(&mut file)?,
            Err(error) if error.kind() == io::ErrorKind::NotFound => create_key(&key_path)?,
            Err(error) => return Err(error),
        };
        match secure_open(&pin_path) {
            Ok(mut file) => {
                let pinned = read_endpoint(&mut file)?;
                if pinned != controller {
                    return Err(io::Error::new(
                        io::ErrorKind::PermissionDenied,
                        "controller endpoint substitution refused",
                    ));
                }
            }
            Err(error) if error.kind() == io::ErrorKind::NotFound => {
                create_secret_file(&pin_path, hex::encode(controller.as_bytes()).as_bytes())?;
            }
            Err(error) => return Err(error),
        }
        File::open(directory)?.sync_all()?;
        Ok(Self { key, controller })
    }

    /// Returns the public Agent `EndpointID`.
    #[must_use]
    pub fn endpoint_id(&self) -> EndpointId {
        self.key.public()
    }

    /// Returns the pinned controller `EndpointID`.
    #[must_use]
    pub const fn controller_endpoint_id(&self) -> EndpointId {
        self.controller
    }

    /// Borrows the secret key without exposing it through formatting.
    #[must_use]
    pub const fn secret_key(&self) -> &SecretKey {
        &self.key
    }

    /// Signs one enrollment request with this identity's long-lived Endpoint
    /// `SecretKey` after every advertised claim has reached its final value.
    ///
    /// # Errors
    ///
    /// Returns an error when the request is not bound to this identity or a
    /// canonical claim is invalid.
    pub fn authorize_enrollment(&self, request: &mut EnrollRequest) -> Result<(), io::Error> {
        if request.endpoint_id != self.endpoint_id().as_bytes() {
            return Err(invalid("enrollment endpoint does not match local identity"));
        }
        authorize_enrollment(request, &self.key)
    }
}

/// Signs an enrollment request with the supplied Iroh Endpoint `SecretKey`.
///
/// # Errors
///
/// Returns an error when the request endpoint differs from the supplied key or
/// a canonical claim is invalid.
pub fn authorize_enrollment(
    request: &mut EnrollRequest,
    secret_key: &SecretKey,
) -> Result<(), io::Error> {
    if request.endpoint_id != secret_key.public().as_bytes() {
        return Err(invalid("enrollment endpoint does not match local identity"));
    }
    request.enrollment_protocol_major = ENROLLMENT_PROTOCOL_MAJOR;
    request.enrollment_protocol_minor = ENROLLMENT_PROTOCOL_MINOR;
    request.capabilities.sort();
    request.sealing_keys.sort_by_key(|key| key.purpose);
    request.proof = None;
    let canonical = enrollment_canonical_v1(request)?;
    request.proof = Some(EnrollmentProofV1 {
        version: ENROLLMENT_PROOF_VERSION_V1,
        signature: secret_key.sign(&canonical).to_bytes().to_vec(),
    });
    Ok(())
}

/// Produces the cross-language canonical `EnrollmentProofV1` signing input.
///
/// # Errors
///
/// Returns an error for a missing, malformed, duplicate, or out-of-range
/// canonical claim.
pub fn enrollment_canonical_v1(request: &EnrollRequest) -> Result<Vec<u8>, io::Error> {
    let timestamp = request
        .time
        .as_ref()
        .ok_or_else(|| invalid("enrollment proof timestamp is missing"))?;
    if request.enrollment_protocol_major != ENROLLMENT_PROTOCOL_MAJOR
        || request.enrollment_protocol_minor != ENROLLMENT_PROTOCOL_MINOR
        || request.endpoint_id.len() != 32
        || request.agent_instance_id.len() != 16
        || !(16..=64).contains(&request.nonce.len())
        || !(0..1_000_000_000).contains(&timestamp.nanos)
        || request.capabilities.is_empty()
        || request.capabilities.len() > 128
        || request.sealing_keys.len() != 2
    {
        return Err(invalid("enrollment proof claims are invalid"));
    }
    let mut sealing_keys = request.sealing_keys.clone();
    sealing_keys.sort_by_key(|key| key.purpose);
    if sealing_keys.iter().enumerate().any(|(index, key)| {
        key.version != i32::from(SealedSecretVersion::V1)
            || key.purpose
                != i32::from(if index == 0 {
                    SealedSecretPurpose::UserPassword
                } else {
                    SealedSecretPurpose::CertificateP12Password
                })
            || key.key_id.is_empty()
            || key.key_id.len() > 128
            || key.public_key_sha256.len() != 32
    }) || sealing_keys[0].key_id == sealing_keys[1].key_id
        || sealing_keys[0].public_key_sha256 == sealing_keys[1].public_key_sha256
    {
        return Err(invalid("enrollment sealing keys are invalid"));
    }
    let mut capabilities = request.capabilities.clone();
    capabilities.sort();
    if capabilities.windows(2).any(|pair| pair[0] == pair[1])
        || capabilities
            .iter()
            .any(|value| value.is_empty() || value.len() > 128)
    {
        return Err(invalid("enrollment proof capabilities are invalid"));
    }
    let token_hash = Sha256::digest(request.token.as_bytes());
    let mut encoded = Vec::with_capacity(1024);
    encoded.extend_from_slice(ENROLLMENT_PROOF_DOMAIN_V1);
    write_u32(&mut encoded, ENROLLMENT_PROOF_VERSION_V1);
    write_u32(&mut encoded, request.enrollment_protocol_major);
    write_u32(&mut encoded, request.enrollment_protocol_minor);
    encoded.extend_from_slice(&token_hash);
    encoded.extend_from_slice(&request.endpoint_id);
    for value in [
        &request.agent_version,
        &request.os_release,
        &request.ocserv_version,
        &request.boot_id,
    ] {
        write_bytes(&mut encoded, value.as_bytes())?;
    }
    encoded.extend_from_slice(&request.agent_instance_id);
    write_u32(
        &mut encoded,
        u32::try_from(capabilities.len()).map_err(|_| invalid("too many capabilities"))?,
    );
    for capability in capabilities {
        write_bytes(&mut encoded, capability.as_bytes())?;
    }
    write_u32(
        &mut encoded,
        u32::try_from(sealing_keys.len()).map_err(|_| invalid("too many sealing keys"))?,
    );
    for key in sealing_keys {
        write_u32(
            &mut encoded,
            u32::try_from(key.version).map_err(|_| invalid("sealing key version invalid"))?,
        );
        write_u32(
            &mut encoded,
            u32::try_from(key.purpose).map_err(|_| invalid("sealing key purpose invalid"))?,
        );
        write_bytes(&mut encoded, key.key_id.as_bytes())?;
        write_bytes(&mut encoded, &key.public_key_sha256)?;
    }
    write_bytes(&mut encoded, request.environment.as_bytes())?;
    write_bytes(&mut encoded, &request.nonce)?;
    encoded.extend_from_slice(&timestamp.seconds.to_be_bytes());
    write_u32(
        &mut encoded,
        u32::try_from(timestamp.nanos).map_err(|_| invalid("timestamp nanos are invalid"))?,
    );
    Ok(encoded)
}

fn write_bytes(output: &mut Vec<u8>, value: &[u8]) -> Result<(), io::Error> {
    write_u32(
        output,
        u32::try_from(value.len()).map_err(|_| invalid("enrollment proof field is too large"))?,
    );
    output.extend_from_slice(value);
    Ok(())
}

fn write_u32(output: &mut Vec<u8>, value: u32) {
    output.extend_from_slice(&value.to_be_bytes());
}

fn invalid(message: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message)
}

fn path_exists(path: &Path) -> Result<bool, io::Error> {
    match std::fs::symlink_metadata(path) {
        Ok(_) => Ok(true),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(false),
        Err(error) => Err(error),
    }
}

fn ensure_directory(path: &Path) -> Result<(), io::Error> {
    match std::fs::symlink_metadata(path) {
        Ok(metadata) => {
            if !metadata.is_dir()
                || metadata.file_type().is_symlink()
                || metadata.uid() != rustix::process::geteuid().as_raw()
                || metadata.mode() & 0o077 != 0
            {
                return Err(io::Error::new(
                    io::ErrorKind::PermissionDenied,
                    "identity directory must be owner-only",
                ));
            }
        }
        Err(error) if error.kind() == io::ErrorKind::NotFound => {
            std::fs::DirBuilder::new().mode(0o700).create(path)?;
        }
        Err(error) => return Err(error),
    }
    Ok(())
}

fn validate_existing_directory(path: &Path) -> Result<(), io::Error> {
    let metadata = std::fs::symlink_metadata(path)?;
    if !metadata.is_dir()
        || metadata.file_type().is_symlink()
        || metadata.uid() != rustix::process::geteuid().as_raw()
        || metadata.mode() & 0o077 != 0
    {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "rebind directory must be existing and owner-only",
        ));
    }
    Ok(())
}

fn create_key(path: &Path) -> Result<SecretKey, io::Error> {
    let key = SecretKey::generate();
    let bytes = Zeroizing::new(key.to_bytes());
    create_secret_file(path, bytes.as_ref())?;
    Ok(key)
}

fn create_secret_file(path: &Path, contents: &[u8]) -> Result<(), io::Error> {
    let mut file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(path)?;
    file.write_all(contents)?;
    file.sync_all()?;
    Ok(())
}

fn secure_open(path: &Path) -> Result<File, io::Error> {
    let file = OpenOptions::new()
        .read(true)
        .custom_flags(no_follow())
        .open(path)?;
    let metadata = file.metadata()?;
    if !metadata.is_file()
        || metadata.uid() != rustix::process::geteuid().as_raw()
        || metadata.mode() & 0o077 != 0
        || metadata.nlink() != 1
    {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "identity file must be owner-only",
        ));
    }
    Ok(file)
}

fn read_key(file: &mut File) -> Result<SecretKey, io::Error> {
    let mut bytes = Zeroizing::new(Vec::with_capacity(33));
    file.take(33).read_to_end(&mut bytes)?;
    let raw: [u8; 32] = bytes.as_slice().try_into().map_err(|_| {
        io::Error::new(
            io::ErrorKind::InvalidData,
            "endpoint key must contain 32 bytes",
        )
    })?;
    Ok(SecretKey::from_bytes(&raw))
}

fn read_endpoint(file: &mut File) -> Result<EndpointId, io::Error> {
    let mut text = String::new();
    file.take(65).read_to_string(&mut text)?;
    if text.len() != 64 || text.bytes().any(|byte| byte.is_ascii_uppercase()) {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "controller endpoint pin is invalid",
        ));
    }
    let bytes: [u8; 32] = hex::decode(text)
        .map_err(|_| {
            io::Error::new(
                io::ErrorKind::InvalidData,
                "controller endpoint pin is invalid",
            )
        })?
        .try_into()
        .map_err(|_| {
            io::Error::new(
                io::ErrorKind::InvalidData,
                "controller endpoint pin is invalid",
            )
        })?;
    EndpointId::from_bytes(&bytes).map_err(|_| {
        io::Error::new(
            io::ErrorKind::InvalidData,
            "controller endpoint pin is invalid",
        )
    })
}

#[cfg(target_os = "linux")]
const fn no_follow() -> i32 {
    0x20_000
}
#[cfg(target_os = "macos")]
const fn no_follow() -> i32 {
    0x100
}

#[cfg(test)]
mod tests {
    use super::*;
    use ocservia_contracts::generated::ocserv::platform::agent::v1::SealingKeyDescriptorV1;
    use std::path::PathBuf;

    fn test_dir() -> PathBuf {
        std::env::temp_dir().join(format!("ocservia-agent-identity-{}", std::process::id()))
    }

    #[test]
    fn endpoint_key_is_stable_and_controller_substitution_is_rejected() {
        let directory = test_dir();
        let _ = std::fs::remove_dir_all(&directory);
        let controller = SecretKey::generate().public();
        let first = Identity::provision(&directory, controller).expect("provision identity");
        let second = Identity::provision(&directory, controller).expect("reload identity");
        assert_eq!(first.endpoint_id(), second.endpoint_id());
        let error = Identity::provision(&directory, SecretKey::generate().public())
            .expect_err("substitution rejected");
        assert_eq!(error.kind(), io::ErrorKind::PermissionDenied);
        std::fs::remove_dir_all(directory).expect("remove identity fixture");
    }

    #[test]
    fn partial_identity_is_rejected_instead_of_rotated() {
        let directory = test_dir().with_extension("partial");
        let _ = std::fs::remove_dir_all(&directory);
        let controller = SecretKey::generate().public();
        Identity::provision(&directory, controller).expect("provision identity");
        std::fs::remove_file(directory.join(KEY_FILE)).expect("remove endpoint key");

        let error = Identity::provision(&directory, controller).expect_err("partial identity");
        assert_eq!(error.kind(), io::ErrorKind::InvalidData);
        assert!(!directory.join(KEY_FILE).exists());
        assert!(directory.join(CONTROLLER_FILE).exists());
        std::fs::remove_dir_all(directory).expect("remove identity fixture");
    }

    #[test]
    fn rebind_staging_preserves_endpoint_and_source_pin() {
        let root = test_dir().with_extension("rebind");
        let _ = std::fs::remove_dir_all(&root);
        ensure_directory(&root).expect("parent");
        let source = root.join("source");
        let target = root.join("target");
        let old = SecretKey::generate().public();
        let new = SecretKey::generate().public();
        let identity = Identity::provision(&source, old).expect("source");
        let staged = Identity::stage_rebind(&source, &target, identity.endpoint_id(), old, new)
            .expect("stage");
        assert_eq!(staged.endpoint_id(), identity.endpoint_id());
        assert_eq!(staged.controller_endpoint_id(), new);
        assert_eq!(
            Identity::provision(&source, old).unwrap().endpoint_id(),
            identity.endpoint_id()
        );
        assert!(Identity::provision(&source, new).is_err());
        assert!(Identity::provision(&target, old).is_err());
        assert_eq!(
            Identity::provision(&target, new).unwrap().endpoint_id(),
            identity.endpoint_id()
        );
        assert!(
            Identity::stage_rebind(&source, &target, identity.endpoint_id(), old, new).is_err()
        );
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn rebind_refuses_wrong_source_and_partial_or_linked_state() {
        let root = test_dir().with_extension("rebind-refusal");
        let _ = std::fs::remove_dir_all(&root);
        ensure_directory(&root).unwrap();
        let source = root.join("source");
        let target = root.join("target");
        let old = SecretKey::generate().public();
        let new = SecretKey::generate().public();
        let identity = Identity::provision(&source, old).unwrap();
        for (endpoint, controller, next) in [
            (new, old, new),
            (identity.endpoint_id(), new, old),
            (identity.endpoint_id(), old, old),
        ] {
            assert!(Identity::stage_rebind(&source, &target, endpoint, controller, next).is_err());
            assert!(!target.exists());
        }
        // A crash before directory publication cannot become an active identity.
        ensure_directory(&root.join("target.staging")).unwrap();
        assert!(
            Identity::stage_rebind(&source, &target, identity.endpoint_id(), old, new).is_err()
        );
        assert!(!target.exists());
        std::fs::remove_dir(root.join("target.staging")).unwrap();
        std::fs::hard_link(source.join(KEY_FILE), root.join("linked-key")).unwrap();
        assert!(
            Identity::stage_rebind(&source, &target, identity.endpoint_id(), old, new).is_err()
        );
        std::fs::remove_file(root.join("linked-key")).unwrap();
        std::fs::remove_file(source.join(KEY_FILE)).unwrap();
        assert!(
            Identity::stage_rebind(&source, &target, identity.endpoint_id(), old, new).is_err()
        );
        assert!(!source.join(KEY_FILE).exists());
        assert!(!target.exists());
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn enrollment_proof_matches_the_go_golden_vector() {
        let mut seed = [0_u8; 32];
        for (index, byte) in seed.iter_mut().enumerate() {
            *byte = u8::try_from(index).expect("seed index");
        }
        let secret_key = SecretKey::from_bytes(&seed);
        let mut request = EnrollRequest {
            token: "enrollment-token-fixture".into(),
            endpoint_id: secret_key.public().as_bytes().to_vec(),
            agent_version: "agent-1.2.3".into(),
            os_release: "FixtureOS 9".into(),
            ocserv_version: "1.3.0".into(),
            boot_id: "boot-fixture".into(),
            agent_instance_id: hex::decode("00112233445566778899aabbccddeeff")
                .expect("instance fixture"),
            capabilities: vec!["ocserv.users.write".into(), "ocserv.status.read".into()],
            environment: "production".into(),
            nonce: hex::decode("ffeeddccbbaa99887766554433221100").expect("nonce fixture"),
            time: Some(prost_types::Timestamp {
                seconds: 1_700_000_000,
                nanos: 123_456_789,
            }),
            enrollment_protocol_major: ENROLLMENT_PROTOCOL_MAJOR,
            enrollment_protocol_minor: ENROLLMENT_PROTOCOL_MINOR,
            proof: None,
            sealing_keys: vec![
                SealingKeyDescriptorV1 {
                    version: SealedSecretVersion::V1 as i32,
                    purpose: SealedSecretPurpose::UserPassword as i32,
                    key_id: "fixture-user-key-v1".into(),
                    public_key_sha256: vec![0x11; 32],
                },
                SealingKeyDescriptorV1 {
                    version: SealedSecretVersion::V1 as i32,
                    purpose: SealedSecretPurpose::CertificateP12Password as i32,
                    key_id: "fixture-p12-key-v1".into(),
                    public_key_sha256: vec![0x22; 32],
                },
            ],
        };
        let fixture = include_str!("../../../../testdata/enrollment-proof-v1.txt");
        let fields: Vec<_> = fixture.split_whitespace().collect();
        assert_eq!(fields.len(), 2);
        assert_eq!(
            hex::encode(enrollment_canonical_v1(&request).expect("canonical enrollment proof")),
            fields[0]
        );
        authorize_enrollment(&mut request, &secret_key).expect("sign enrollment proof");
        assert_eq!(
            hex::encode(&request.proof.expect("proof").signature),
            fields[1]
        );
    }
}
