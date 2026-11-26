use super::{
    Adapter, AdapterError, ConfigApplyResult, ConfigPlanResult, EffectIdentity, FileIdentity,
    StagingFile, authoritative_user_owner, lock_config_file, redacted_candidate_diff,
    write_new_synced,
};
use ocservia_command_authorization::VerifiedCommand;
use ocservia_contracts::config_profile;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CompleteConfigApply, CompleteConfigCandidate, CompleteConfigPlan, command_envelope,
    complete_config_directive::Value,
};
use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::collections::BTreeMap;
use std::fs::File;
use std::io::Read;
use std::os::unix::fs::MetadataExt;
use std::path::{Component, Path};
use std::time::{SystemTime, UNIX_EPOCH};
use uuid::Uuid;
use x509_parser::{parse_x509_certificate, pem::parse_x509_pem};

const FIXED_WORKER_CONFIG: &str =
    "run-as-user = ocservia-vpn\nrun-as-group = ocservia-vpn\nuse-occtl = true\n";

// Ocserv keeps these bindings across HUP; changing their file representation
// is not proof that the running daemon adopted them.
const STARTUP_BINDINGS: &[&str] = &[
    "auth",
    "tcp-port",
    "udp-port",
    "run-as-user",
    "run-as-group",
    "socket-file",
    "server-cert",
    "server-key",
    "ca-cert",
];

fn profile_values<'a>(
    bytes: &'a [u8],
    audit_hook: &str,
) -> Result<BTreeMap<&'a str, &'a str>, AdapterError> {
    let text = std::str::from_utf8(bytes).map_err(|_| AdapterError::InvalidResource)?;
    if text.contains(['\r', '\0']) || bytes.len() > super::MAX_CONFIG_PLAN_BYTES {
        return Err(AdapterError::InvalidResource);
    }
    let mut values = BTreeMap::new();
    for line in text
        .lines()
        .map(str::trim)
        .filter(|line| !line.is_empty() && !line.starts_with('#'))
    {
        let (name, value) = line
            .split_once(" = ")
            .ok_or(AdapterError::InvalidResource)?;
        if matches!(name, "connect-script" | "disconnect-script") {
            if value != audit_hook || values.insert(name, value).is_some() {
                return Err(AdapterError::InvalidResource);
            }
            continue;
        }
        if !STARTUP_BINDINGS.contains(&name)
            && ![
                "cookie-timeout",
                "device",
                "dns",
                "ipv4-network",
                "max-clients",
                "max-same-clients",
                "route",
                "use-occtl",
            ]
            .contains(&name)
        {
            return Err(AdapterError::InvalidResource);
        }
        if value.is_empty() || values.insert(name, value).is_some() {
            return Err(AdapterError::InvalidResource);
        }
    }
    Ok(values)
}

