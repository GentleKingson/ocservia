use super::{
    ARTIFACT_CHUNK_BYTES, Adapter, AdapterError, ArtifactData, ArtifactLeaseIdentity,
    CertificateArtifactResult, CertificateCsrResult, CertificateRevokeResult, Digest, Duration,
    EffectStore, HashSet, MAX_ARTIFACT_BYTES, MetadataExt, MutationResult, OpenOptionsExt, Path,
    PermissionsExt, SealedSecretPurpose, SealedSecretV1, SealedSecretVersion, Sha256, StagingFile,
    StdOpenOptions, SystemTime, Uuid, Write, Zeroizing, atomic_replace, effect_now, io,
    sync_directory, valid_dns_name,
};

impl Adapter {
    /// Generates a private key in the fixed root-owned key directory and returns only CSR bytes
    /// and a digest of the corresponding public key.
    ///
    /// # Errors
    ///
    /// Returns a validation, resource, process, or I/O error if key generation cannot complete.
    #[allow(clippy::too_many_lines)]
    pub async fn certificate_csr(
        &self,
        certificate_id: &[u8],
        common_name: &str,
        dns_names: &[String],
        key_bits: u32,
    ) -> Result<CertificateCsrResult, AdapterError> {
        let id = Uuid::from_slice(certificate_id).map_err(|_| AdapterError::InvalidRequest)?;
        if id.get_version_num() != 7
            || !valid_dns_name(common_name)
            || dns_names.len() > 32
            || !dns_names.iter().all(|name| valid_dns_name(name))
            || !matches!(key_bits, 2048 | 3072 | 4096)
        {
            return Err(AdapterError::InvalidRequest);
        }
        tokio::fs::create_dir_all(&self.resources.certificate_key_dir)
            .await
            .map_err(AdapterError::Io)?;
        tokio::fs::set_permissions(
            &self.resources.certificate_key_dir,
            std::fs::Permissions::from_mode(0o710),
        )
        .await
        .map_err(AdapterError::Io)?;
        let metadata = tokio::fs::symlink_metadata(&self.resources.certificate_key_dir)
            .await
            .map_err(AdapterError::Io)?;
        if !metadata.is_dir()
            || metadata.file_type().is_symlink()
            || metadata.permissions().mode() & 0o022 != 0
        {
            return Err(AdapterError::InvalidResource);
        }
        let key_path = self
            .resources
            .certificate_key_dir
            .join(format!("{id}.key.pem"));
        #[expect(
            clippy::verbose_bit_mask,
            reason = "the octal mask directly expresses forbidden group/other permissions"
        )]
        let existing_key = match tokio::fs::symlink_metadata(&key_path).await {
            Ok(key_metadata)
                if key_metadata.is_file()
                    && !key_metadata.file_type().is_symlink()
                    && key_metadata.permissions().mode() & 0o077 == 0 =>
            {
                true
            }
            Ok(_) => return Err(AdapterError::InvalidResource),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => false,
            Err(error) => return Err(AdapterError::Io(error)),
        };
        let subject = format!("/CN={common_name}");
        let key_spec = format!("rsa:{key_bits}");
        let key_path_text = key_path.to_str().ok_or(AdapterError::InvalidResource)?;
        let mut owned = vec!["req".to_owned(), "-new".to_owned()];
        if existing_key {
            owned.extend(["-key".to_owned(), key_path_text.to_owned()]);
        } else {
            owned.extend([
                "-newkey".to_owned(),
                key_spec,
                "-nodes".to_owned(),
                "-keyout".to_owned(),
                key_path_text.to_owned(),
            ]);
        }
        owned.extend([
            "-subj".to_owned(),
            subject,
            "-outform".to_owned(),
            "DER".to_owned(),
        ]);
        if !dns_names.is_empty() {
            owned.push("-addext".to_owned());
            owned.push(format!(
                "subjectAltName={}",
                dns_names
                    .iter()
                    .map(|name| format!("DNS:{name}"))
                    .collect::<Vec<_>>()
                    .join(",")
            ));
        }
        let args = owned.iter().map(String::as_str).collect::<Vec<_>>();
        let mut new_key_guard = (!existing_key).then(|| StagingFile::new(key_path.clone()));
        let csr = match self.execute(&self.resources.openssl, &args).await {
            Ok(output) if output.stdout.len() >= 64 && output.stdout.len() <= 64 * 1024 => {
                output.stdout
            }
            Ok(_) => return Err(AdapterError::MalformedOutput),
            Err(error) => return Err(error),
        };
        if !existing_key {
            tokio::fs::set_permissions(&key_path, std::fs::Permissions::from_mode(0o600))
                .await
                .map_err(AdapterError::Io)?;
        }
        let public = self
            .execute(
                &self.resources.openssl,
                &["pkey", "-in", key_path_text, "-pubout", "-outform", "DER"],
            )
            .await?;
        if public.stdout.is_empty() || public.stdout.len() > 64 * 1024 {
            return Err(AdapterError::MalformedOutput);
        }
        if let Some(guard) = new_key_guard.as_mut() {
            guard.disarm();
        }
        Ok(CertificateCsrResult {
            certificate_id: certificate_id.to_vec(),
            csr_der: csr,
            public_key_sha256: Sha256::digest(public.stdout).to_vec(),
        })
    }

    /// Removes only the private key derived from the supplied certificate UUID.
    ///
    /// # Errors
    ///
    /// Returns a validation, resource, or I/O error if the fixed key cannot be removed safely.
    pub async fn certificate_revoke(
        &self,
        certificate_id: &[u8],
        certificate_version: u64,
    ) -> Result<CertificateRevokeResult, AdapterError> {
        let id = Uuid::from_slice(certificate_id).map_err(|_| AdapterError::InvalidRequest)?;
        if id.get_version_num() != 7 || certificate_version == 0 {
            return Err(AdapterError::InvalidRequest);
        }
        let _guard = self.certificate_artifact_lock.lock().await;
        let artifact_ids = EffectStore::open_for_mutation(&self.resources)?
            .revoke_certificate_artifacts(id.as_bytes())?;
        let artifact_dir = self.resources.certificate_key_dir.join("artifacts");
        for artifact_id in artifact_ids {
            let artifact = Uuid::from_bytes(artifact_id);
            for path in [
                artifact_dir.join(format!("{artifact}.p12")),
                artifact_dir.join(format!(".{artifact}.p12")),
                artifact_dir.join(format!(".{artifact}.chain.pem")),
            ] {
                match tokio::fs::symlink_metadata(&path).await {
                    Ok(metadata) if metadata.is_file() && !metadata.file_type().is_symlink() => {
                        tokio::fs::remove_file(path)
                            .await
                            .map_err(AdapterError::Io)?;
                    }
                    Ok(_) => return Err(AdapterError::InvalidResource),
                    Err(error) if error.kind() == io::ErrorKind::NotFound => {}
                    Err(error) => return Err(AdapterError::Io(error)),
                }
            }
        }
        let key_path = self
            .resources
            .certificate_key_dir
            .join(format!("{id}.key.pem"));
        match tokio::fs::symlink_metadata(&key_path).await {
            Ok(metadata) if metadata.file_type().is_symlink() || !metadata.is_file() => {
                return Err(AdapterError::InvalidResource);
            }
            Ok(_) => tokio::fs::remove_file(&key_path)
                .await
                .map_err(AdapterError::Io)?,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(AdapterError::Io(error)),
        }
        Ok(CertificateRevokeResult {
            certificate_id: certificate_id.to_vec(),
            key_removed: true,
        })
    }

    /// Creates an encrypted P12 only in the fixed UUID-addressed artifact spool.
    ///
    /// # Errors
    ///
    /// Returns a validation, resource, process, or I/O error if packaging cannot complete.
    #[allow(clippy::too_many_arguments, clippy::too_many_lines)]
    pub async fn certificate_p12(
        &self,
        certificate_id: &[u8],
        artifact_id: &[u8],
        certificate_chain_pem: &[u8],
        sealed_password: &SealedSecretV1,
        certificate_version: u64,
        operation_id: &[u8],
        artifact_expires_at_unix_seconds: i64,
    ) -> Result<CertificateArtifactResult, AdapterError> {
        let certificate =
            Uuid::from_slice(certificate_id).map_err(|_| AdapterError::InvalidRequest)?;
        let artifact = Uuid::from_slice(artifact_id).map_err(|_| AdapterError::InvalidRequest)?;
        let operation = Uuid::from_slice(operation_id).map_err(|_| AdapterError::InvalidRequest)?;
        if certificate.get_version_num() != 7
            || artifact.get_version_num() != 7
            || operation.get_version_num() != 7
            || certificate_version == 0
            || certificate_chain_pem.len() < 64
            || certificate_chain_pem.len() > 256 * 1024
            || SealedSecretVersion::try_from(sealed_password.version).ok()
                != Some(SealedSecretVersion::V1)
            || SealedSecretPurpose::try_from(sealed_password.purpose).ok()
                != Some(SealedSecretPurpose::CertificateP12Password)
            || sealed_password.ciphertext.len() < 32
            || sealed_password.ciphertext.len() > 16 * 1024
            || sealed_password.key_id != self.resources.p12_secret_key_id
            || artifact_expires_at_unix_seconds <= effect_now()?
        {
            return Err(AdapterError::InvalidRequest);
        }
        let _guard = self.certificate_artifact_lock.lock().await;
        let key_path = self
            .resources
            .certificate_key_dir
            .join(format!("{certificate}.key.pem"));
        let key_metadata = tokio::fs::symlink_metadata(&key_path)
            .await
            .map_err(AdapterError::Io)?;
        if !key_metadata.is_file()
            || key_metadata.file_type().is_symlink()
            || key_metadata.permissions().mode() & 0o077 != 0
        {
            return Err(AdapterError::InvalidResource);
        }
        let artifact_dir = self.resources.certificate_key_dir.join("artifacts");
        tokio::fs::create_dir_all(&artifact_dir)
            .await
            .map_err(AdapterError::Io)?;
        tokio::fs::set_permissions(&artifact_dir, std::fs::Permissions::from_mode(0o700))
            .await
            .map_err(AdapterError::Io)?;
        let directory_metadata = tokio::fs::symlink_metadata(&artifact_dir)
            .await
            .map_err(AdapterError::Io)?;
        if !directory_metadata.is_dir()
            || directory_metadata.file_type().is_symlink()
            || directory_metadata.permissions().mode() & 0o022 != 0
        {
            return Err(AdapterError::InvalidResource);
        }
        let chain_path = artifact_dir.join(format!(".{artifact}.chain.pem"));
        let staging_path = artifact_dir.join(format!(".{artifact}.p12"));
        let output_path = artifact_dir.join(format!("{artifact}.p12"));
        let mut effect_store = EffectStore::open_for_mutation(&self.resources)?;
        effect_store.prepare_artifact(
            artifact.as_bytes(),
            certificate.as_bytes(),
            certificate_version,
            operation.as_bytes(),
            artifact_expires_at_unix_seconds,
        )?;
        let artifact_registered = effect_store.artifact_is_registered(
            artifact.as_bytes(),
            certificate.as_bytes(),
            certificate_version,
            operation.as_bytes(),
            artifact_expires_at_unix_seconds,
        )?;
        drop(effect_store);
        let password = self.decrypt_p12_password(sealed_password).await?;
        let register = |data: &[u8]| {
            let digest = Sha256::digest(data).to_vec();
            let size = u64::try_from(data.len()).map_err(|_| AdapterError::OutputLimit)?;
            EffectStore::open_for_mutation(&self.resources)?.register_artifact(
                artifact_id,
                certificate_id,
                certificate_version,
                operation_id,
                &digest,
                size,
                artifact_expires_at_unix_seconds,
            )?;
            Ok(CertificateArtifactResult {
                certificate_id: certificate_id.to_vec(),
                artifact_id: artifact_id.to_vec(),
                artifact_sha256: digest,
                artifact_size: size,
            })
        };
        match tokio::fs::symlink_metadata(&output_path).await {
            Ok(metadata) if metadata.is_file() && !metadata.file_type().is_symlink() => {
                let output_text = output_path.to_str().ok_or(AdapterError::InvalidResource)?;
                if self
                    .execute_with_input(
                        &self.resources.openssl,
                        &["pkcs12", "-in", output_text, "-passin", "stdin", "-noout"],
                        &password,
                    )
                    .await
                    .is_ok()
                {
                    let data = tokio::fs::read(&output_path)
                        .await
                        .map_err(AdapterError::Io)?;
                    if !data.is_empty() && data.len() <= MAX_ARTIFACT_BYTES {
                        return register(&data);
                    }
                }
                tokio::fs::remove_file(&output_path)
                    .await
                    .map_err(AdapterError::Io)?;
                if artifact_registered {
                    return Err(AdapterError::InvalidResource);
                }
            }
            Ok(_) => return Err(AdapterError::InvalidResource),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                if artifact_registered {
                    return Err(AdapterError::InvalidResource);
                }
            }
            Err(error) => return Err(AdapterError::Io(error)),
        }
        let data = self
            .stage_p12(
                &key_path,
                &chain_path,
                &staging_path,
                certificate_chain_pem,
                &password,
            )
            .await?;
        atomic_replace(&staging_path, &output_path).await?;
        register(&data)
    }

    async fn stage_p12(
        &self,
        key_path: &Path,
        chain_path: &Path,
        staging_path: &Path,
        certificate_chain_pem: &[u8],
        password: &[u8],
    ) -> Result<Vec<u8>, AdapterError> {
        for stale in [chain_path, staging_path] {
            match tokio::fs::symlink_metadata(stale).await {
                Ok(metadata) if metadata.is_file() && !metadata.file_type().is_symlink() => {
                    tokio::fs::remove_file(stale)
                        .await
                        .map_err(AdapterError::Io)?;
                }
                Ok(_) => return Err(AdapterError::InvalidResource),
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
                Err(error) => return Err(AdapterError::Io(error)),
            }
        }
        let mut chain_file = StdOpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(chain_path)
            .map_err(AdapterError::Io)?;
        let chain_guard = StagingFile::new(chain_path.to_path_buf());
        chain_file
            .write_all(certificate_chain_pem)
            .map_err(AdapterError::Io)?;
        chain_file.sync_all().map_err(AdapterError::Io)?;
        drop(chain_file);
        let key_text = key_path.to_str().ok_or(AdapterError::InvalidResource)?;
        let chain_text = chain_path.to_str().ok_or(AdapterError::InvalidResource)?;
        let staging_text = staging_path.to_str().ok_or(AdapterError::InvalidResource)?;
        drop(
            StdOpenOptions::new()
                .write(true)
                .create_new(true)
                .mode(0o600)
                .open(staging_path)
                .map_err(AdapterError::Io)?,
        );
        let mut staging_guard = StagingFile::new(staging_path.to_path_buf());
        let created = self
            .execute_with_input(
                &self.resources.openssl,
                &[
                    "pkcs12",
                    "-export",
                    "-inkey",
                    key_text,
                    "-in",
                    chain_text,
                    "-out",
                    staging_text,
                    "-passout",
                    "stdin",
                ],
                password,
            )
            .await;
        drop(chain_guard);
        created?;
        self.execute_with_input(
            &self.resources.openssl,
            &["pkcs12", "-in", staging_text, "-passin", "stdin", "-noout"],
            password,
        )
        .await?;
        let data = tokio::fs::read(staging_path)
            .await
            .map_err(AdapterError::Io)?;
        if data.is_empty() || data.len() > MAX_ARTIFACT_BYTES {
            return Err(AdapterError::OutputLimit);
        }
        tokio::fs::set_permissions(staging_path, std::fs::Permissions::from_mode(0o600))
            .await
            .map_err(AdapterError::Io)?;
        staging_guard.disarm();
        Ok(data)
    }

    async fn decrypt_p12_password(
        &self,
        sealed_password: &SealedSecretV1,
    ) -> Result<Zeroizing<Vec<u8>>, AdapterError> {
        let key = self
            .resources
            .p12_secret_key
            .to_str()
            .ok_or(AdapterError::InvalidResource)?;
        let decrypted = self
            .execute_with_input(
                &self.resources.openssl,
                &[
                    "pkeyutl",
                    "-decrypt",
                    "-inkey",
                    key,
                    "-pkeyopt",
                    "rsa_padding_mode:oaep",
                    "-pkeyopt",
                    "rsa_oaep_md:sha256",
                ],
                &sealed_password.ciphertext,
            )
            .await?;
        if !decrypted.stdout.is_empty()
            && decrypted.stdout.len() <= 1024
            && !decrypted
                .stdout
                .iter()
                .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
        {
            Ok(Zeroizing::new(decrypted.stdout))
        } else {
            Err(AdapterError::MalformedOutput)
        }
    }

    /// Reads one fixed-size artifact chunk only while the exact signed grant
    /// owns the durable root-side lease.
    ///
    /// # Errors
    ///
    /// Returns a validation, authorization, resource, or I/O error when the
    /// lease cannot be acquired or the stored artifact cannot be read safely.
    pub async fn artifact_read(
        &self,
        identity: ArtifactLeaseIdentity<'_>,
        offset: u64,
    ) -> Result<ArtifactData, AdapterError> {
        use tokio::io::{AsyncReadExt as _, AsyncSeekExt as _};

        let _guard = self.certificate_artifact_lock.lock().await;
        let mut store = EffectStore::open_for_mutation(&self.resources)?;
        let record = store.lease_artifact(identity)?;
        if offset != record.active_offset || offset > record.content_size {
            return Err(AdapterError::InvalidRequest);
        }
        let total_size =
            usize::try_from(record.content_size).map_err(|_| AdapterError::InvalidResource)?;
        if total_size > MAX_ARTIFACT_BYTES {
            return Err(AdapterError::InvalidResource);
        }
        let artifact = Uuid::from_bytes(record.artifact_id);
        let path = self
            .resources
            .certificate_key_dir
            .join("artifacts")
            .join(format!("{artifact}.p12"));
        let symlink_meta = tokio::fs::symlink_metadata(&path)
            .await
            .map_err(AdapterError::Io)?;
        if !symlink_meta.is_file()
            || symlink_meta.file_type().is_symlink()
            || symlink_meta.permissions().mode() & 0o077 != 0
            || symlink_meta.len() != record.content_size
        {
            return Err(AdapterError::InvalidResource);
        }
        let mut file = tokio::fs::File::open(&path)
            .await
            .map_err(AdapterError::Io)?;
        let fd_meta = file.metadata().await.map_err(AdapterError::Io)?;
        if !fd_meta.is_file()
            || fd_meta.permissions().mode() & 0o077 != 0
            || fd_meta.len() != record.content_size
            || fd_meta.dev() != symlink_meta.dev()
            || fd_meta.ino() != symlink_meta.ino()
        {
            return Err(AdapterError::InvalidResource);
        }

        let start = usize::try_from(offset).map_err(|_| AdapterError::InvalidRequest)?;
        let end = start.saturating_add(ARTIFACT_CHUNK_BYTES).min(total_size);
        let chunk_len = end - start;
        let mut chunk = Vec::with_capacity(chunk_len);

        if offset == 0 {
            // Stream-verify full file SHA-256 on lease start while keeping only
            // the first chunk (<= 256 KiB) in memory.
            let mut hasher = Sha256::new();
            let mut buf = vec![0_u8; 64 * 1024].into_boxed_slice();
            let mut total_read = 0_usize;
            loop {
                let n = file.read(&mut buf).await.map_err(AdapterError::Io)?;
                if n == 0 {
                    break;
                }
                total_read = total_read
                    .checked_add(n)
                    .ok_or(AdapterError::InvalidResource)?;
                if total_read > MAX_ARTIFACT_BYTES || total_read > total_size {
                    return Err(AdapterError::InvalidResource);
                }
                hasher.update(&buf[..n]);
                if chunk.len() < chunk_len {
                    let take = (chunk_len - chunk.len()).min(n);
                    chunk.extend_from_slice(&buf[..take]);
                }
            }
            if total_read != total_size || hasher.finalize().as_slice() != record.content_sha256 {
                return Err(AdapterError::InvalidResource);
            }
        } else {
            file.seek(io::SeekFrom::Start(offset))
                .await
                .map_err(AdapterError::Io)?;
            chunk.resize(chunk_len, 0);
            file.read_exact(&mut chunk)
                .await
                .map_err(AdapterError::Io)?;
        }

        store.advance_artifact(
            identity,
            offset,
            u64::try_from(end).map_err(|_| AdapterError::OutputLimit)?,
        )?;
        Ok(ArtifactData {
            artifact_id: record.artifact_id.to_vec(),
            grant_id: identity.grant_id.to_vec(),
            offset,
            data: chunk,
            eof: end == total_size,
            sha256: record.content_sha256.to_vec(),
        })
    }

    /// Commits one artifact consumption and removes the root-owned bytes.
    ///
    /// # Errors
    ///
    /// Returns a validation, authorization, resource, or I/O error when the
    /// exact lease cannot be consumed or the artifact cannot be removed.
    pub async fn artifact_consume(
        &self,
        identity: ArtifactLeaseIdentity<'_>,
        sha256: &[u8],
        size: u64,
    ) -> Result<MutationResult, AdapterError> {
        let _guard = self.certificate_artifact_lock.lock().await;
        EffectStore::open_for_mutation(&self.resources)?
            .consume_artifact(identity, sha256, size)?;
        let artifact =
            Uuid::from_slice(identity.artifact_id).map_err(|_| AdapterError::InvalidRequest)?;
        let path = self
            .resources
            .certificate_key_dir
            .join("artifacts")
            .join(format!("{artifact}.p12"));
        match tokio::fs::symlink_metadata(&path).await {
            Ok(metadata) if metadata.is_file() && !metadata.file_type().is_symlink() => {
                tokio::fs::remove_file(&path)
                    .await
                    .map_err(AdapterError::Io)?;
            }
            Ok(_) => return Err(AdapterError::InvalidResource),
            Err(error) if error.kind() == io::ErrorKind::NotFound => {}
            Err(error) => return Err(AdapterError::Io(error)),
        }
        if let Some(parent) = path.parent() {
            sync_directory(parent).await?;
        }
        Ok(MutationResult { applied: true })
    }

    /// Confirms an exact already-consumed record without reading, deleting, or
    /// changing either the root ledger or artifact bytes.
    ///
    /// # Errors
    ///
    /// Returns an error when the signed identity or evidence does not exactly
    /// match the authenticated root ledger record.
    pub async fn artifact_confirm_consumed(
        &self,
        identity: ArtifactLeaseIdentity<'_>,
        sha256: &[u8],
        size: u64,
    ) -> Result<MutationResult, AdapterError> {
        let _guard = self.certificate_artifact_lock.lock().await;
        let consumed = EffectStore::open_existing(&self.resources)?
            .confirm_artifact_consumed(identity, sha256, size)?;
        Ok(MutationResult { applied: consumed })
    }
}

