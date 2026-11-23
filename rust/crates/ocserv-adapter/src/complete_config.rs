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
                true,
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
mod tests {
    use super::*;
    use crate::{FixedResources, Limits};
    use ocservia_contracts::generated::ocserv::platform::agent::v1::{
        CompleteConfigDirective, NodeLocalTlsReference,
    };
    use ocservia_test_support::PrivateTempDir;
    use std::os::unix::fs::{PermissionsExt, symlink};
    use std::path::PathBuf;

    struct Fixture {
        directory: PrivateTempDir,
        bundle: PathBuf,
        adapter: Adapter,
        plan: CompleteConfigPlan,
        original: Vec<u8>,
    }

    fn write(path: &Path, bytes: impl AsRef<[u8]>, mode: u32) {
        if path.exists() {
            std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600))
                .expect("make owned fixture writable");
        }
        if mode & 0o111 != 0 {
            super::super::tests::write_executable(path, bytes);
        } else {
            std::fs::write(path, bytes).expect("write fixture");
        }
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(mode))
            .expect("fixture permissions");
    }

    fn openssl(args: &[&str]) -> Vec<u8> {
        let result = std::process::Command::new("openssl")
            .args(args)
            .output()
            .expect("openssl fixture");
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
            // /tmp is deliberately not trusted by the production ancestry checks.
            let directory = PrivateTempDir::new("cc");
            let tls = directory.create_subdir_all("tls");
            let node = Uuid::now_v7();
            let reference = Uuid::now_v7();
            let bundle = directory
                .create_subdir_all(PathBuf::from("tls").join(reference.to_string()).join("v1"));
            write(&tls.join(".lock"), [], 0o600);
            let cert = bundle.join("server-cert.pem");
            let key = bundle.join("server-key.pem");
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
                cert.to_str().expect("cert"),
                "-outform",
                "DER",
            ]))
            .to_vec();
            let spki_sha256 = Sha256::digest(openssl(&[
                "pkey",
                "-in",
                key.to_str().expect("key"),
                "-pubout",
                "-outform",
                "DER",
            ]))
            .to_vec();
            for path in [&cert, &key] {
                std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o400))
                    .expect("root-only TLS");
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
                bundle,
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
                .expect("complete plan");
            assert!(
                result.current_unchanged && result.staging_cleaned && result.warnings.is_empty()
            );
            assert!(
                !result
                    .diff_redacted
                    .contains(self.directory.path().to_str().expect("directory"))
            );
            assert!(!result.diff_redacted.contains("PRIVATE KEY"));
            let mut request = CompleteConfigApply {
                candidate: self.plan.candidate.clone(),
                candidate_hash: self.plan.candidate_hash.clone(),
                expected_current_hash: result.current_hash,
                materialized_hash: result.materialized_hash,
                desired_revision: 1,
                plan_id: Uuid::now_v7().as_bytes().to_vec(),
                plan_expires_at: None,
            };
            request.plan_expires_at.get_or_insert_default().seconds = i64::MAX;
            request
        }
    }

    #[tokio::test]
    async fn complete_profile_rejects_startup_binding_changes_before_effect() {
        let fixture = Fixture::new();
        let apply = fixture.apply().await;
        for (from, to) in [
            ("tcp-port = 443", "tcp-port = 444"),
            ("udp-port = 0", "udp-port = 443"),
            ("run-as-user = ocservia-vpn", "run-as-user = nobody"),
            ("run-as-group = ocservia-vpn", "run-as-group = nogroup"),
            (
                "auth = \"plain[passwd=/etc/ocserv/ocpasswd]\"",
                "auth = \"pam\"",
            ),
            (
                "socket-file = /run/ocserv.socket",
                "socket-file = /run/other.socket",
            ),
            ("/v1/server-cert.pem", "/v2/server-cert.pem"),
            ("/v1/server-key.pem", "/v2/server-key.pem"),
        ] {
            let changed = String::from_utf8(fixture.original.clone())
                .expect("config")
                .replace(from, to);
            assert_ne!(changed.as_bytes(), fixture.original);
            write(&fixture.adapter.resources.config, &changed, 0o600);
            assert!(
                fixture
                    .adapter
                    .complete_config_plan(&fixture.plan)
                    .await
                    .is_err()
            );
            let mut stale_binding = apply.clone();
            stale_binding.expected_current_hash = Sha256::digest(changed.as_bytes()).to_vec();
            assert!(
                fixture
                    .adapter
                    .complete_config_apply(&stale_binding, super::super::tests::test_effect())
                    .await
                    .is_err()
            );
            assert_eq!(
                std::fs::read(&fixture.adapter.resources.config).expect("unchanged"),
                changed.as_bytes()
            );
            assert!(!fixture.adapter.resources.effect_store.exists());
        }
        for extra in [
            "ca-cert = /etc/ca.pem\n",
            "include = /etc/other.conf\n",
            "[vhost:other]\n",
            "server-key = /etc/other.key\n",
        ] {
            let mut changed = fixture.original.clone();
            changed.extend_from_slice(extra.as_bytes());
            write(&fixture.adapter.resources.config, &changed, 0o600);
            assert!(
                fixture
                    .adapter
                    .complete_config_plan(&fixture.plan)
                    .await
                    .is_err()
            );
        }
        write(&fixture.adapter.resources.config, &fixture.original, 0o600);
        fixture.apply().await;
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
        assert_eq!(outcome.candidate_hash, apply.candidate_hash);
        assert_eq!(outcome.observed_hash, apply.materialized_hash);
        assert_eq!(
            std::fs::metadata(&fixture.adapter.resources.config)
                .expect("config")
                .mode()
                & 0o7777,
            0o600
        );
        let restarted = Adapter::new(fixture.adapter.resources.clone(), Limits::default());
        let replay = restarted
            .complete_config_apply(&apply, super::super::tests::test_effect())
            .await
            .expect("durable replay");
        assert_eq!(replay, outcome);
    }

    #[tokio::test]
    async fn complete_tls_path_and_binding_fail_closed() {
        let fixture = Fixture::new();
        let key = fixture.bundle.join("server-key.pem");
        let original = fixture.bundle.join("original.pem");
        std::fs::rename(&key, &original).expect("move key");
        symlink(&original, &key).expect("symlink");
        assert!(
            fixture
                .adapter
                .complete_config_plan(&fixture.plan)
                .await
                .is_err()
        );
        std::fs::remove_file(&key).expect("remove symlink");
        std::fs::hard_link(&original, &key).expect("hardlink");
        assert!(
            fixture
                .adapter
                .complete_config_plan(&fixture.plan)
                .await
                .is_err()
        );
        std::fs::remove_file(&key).expect("remove link");
        std::fs::rename(&original, &key).expect("restore key");
        std::fs::set_permissions(&key, std::fs::Permissions::from_mode(0o640)).expect("wrong mode");
        assert!(
            fixture
                .adapter
                .complete_config_plan(&fixture.plan)
                .await
                .is_err()
        );
        std::fs::set_permissions(&key, std::fs::Permissions::from_mode(0o400))
            .expect("restore mode");
        std::fs::set_permissions(&fixture.bundle, std::fs::Permissions::from_mode(0o770))
            .expect("unsafe parent");
        assert!(
            fixture
                .adapter
                .complete_config_plan(&fixture.plan)
                .await
                .is_err()
        );
        std::fs::set_permissions(&fixture.bundle, std::fs::Permissions::from_mode(0o700))
            .expect("restore parent");
        let mut manifest: serde_json::Value = serde_json::from_slice(
            &std::fs::read(fixture.bundle.join("manifest.json")).expect("manifest"),
        )
        .expect("json");
        manifest["node_id"] = Uuid::now_v7().to_string().into();
        write(
            &fixture.bundle.join("manifest.json"),
            serde_json::to_vec(&manifest).expect("manifest json"),
            0o400,
        );
        assert!(
            fixture
                .adapter
                .complete_config_plan(&fixture.plan)
                .await
                .is_err()
        );
        assert_eq!(
            std::fs::read(&fixture.adapter.resources.config).expect("unchanged"),
            fixture.original
        );
    }

    #[tokio::test]
    async fn complete_tls_rejects_mismatched_key_expiry_and_missing_version() {
        let mut fixture = Fixture::new();
        let other = Fixture::new();
        let key_path = fixture.bundle.join("server-key.pem");
        let original_key = std::fs::read(&key_path).expect("fixture key");
        write(
            &key_path,
            std::fs::read(other.bundle.join("server-key.pem")).expect("other fixture key"),
            0o400,
        );
        assert!(
            fixture
                .adapter
                .complete_config_plan(&fixture.plan)
                .await
                .is_err()
        );
        write(&key_path, original_key, 0o400);

        let cert = fixture.bundle.join("server-cert.pem");
        // A zero-length validity window is already expired during verification.
        // OpenSSL 3.5 rejects negative days before the adapter can inspect it.
        let expired = openssl(&[
            "x509",
            "-in",
            cert.to_str().expect("cert"),
            "-signkey",
            key_path.to_str().expect("key"),
            "-days",
            "0",
        ]);
        write(&cert, expired, 0o400);
        let digest = Sha256::digest(openssl(&[
            "x509",
            "-in",
            cert.to_str().expect("cert"),
            "-outform",
            "DER",
        ]))
        .to_vec();
        let manifest_path = fixture.bundle.join("manifest.json");
        let mut manifest: serde_json::Value =
            serde_json::from_slice(&std::fs::read(&manifest_path).expect("manifest"))
                .expect("json");
        manifest["certificate_sha256"] = hex::encode(&digest).into();
        write(
            &manifest_path,
            serde_json::to_vec(&manifest).expect("manifest json"),
            0o400,
        );
        for directive in &mut fixture
            .plan
            .candidate
            .as_mut()
            .expect("candidate")
            .directives
        {
            if let Some(Value::Tls(reference)) = &mut directive.value {
                reference.certificate_sha256.clone_from(&digest);
            }
        }
        fixture.plan.candidate_hash = Sha256::digest(
            config_profile::canonical(fixture.plan.candidate.as_ref().expect("candidate"))
                .expect("canonical"),
        )
        .to_vec();
        assert!(
            fixture
                .adapter
                .complete_config_plan(&fixture.plan)
                .await
                .is_err()
        );

        let mut fixture = Fixture::new();
        for directive in &mut fixture
            .plan
            .candidate
            .as_mut()
            .expect("candidate")
            .directives
        {
            if let Some(Value::Tls(reference)) = &mut directive.value {
                reference.version = "missing-v2".to_owned();
            }
        }
        fixture.plan.candidate_hash = Sha256::digest(
            config_profile::canonical(fixture.plan.candidate.as_ref().expect("candidate"))
                .expect("canonical"),
        )
        .to_vec();
        assert!(
            fixture
                .adapter
                .complete_config_plan(&fixture.plan)
                .await
                .is_err()
        );
    }

    #[tokio::test]
    async fn complete_tls_provisioning_lock_prevents_resolution() {
        let fixture = Fixture::new();
        let lock = safe_file(
            &fixture.adapter.resources.config_tls_root.join(".lock"),
            0o600,
        )
        .expect("lock");
        rustix::fs::flock(&lock, rustix::fs::FlockOperation::LockExclusive)
            .expect("provisioning lock");
        assert!(matches!(
            fixture.adapter.complete_config_plan(&fixture.plan).await,
            Err(AdapterError::Unavailable)
        ));
        // Model the descriptor a parallel test's child can retain until exec.
        let inherited_lock = lock.try_clone().expect("inherited provisioning lock");
        rustix::fs::flock(&lock, rustix::fs::FlockOperation::Unlock)
            .expect("release provisioning lock even while a child holds a duplicate");
        drop(lock);
        fixture.apply().await;
        drop(inherited_lock);
    }

    #[tokio::test]
    async fn complete_apply_rejects_changed_hash_expiry_and_parser_before_publication() {
        let fixture = Fixture::new();
        let apply = fixture.apply().await;
        let mut bad = apply.clone();
        bad.materialized_hash[0] ^= 1;
        assert!(
            fixture
                .adapter
                .complete_config_apply(&bad, super::super::tests::test_effect())
                .await
                .is_err()
        );
        bad = apply.clone();
        bad.plan_expires_at.as_mut().expect("expiry").seconds = 1;
        assert!(
            fixture
                .adapter
                .complete_config_apply(&bad, super::super::tests::test_effect())
                .await
                .is_err()
        );
        write(
            &fixture.adapter.resources.ocserv,
            b"#!/bin/sh\nexit 9\n",
            0o700,
        );
        assert!(
            fixture
                .adapter
                .complete_config_apply(&apply, super::super::tests::test_effect())
                .await
                .is_err()
        );
        assert_eq!(
            std::fs::read(&fixture.adapter.resources.config).expect("unchanged"),
            fixture.original
        );
        assert!(
            !std::fs::read_dir(&fixture.directory)
                .expect("entries")
                .flatten()
                .any(|entry| entry
                    .file_name()
                    .to_string_lossy()
                    .starts_with(".ocservia-apply-"))
        );
    }

    #[tokio::test]
    async fn complete_apply_rolls_back_exact_bytes_and_recovers_after_publish() {
        let fixture = Fixture::new();
        let apply = fixture.apply().await;
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
            .expect("rollback outcome");
        assert!(outcome.healthy && outcome.rolled_back && !outcome.failed_critical);
        assert_eq!(outcome.observed_hash, apply.expected_current_hash);
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
        let outcome = restarted
            .complete_config_apply(&apply, super::super::tests::test_effect())
            .await
            .expect("recover exact effect");
        assert!(outcome.healthy && !outcome.rolled_back);
        assert_eq!(outcome.candidate_hash, apply.candidate_hash);
        assert_eq!(outcome.observed_hash, apply.materialized_hash);
    }

    #[tokio::test]
    async fn complete_profile_materializes_audit_hook_without_breaking_redacted_diff() {
        let mut fixture = Fixture::new();
        let baseline = fixture
            .adapter
            .complete_config_plan(&fixture.plan)
            .await
            .expect("baseline plan without hook");
        let hook = fixture.directory.join("vpn-session-hook");
        write(&hook, b"#!/bin/sh\nexit 0\n", 0o755);
        fixture.adapter.resources.audit_hook = hook.clone();

        let with_hook = fixture
            .adapter
            .complete_config_plan(&fixture.plan)
            .await
            .expect("plan with hook");
        assert_eq!(with_hook.diff_redacted, baseline.diff_redacted);
        assert!(!with_hook.diff_redacted.contains("connect-script"));
        assert!(!with_hook.diff_redacted.contains("disconnect-script"));
        assert_ne!(with_hook.materialized_hash, baseline.materialized_hash);

        let mut apply = CompleteConfigApply {
            candidate: fixture.plan.candidate.clone(),
            candidate_hash: fixture.plan.candidate_hash.clone(),
            expected_current_hash: with_hook.current_hash,
            materialized_hash: with_hook.materialized_hash,
            desired_revision: 1,
            plan_id: Uuid::now_v7().as_bytes().to_vec(),
            plan_expires_at: None,
        };
        apply.plan_expires_at.get_or_insert_default().seconds = i64::MAX;
        let outcome = fixture
            .adapter
            .complete_config_apply(&apply, super::super::tests::test_effect())
            .await
            .expect("apply with hook");
        assert!(outcome.healthy && !outcome.rolled_back);
        assert_eq!(outcome.observed_hash, apply.materialized_hash);
        let written = std::fs::read_to_string(&fixture.adapter.resources.config).expect("config");
        assert!(written.contains(&format!("connect-script = {}\n", hook.display())));
        assert!(written.contains(&format!("disconnect-script = {}\n", hook.display())));
    }
}