pub(super) fn validate_reload_bindings(
    current: &[u8],
    candidate: &[u8],
    audit_hook: &Path,
) -> Result<(), AdapterError> {
    let audit_hook = audit_hook.to_str().ok_or(AdapterError::InvalidResource)?;
    let current = profile_values(current, audit_hook)?;
    let candidate = profile_values(candidate, audit_hook)?;
    if STARTUP_BINDINGS
        .iter()
        .any(|name| current.get(name) != candidate.get(name))
    {
        return Err(AdapterError::InvalidResource);
    }
    Ok(())
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct TlsManifest {
    node_id: String,
    secret_ref_id: String,
    version: String,
    certificate_sha256: String,
    spki_sha256: String,
    ca_sha256: String,
}

struct Materialized {
    bytes: Vec<u8>,
    redacted: String,
    // All immutable versions, including rollback dependencies, share this lock.
    _lock: File,
}

fn safe_file(path: &Path, mode: u32) -> Result<File, AdapterError> {
    let mut parts = path.components();
    if parts.next() != Some(Component::RootDir) {
        return Err(AdapterError::InvalidResource);
    }
    let names = parts
        .map(|part| match part {
            Component::Normal(name) => Ok(name),
            _ => Err(AdapterError::InvalidResource),
        })
        .collect::<Result<Vec<_>, _>>()?;
    let (name, parents) = names.split_last().ok_or(AdapterError::InvalidResource)?;
    let flags = rustix::fs::OFlags::RDONLY
        | rustix::fs::OFlags::DIRECTORY
        | rustix::fs::OFlags::NOFOLLOW
        | rustix::fs::OFlags::CLOEXEC;
    let mut parent = File::from(
        rustix::fs::open("/", flags, rustix::fs::Mode::empty())
            .map_err(|err| AdapterError::Io(err.into()))?,
    );
    let (uid, gid) = authoritative_user_owner();
    for component in parents {
        let metadata = parent.metadata().map_err(AdapterError::Io)?;
        if !metadata.is_dir()
            || (metadata.uid() != 0 && metadata.uid() != uid)
            || metadata.mode() & 0o022 != 0
        {
            return Err(AdapterError::InvalidResource);
        }
        parent = File::from(
            rustix::fs::openat(&parent, *component, flags, rustix::fs::Mode::empty())
                .map_err(|err| AdapterError::Io(err.into()))?,
        );
    }
    let metadata = parent.metadata().map_err(AdapterError::Io)?;
    if !metadata.is_dir() || metadata.uid() != uid || metadata.mode() & 0o022 != 0 {
        return Err(AdapterError::InvalidResource);
    }
    let file = File::from(
        rustix::fs::openat(
            &parent,
            *name,
            rustix::fs::OFlags::RDONLY
                | rustix::fs::OFlags::NOFOLLOW
                | rustix::fs::OFlags::NONBLOCK
                | rustix::fs::OFlags::CLOEXEC,
            rustix::fs::Mode::empty(),
        )
        .map_err(|err| AdapterError::Io(err.into()))?,
    );
    let metadata = file.metadata().map_err(AdapterError::Io)?;
    if !metadata.is_file()
        || metadata.nlink() != 1
        || metadata.uid() != uid
        || metadata.gid() != gid
        || metadata.mode() & 0o7777 != mode
    {
        return Err(AdapterError::InvalidResource);
    }
    Ok(file)
}

fn read_public_file(path: &Path, max: u64) -> Result<Vec<u8>, AdapterError> {
    let file = safe_file(path, 0o400)?;
    let mut bytes = Vec::new();
    file.take(max + 1)
        .read_to_end(&mut bytes)
        .map_err(AdapterError::Io)?;
    if bytes.is_empty() || u64::try_from(bytes.len()).map_err(|_| AdapterError::OutputLimit)? > max
    {
        return Err(AdapterError::OutputLimit);
    }
    Ok(bytes)
}

fn pinned_certificate(
    pem: &[u8],
    expected_digest: &[u8],
    is_ca: bool,
) -> Result<Vec<u8>, AdapterError> {
    let (_, pem) = parse_x509_pem(pem).map_err(|_| AdapterError::InvalidResource)?;
    if pem.label != "CERTIFICATE" || Sha256::digest(&pem.contents).as_slice() != expected_digest {
        return Err(AdapterError::InvalidResource);
    }
    let (remaining, certificate) =
        parse_x509_certificate(&pem.contents).map_err(|_| AdapterError::InvalidResource)?;
    let Some(constraints) = certificate
        .basic_constraints()
        .map_err(|_| AdapterError::InvalidResource)?
    else {
        return Err(AdapterError::InvalidResource);
    };
    if !remaining.is_empty()
        || constraints.value.ca != is_ca
        || (!is_ca && constraints.value.path_len_constraint.is_some())
    {
        return Err(AdapterError::InvalidResource);
    }
    Ok(certificate.public_key().raw.to_vec())
}

impl Adapter {
    #[allow(clippy::too_many_lines)]
    async fn materialize_complete(
        &self,
        candidate: &CompleteConfigCandidate,
    ) -> Result<Materialized, AdapterError> {
        let lock = safe_file(&self.resources.config_tls_root.join(".lock"), 0o600)?;
        rustix::fs::flock(&lock, rustix::fs::FlockOperation::NonBlockingLockShared)
            .map_err(|_| AdapterError::Unavailable)?;
        let node_id =
            Uuid::from_slice(&candidate.node_id).map_err(|_| AdapterError::InvalidRequest)?;
        let reference = candidate
            .directives
            .iter()
            .find_map(|d| match d.value.as_ref() {
                Some(Value::Tls(value)) => Some(value),
                _ => None,
            })
            .ok_or(AdapterError::InvalidRequest)?;
        let ref_id =
            Uuid::from_slice(&reference.secret_ref_id).map_err(|_| AdapterError::InvalidRequest)?;
        let directory = self
            .resources
            .config_tls_root
            .join(ref_id.to_string())
            .join(&reference.version);
        let manifest: TlsManifest = serde_json::from_slice(&read_public_file(
            &directory.join("manifest.json"),
            16 * 1024,
        )?)
        .map_err(|_| AdapterError::InvalidResource)?;
        if manifest.node_id != node_id.to_string()
            || manifest.secret_ref_id != ref_id.to_string()
            || manifest.version != reference.version
            || manifest.certificate_sha256 != hex::encode(&reference.certificate_sha256)
            || manifest.spki_sha256 != hex::encode(&reference.spki_sha256)
            || manifest.ca_sha256 != hex::encode(&reference.ca_sha256)
        {
            return Err(AdapterError::InvalidResource);
        }
        let cert = directory.join("server-cert.pem");
        let key = directory.join("server-key.pem");
        let ca = directory.join("ca-cert.pem");
        let cert_pem = read_public_file(&cert, 256 * 1024)?;
        let key_file = safe_file(&key, 0o400)?;
        if key_file.metadata().map_err(AdapterError::Io)?.len() > 64 * 1024 {
            return Err(AdapterError::InvalidResource);
        }
        let cert_path = cert.to_str().ok_or(AdapterError::InvalidResource)?;
        let key_path = key.to_str().ok_or(AdapterError::InvalidResource)?;
        let public_der = pinned_certificate(&cert_pem, &reference.certificate_sha256, false)?;
        let key_public = self
            .execute(
                &self.resources.openssl,
                &["pkey", "-in", key_path, "-pubout", "-outform", "DER"],
            )
            .await?;
        if public_der != key_public.stdout
            || Sha256::digest(&public_der).as_slice() != reference.spki_sha256
        {
            return Err(AdapterError::InvalidResource);
        }
        self.execute(
            &self.resources.openssl,
            &[
                "verify",
                "-no-CAfile",
                "-no-CApath",
                "-no-CAstore",
                "-trusted",
                cert_path,
                "-partial_chain",
                "-purpose",
                "sslserver",
                cert_path,
            ],
        )
        .await?;
        if !reference.ca_sha256.is_empty() {
            let ca_pem = read_public_file(&ca, 256 * 1024)?;
            pinned_certificate(&ca_pem, &reference.ca_sha256, true)?;
            let ca_path = ca.to_str().ok_or(AdapterError::InvalidResource)?;
            self.execute(
                &self.resources.openssl,
                &[
                    "verify",
                    "-no-CAfile",
                    "-no-CApath",
                    "-no-CAstore",
                    "-trusted",
                    ca_path,
                    "-partial_chain",
                    ca_path,
                ],
            )
            .await?;
        }
        let mut materialized = String::from("# generated by ocservia complete-config/v1\n");
        let mut redacted = materialized.clone();
        for directive in &candidate.directives {
            let (value, safe) = match directive
                .value
                .as_ref()
                .ok_or(AdapterError::InvalidRequest)?
            {
                Value::Literal(value) => (
                    if directive.name == "auth" {
                        format!("\"{value}\"")
                    } else {
                        value.clone()
                    },
                    value.clone(),
                ),
                Value::Tls(_) => {
                    let path = match directive.name.as_str() {
                        "server-cert" => &cert,
                        "server-key" => &key,
                        "ca-cert" => &ca,
                        _ => return Err(AdapterError::InvalidRequest),
                    };
                    (
                        path.to_str()
                            .ok_or(AdapterError::InvalidResource)?
                            .to_owned(),
                        format!(
                            "<secret-ref:{ref_id}:{}:{}>",
                            config_profile::PROVIDER,
                            reference.version
                        ),
                    )
                }
            };
            for (output, text) in [(&mut materialized, value), (&mut redacted, safe)] {
                output.push_str(&directive.name);
                output.push_str(" = ");
                output.push_str(&text);
                output.push('\n');
            }
        }
        materialized.push_str(FIXED_WORKER_CONFIG);
        redacted.push_str(FIXED_WORKER_CONFIG);
        if self
            .resources
            .audit_hook
            .try_exists()
            .map_err(AdapterError::Io)?
        {
            // Only publish a fixed hook from the protected package tree.
            // Keep redacted aligned with the Controller's logical candidate
            // diff; materialized_hash still binds the exact on-disk bytes.
            safe_file(&self.resources.audit_hook, 0o755)?;
            let hook = self
                .resources
                .audit_hook
                .to_str()
                .ok_or(AdapterError::InvalidResource)?;
            let hooks = format!("connect-script = {hook}\ndisconnect-script = {hook}\n");
            materialized.push_str(&hooks);
        }
        Ok(Materialized {
            bytes: materialized.into_bytes(),
            redacted,
            _lock: lock,
        })
    }

    /// Validate all required directives and pinned node-local TLS without publication.
    ///
    /// # Errors
    /// Rejects unsafe resources, stale bindings, TLS or native parser failures.
    pub async fn complete_config_plan_verified(
        &self,
        verified: &VerifiedCommand<'_>,
    ) -> Result<ConfigPlanResult, AdapterError> {
        let Some(command_envelope::Payload::CompleteConfigPlan(request)) =
            verified.envelope().payload.as_ref()
        else {
            return Err(AdapterError::InvalidRequest);
        };
        self.plan_complete_config(request).await
    }

    #[cfg(test)]
    async fn complete_config_plan(
        &self,
        request: &CompleteConfigPlan,
    ) -> Result<ConfigPlanResult, AdapterError> {
        validate_test_candidate(request.candidate.as_ref(), &request.candidate_hash)?;
        self.plan_complete_config(request).await
    }

    async fn plan_complete_config(
        &self,
        request: &CompleteConfigPlan,
    ) -> Result<ConfigPlanResult, AdapterError> {
        let candidate = request
            .candidate
            .as_ref()
            .ok_or(AdapterError::InvalidRequest)?;
        let materialized = self.materialize_complete(candidate).await?;
        let _guard = self.config_plan_lock.lock().await;
        let _config_file = safe_file(&self.resources.config, 0o600)?;
        let _file_lock = lock_config_file(&self.resources.config)?;
        let before = self.config_fingerprint().await?;
        validate_reload_bindings(
            &tokio::fs::read(&self.resources.config)
                .await
                .map_err(AdapterError::Io)?,
            &materialized.bytes,
            &self.resources.audit_hook,
        )?;
        let parent = self
            .resources
            .config
            .parent()
            .ok_or(AdapterError::InvalidResource)?;
        let path = parent.join(format!(".ocservia-plan-{}", Uuid::now_v7()));
        let mut stage = StagingFile::new(path.clone());
        write_new_synced(
            &path,
            &materialized.bytes,
            FileIdentity {
                mode: 0o600,
                uid: authoritative_user_owner().0,
                gid: authoritative_user_owner().1,
            },
        )
        .await?;
        self.execute(
            &self.resources.ocserv,
            &[
                "-t",
                "-c",
                path.to_str().ok_or(AdapterError::InvalidResource)?,
            ],
        )
        .await?;
        stage.remove().await?;
        if self.config_fingerprint().await? != before {
            return Err(AdapterError::Unavailable);
        }
        Ok(ConfigPlanResult {
            candidate_hash: request.candidate_hash.clone(),
            diff_redacted: redacted_candidate_diff(&materialized.redacted)?,
            warnings: Vec::new(),
            current_unchanged: true,
            staging_cleaned: !path.exists(),
            current_hash: hex::decode(before.sha256).map_err(|_| AdapterError::MalformedOutput)?,
            materialized_hash: Sha256::digest(&materialized.bytes).to_vec(),
        })
    }

    /// Apply an exact root-validated materialization using the existing durable effect store.
    ///
    /// # Errors
    /// Rejects changed TLS, expired plans or stale fingerprints, preserving uncertain effects.
    pub async fn complete_config_apply_verified(
        &self,
        verified: &VerifiedCommand<'_>,
        effect: EffectIdentity<'_>,
    ) -> Result<ConfigApplyResult, AdapterError> {
        let Some(command_envelope::Payload::CompleteConfigApply(request)) =
            verified.envelope().payload.as_ref()
        else {
            return Err(AdapterError::InvalidRequest);
        };
        self.apply_complete_config(request, effect).await
    }

    #[cfg(test)]
    async fn complete_config_apply(
        &self,
        request: &CompleteConfigApply,
        effect: EffectIdentity<'_>,
    ) -> Result<ConfigApplyResult, AdapterError> {
        validate_test_candidate(request.candidate.as_ref(), &request.candidate_hash)?;
        self.apply_complete_config(request, effect).await
    }

    async fn apply_complete_config(
        &self,
        request: &CompleteConfigApply,
        effect: EffectIdentity<'_>,
    ) -> Result<ConfigApplyResult, AdapterError> {
        let candidate = request
            .candidate
            .as_ref()
            .ok_or(AdapterError::InvalidRequest)?;
        let materialized = self.materialize_complete(candidate).await?;
        let expiry = request
            .plan_expires_at
            .as_ref()
            .ok_or(AdapterError::InvalidRequest)?;
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_err(|_| AdapterError::Unavailable)?;
        if expiry.seconds < 0
            || u64::try_from(expiry.seconds).map_err(|_| AdapterError::InvalidRequest)?
                <= now.as_secs()
            || !(0..1_000_000_000).contains(&expiry.nanos)
            || request.plan_id.len() != 16
            || request.desired_revision <= candidate.expected_revision
            || request.expected_current_hash.len() != 32
            || Sha256::digest(&materialized.bytes).as_slice() != request.materialized_hash
        {
            return Err(AdapterError::InvalidRequest);
        }
        let _config_file = safe_file(&self.resources.config, 0o600)?;
        let mut result = self
            .apply_validated_config(
                &materialized.bytes,
                &request.materialized_hash,
                &request.expected_current_hash,
                request.desired_revision,
                effect,
            )
            .await?;
        result.candidate_hash.clone_from(&request.candidate_hash);
        Ok(result)
    }
}

#[cfg(test)]
fn validate_test_candidate(
    candidate: Option<&CompleteConfigCandidate>,
    digest: &[u8],
) -> Result<(), AdapterError> {
    let candidate = candidate.ok_or(AdapterError::InvalidRequest)?;
    let canonical = ocservia_contracts::config_profile::canonical(candidate)
        .map_err(|_| AdapterError::InvalidRequest)?;
    if Sha256::digest(canonical).as_slice() == digest {
        Ok(())
    } else {
        Err(AdapterError::InvalidRequest)
    }
}

#[cfg(test)]
#[path = "complete_config_tests.rs"]
mod tests;