impl Adapter {
    /// Removes only expired UUID-addressed encrypted P12 artifacts and their staging files.
    ///
    /// # Errors
    ///
    /// Returns an I/O error when the fixed artifact directory cannot be inspected or cleaned.
    pub async fn cleanup_stale_certificate_artifacts(&self) -> Result<(), AdapterError> {
        let _guard = self.certificate_artifact_lock.lock().await;
        let directory = self.resources.certificate_key_dir.join("artifacts");
        let directory_metadata = match tokio::fs::symlink_metadata(&directory).await {
            Ok(metadata) => metadata,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
            Err(error) => return Err(AdapterError::Io(error)),
        };
        if !directory_metadata.is_dir() || directory_metadata.file_type().is_symlink() {
            return Err(AdapterError::InvalidResource);
        }
        // Close the legacy group-readable directory before inspecting or deleting
        // any pre-grant files. This happens during privd startup before Agent runs.
        tokio::fs::set_permissions(&directory, std::fs::Permissions::from_mode(0o700))
            .await
            .map_err(AdapterError::Io)?;
        let hardened = tokio::fs::symlink_metadata(&directory)
            .await
            .map_err(AdapterError::Io)?;
        if !hardened.is_dir() || hardened.file_type().is_symlink() || hardened.mode() & 0o077 != 0 {
            return Err(AdapterError::InvalidResource);
        }
        let mut store = EffectStore::open_for_mutation(&self.resources)?;
        let terminal: HashSet<[u8; 16]> = store.reconcile_artifacts()?.into_iter().collect();
        let tracked: HashSet<[u8; 16]> = store.tracked_artifact_ids()?.into_iter().collect();
        let mut entries = match tokio::fs::read_dir(&directory).await {
            Ok(entries) => entries,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
            Err(error) => return Err(AdapterError::Io(error)),
        };
        let cutoff = SystemTime::now()
            .checked_sub(Duration::from_mins(15))
            .ok_or(AdapterError::InvalidResource)?;
        while let Some(entry) = entries.next_entry().await.map_err(AdapterError::Io)? {
            let name = entry.file_name();
            let name = name.to_string_lossy();
            let is_staging = name.starts_with('.');
            let id_text = if is_staging {
                name.strip_prefix('.').and_then(|value| {
                    value
                        .strip_suffix(".p12")
                        .or_else(|| value.strip_suffix(".chain.pem"))
                })
            } else {
                name.strip_suffix(".p12")
            };
            let Some(id_text) = id_text else { continue };
            let Ok(id) = Uuid::parse_str(id_text) else {
                continue;
            };
            if id.get_version_num() != 7 {
                continue;
            }
            let metadata = tokio::fs::symlink_metadata(entry.path())
                .await
                .map_err(AdapterError::Io)?;
            if !metadata.is_file() || metadata.file_type().is_symlink() {
                return Err(AdapterError::InvalidResource);
            }
            let is_tracked = tracked.contains(id.as_bytes());
            let should_remove = !is_tracked
                || terminal.contains(id.as_bytes())
                || (is_staging && metadata.modified().map_err(AdapterError::Io)? < cutoff);
            if should_remove {
                tokio::fs::remove_file(entry.path())
                    .await
                    .map_err(AdapterError::Io)?;
            }
        }
        sync_directory(&directory).await
    }
}
