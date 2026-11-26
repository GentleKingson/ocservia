use super::{
    Adapter, AdapterError, AsyncReadExt, ConfigApplyResult, ConfigFingerprint, Digest,
    EffectIdentity, MAX_CONFIG_BYTES, PathBuf, PrepareEffect, Sha256, StagingFile, Uuid,
    atomic_replace, complete_config, file_identity, lock_config_file, sync_directory,
    write_new_synced,
};
#[cfg(test)]
use std::io;

impl Adapter {
    /// Fingerprints the fixed Ocserv config with a strict size bound.
    ///
    /// # Errors
    ///
    /// Returns a stable adapter error for missing, unreadable, or oversized data.
    pub async fn config_fingerprint(&self) -> Result<ConfigFingerprint, AdapterError> {
        let file = tokio::fs::File::open(&self.resources.config)
            .await
            .map_err(AdapterError::Io)?;
        let mut bytes = Vec::with_capacity(4096);
        file.take(u64::try_from(MAX_CONFIG_BYTES + 1).map_err(|_| AdapterError::OutputLimit)?)
            .read_to_end(&mut bytes)
            .await
            .map_err(AdapterError::Io)?;
        if bytes.len() > MAX_CONFIG_BYTES {
            return Err(AdapterError::OutputLimit);
        }
        Ok(ConfigFingerprint {
            sha256: hex::encode(Sha256::digest(&bytes)),
            size_bytes: u64::try_from(bytes.len()).map_err(|_| AdapterError::OutputLimit)?,
        })
    }

    // Only the complete TLS materializer can reach this.
    pub(super) async fn apply_validated_config(
        &self,
        candidate: &[u8],
        candidate_hash: &[u8],
        expected_current_hash: &[u8],
        desired_revision: u64,
        effect: EffectIdentity<'_>,
    ) -> Result<ConfigApplyResult, AdapterError> {
        self.apply_validated_config_inner(
            candidate,
            candidate_hash,
            expected_current_hash,
            desired_revision,
            effect,
            true,
        )
        .await
    }

    #[cfg(test)]
    pub(super) async fn apply_validated_config_without_preflight(
        &self,
        candidate: &[u8],
        candidate_hash: &[u8],
        expected_current_hash: &[u8],
        desired_revision: u64,
        effect: EffectIdentity<'_>,
    ) -> Result<ConfigApplyResult, AdapterError> {
        self.apply_validated_config_inner(
            candidate,
            candidate_hash,
            expected_current_hash,
            desired_revision,
            effect,
            false,
        )
        .await
    }

    async fn apply_validated_config_inner(
        &self,
        candidate: &[u8],
        candidate_hash: &[u8],
        expected_current_hash: &[u8],
        desired_revision: u64,
        effect: EffectIdentity<'_>,
        complete: bool,
    ) -> Result<ConfigApplyResult, AdapterError> {
        let _guard = self.config_plan_lock.lock().await;
        let _file_lock = lock_config_file(&self.resources.config)?;
        let before = self.config_fingerprint().await?;
        let before_bytes = tokio::fs::read(&self.resources.config)
            .await
            .map_err(AdapterError::Io)?;
        let previous_hash =
            hex::decode(&before.sha256).map_err(|_| AdapterError::MalformedOutput)?;
        // Reject a stale plan while the config lock still proves no effect occurred. A current
        // candidate remains eligible for post-publish crash recovery below.
        if previous_hash != expected_current_hash && previous_hash != candidate_hash {
            return Err(AdapterError::Unavailable);
        }
        if complete {
            complete_config::validate_reload_bindings(
                &before_bytes,
                candidate,
                &self.resources.audit_hook,
            )?;
        }
        let preparation = self.with_effect_store(true, |effect_store| {
            effect_store.prepare(
                "config_apply",
                "ocserv.conf",
                desired_revision,
                effect,
                &before_bytes,
                candidate,
            )
        })?;
        if preparation == PrepareEffect::AlreadyApplied {
            if previous_hash != candidate_hash {
                return Err(AdapterError::Unavailable);
            }
            return Ok(applied_config(
                candidate_hash,
                expected_current_hash,
                candidate_hash.to_vec(),
                desired_revision,
            ));
        }
        if previous_hash == candidate_hash {
            return self
                .recover_published_config(
                    candidate_hash,
                    expected_current_hash,
                    desired_revision,
                    effect,
                )
                .await;
        }
        let parent = self
            .resources
            .config
            .parent()
            .ok_or(AdapterError::InvalidResource)?;
        let id = Uuid::now_v7();
        let backup_path = self
            .publish_config_candidate(parent, id, candidate, complete)
            .await?;
        let new_health = self.config_health().await;
        if new_health.is_ok() {
            let retained = parent.join(format!(".ocservia-success-{id}"));
            tokio::fs::rename(&backup_path, &retained)
                .await
                .map_err(AdapterError::Io)?;
            sync_directory(parent).await?;
            self.prune_config_backups().await?;
            let observed = self.config_fingerprint().await?;
            self.with_effect_store(true, |effect_store| {
                effect_store.mark_applied("config_apply", "ocserv.conf", desired_revision, effect)
            })?;
            return Ok(applied_config(
                candidate_hash,
                &previous_hash,
                hex::decode(observed.sha256).map_err(|_| AdapterError::MalformedOutput)?,
                desired_revision,
            ));
        }

        self.rollback_config(
            &backup_path,
            candidate_hash,
            previous_hash,
            desired_revision,
            effect,
        )
        .await
    }

    async fn rollback_config(
        &self,
        backup_path: &std::path::Path,
        candidate_hash: &[u8],
        previous_hash: Vec<u8>,
        desired_revision: u64,
        effect: EffectIdentity<'_>,
    ) -> Result<ConfigApplyResult, AdapterError> {
        let rollback = async {
            atomic_replace(backup_path, &self.resources.config).await?;
            self.config_health().await?;
            let observed = self.config_fingerprint().await?;
            if observed.sha256 != hex::encode(&previous_hash) {
                return Err(AdapterError::Unavailable);
            }
            Ok::<ConfigFingerprint, AdapterError>(observed)
        }
        .await;
        // Neither a successful rollback nor a critically failed one confirms an
        // applied revision. Settle the prepared effect before reporting the result.
        self.with_effect_store(true, |effect_store| {
            effect_store.mark_absent("config_apply", "ocserv.conf", desired_revision, effect)
        })?;
        match rollback {
            Ok(observed) => Ok(ConfigApplyResult {
                candidate_hash: candidate_hash.to_vec(),
                previous_hash,
                observed_hash: hex::decode(observed.sha256)
                    .map_err(|_| AdapterError::MalformedOutput)?,
                applied_revision: 0,
                healthy: true,
                rolled_back: true,
                failed_critical: false,
                failure_code: "health_check_failed".to_owned(),
            }),
            Err(_) => Ok(ConfigApplyResult {
                candidate_hash: candidate_hash.to_vec(),
                previous_hash,
                observed_hash: Vec::new(),
                applied_revision: 0,
                healthy: false,
                rolled_back: false,
                failed_critical: true,
                failure_code: "rollback_failed".to_owned(),
            }),
        }
    }

    async fn publish_config_candidate(
        &self,
        parent: &std::path::Path,
        id: Uuid,
        candidate: &[u8],
        complete: bool,
    ) -> Result<PathBuf, AdapterError> {
        let stage_path = parent.join(format!(".ocservia-apply-{id}"));
        let backup_path = parent.join(format!(".ocservia-backup-{id}"));
        let mut stage = StagingFile::new(stage_path.clone());
        let mut backup = StagingFile::new(backup_path.clone());
        let identity = file_identity(&self.resources.config).await?;
        #[cfg(test)]
        if self.take_config_apply_fault(1) {
            return Err(AdapterError::Io(io::Error::from_raw_os_error(28)));
        }
        write_new_synced(
            &backup_path,
            &tokio::fs::read(&self.resources.config)
                .await
                .map_err(AdapterError::Io)?,
            identity,
        )
        .await?;
        #[cfg(test)]
        if self.take_config_apply_fault(2) {
            return Err(AdapterError::Unavailable);
        }
        write_new_synced(&stage_path, candidate, identity).await?;
        sync_directory(parent).await?;
        if complete {
            self.execute(
                &self.resources.ocserv,
                &[
                    "-t",
                    "-c",
                    stage_path.to_str().ok_or(AdapterError::InvalidResource)?,
                ],
            )
            .await?;
        }
        #[cfg(test)]
        if self.take_config_apply_fault(3) {
            return Err(AdapterError::Unavailable);
        }
        // From this point onward, cancellation may race the rename or directory fsync.
        // Retain the durable backup even when publication has not yet become observable.
        backup.disarm();
        atomic_replace(&stage_path, &self.resources.config).await?;
        stage.disarm();
        #[cfg(test)]
        if self.take_config_apply_fault(4) {
            return Err(AdapterError::Unavailable);
        }

        Ok(backup_path)
    }

    async fn recover_published_config(
        &self,
        candidate_hash: &[u8],
        expected_current_hash: &[u8],
        desired_revision: u64,
        effect: EffectIdentity<'_>,
    ) -> Result<ConfigApplyResult, AdapterError> {
        let parent = self
            .resources
            .config
            .parent()
            .ok_or(AdapterError::InvalidResource)?;
        let backup = self.find_config_recovery(expected_current_hash).await?;
        if self.config_health().await.is_ok() {
            let Some(path) = backup else {
                return Err(AdapterError::Unavailable);
            };
            if path
                .file_name()
                .is_some_and(|name| name.to_string_lossy().starts_with(".ocservia-backup-"))
            {
                let retained = parent.join(format!(".ocservia-success-{}", Uuid::now_v7()));
                tokio::fs::rename(path, retained)
                    .await
                    .map_err(AdapterError::Io)?;
                sync_directory(parent).await?;
                self.prune_config_backups().await?;
            }
            self.with_effect_store(true, |effect_store| {
                effect_store.mark_applied("config_apply", "ocserv.conf", desired_revision, effect)
            })?;
            return Ok(applied_config(
                candidate_hash,
                expected_current_hash,
                candidate_hash.to_vec(),
                desired_revision,
            ));
        }
        if let Some(path) = backup
            && atomic_replace(&path, &self.resources.config).await.is_ok()
            && self.config_health().await.is_ok()
            && self
                .config_fingerprint()
                .await
                .is_ok_and(|observed| observed.sha256 == hex::encode(expected_current_hash))
        {
            self.with_effect_store(true, |effect_store| {
                effect_store.mark_absent("config_apply", "ocserv.conf", desired_revision, effect)
            })?;
            return Ok(ConfigApplyResult {
                candidate_hash: candidate_hash.to_vec(),
                previous_hash: expected_current_hash.to_vec(),
                observed_hash: expected_current_hash.to_vec(),
                applied_revision: 0,
                healthy: true,
                rolled_back: true,
                failed_critical: false,
                failure_code: "recovered_health_check_failed".to_owned(),
            });
        }
        self.with_effect_store(true, |effect_store| {
            effect_store.mark_absent("config_apply", "ocserv.conf", desired_revision, effect)
        })?;
        Ok(ConfigApplyResult {
            candidate_hash: candidate_hash.to_vec(),
            previous_hash: expected_current_hash.to_vec(),
            observed_hash: Vec::new(),
            applied_revision: 0,
            healthy: false,
            rolled_back: false,
            failed_critical: true,
            failure_code: "recovery_rollback_failed".to_owned(),
        })
    }

    async fn config_health(&self) -> Result<(), AdapterError> {
        self.execute(
            &self.resources.ocserv,
            &[
                "-t",
                "-c",
                self.resources
                    .config
                    .to_str()
                    .ok_or(AdapterError::InvalidResource)?,
            ],
        )
        .await?;
        self.service_reload().await?;
        let status = self.service_status().await?;
        if status.load_state != "loaded"
            || status.active_state != "active"
            || status.sub_state != "running"
        {
            return Err(AdapterError::Unavailable);
        }
        self.session_list().await?;
        Ok(())
    }

    async fn prune_config_backups(&self) -> Result<(), AdapterError> {
        let parent = self
            .resources
            .config
            .parent()
            .ok_or(AdapterError::InvalidResource)?;
        let mut entries = tokio::fs::read_dir(parent)
            .await
            .map_err(AdapterError::Io)?;
        let mut backups = Vec::new();
        while let Some(entry) = entries.next_entry().await.map_err(AdapterError::Io)? {
            let name = entry.file_name();
            if name.to_string_lossy().starts_with(".ocservia-success-") {
                backups.push((name, entry.path()));
            }
        }
        if backups.len() <= 10 {
            return Ok(());
        }
        backups.sort_by(|left, right| left.0.cmp(&right.0));
        let remove = backups.len().saturating_sub(10);
        for (_, path) in backups.into_iter().take(remove) {
            tokio::fs::remove_file(path)
                .await
                .map_err(AdapterError::Io)?;
        }
        sync_directory(parent).await
    }

    async fn find_config_recovery(
        &self,
        expected_hash: &[u8],
    ) -> Result<Option<PathBuf>, AdapterError> {
        let parent = self
            .resources
            .config
            .parent()
            .ok_or(AdapterError::InvalidResource)?;
        let mut entries = tokio::fs::read_dir(parent)
            .await
            .map_err(AdapterError::Io)?;
        let mut backups = Vec::new();
        let mut successes = Vec::new();
        while let Some(entry) = entries.next_entry().await.map_err(AdapterError::Io)? {
            let name = entry.file_name();
            let name = name.to_string_lossy();
            if !name.starts_with(".ocservia-backup-") && !name.starts_with(".ocservia-success-") {
                continue;
            }
            let bytes = tokio::fs::read(entry.path())
                .await
                .map_err(AdapterError::Io)?;
            if bytes.len() <= MAX_CONFIG_BYTES && Sha256::digest(&bytes).as_slice() == expected_hash
            {
                if name.starts_with(".ocservia-backup-") {
                    backups.push(entry.path());
                } else {
                    successes.push(entry.path());
                }
            }
        }
        backups.sort();
        successes.sort();
        Ok(backups.pop().or_else(|| successes.pop()))
    }

    /// Removes only stale configuration-plan files from the fixed config directory.
    ///
    /// # Errors
    ///
    /// Returns an error when the fixed directory cannot be inspected or a matching stale file
    /// cannot be removed.
    pub async fn cleanup_stale_config_plans(&self) -> Result<(), AdapterError> {
        let _guard = self.config_plan_lock.lock().await;
        self.cleanup_stale_config_prefix(".ocservia-plan-", false)
            .await
    }

    /// Removes only uncommitted config-apply staging files after a privd restart.
    ///
    /// # Errors
    ///
    /// Returns a fixed-path I/O error when the config directory cannot be read or a recognized
    /// stale staging file cannot be removed.
    pub async fn cleanup_stale_config_apply_staging(&self) -> Result<(), AdapterError> {
        let _guard = self.config_plan_lock.lock().await;
        self.cleanup_stale_config_prefix(".ocservia-apply-", true)
            .await
    }

    async fn cleanup_stale_config_prefix(
        &self,
        prefix: &str,
        sync_parent: bool,
    ) -> Result<(), AdapterError> {
        let parent = self
            .resources
            .config
            .parent()
            .ok_or(AdapterError::InvalidResource)?;
        let mut entries = tokio::fs::read_dir(parent)
            .await
            .map_err(AdapterError::Io)?;
        while let Some(entry) = entries.next_entry().await.map_err(AdapterError::Io)? {
            let Some(name) = entry.file_name().to_str().map(str::to_owned) else {
                continue;
            };
            let Some(suffix) = name.strip_prefix(prefix) else {
                continue;
            };
            let Ok(id) = Uuid::parse_str(suffix) else {
                continue;
            };
            if id.get_version_num() == 7 {
                tokio::fs::remove_file(entry.path())
                    .await
                    .map_err(AdapterError::Io)?;
            }
        }
        if sync_parent {
            sync_directory(parent).await?;
        }
        Ok(())
    }
}

fn applied_config(
    candidate_hash: &[u8],
    previous_hash: &[u8],
    observed_hash: Vec<u8>,
    desired_revision: u64,
) -> ConfigApplyResult {
    ConfigApplyResult {
        candidate_hash: candidate_hash.to_vec(),
        previous_hash: previous_hash.to_vec(),
        observed_hash,
        applied_revision: desired_revision,
        healthy: true,
        rolled_back: false,
        failed_critical: false,
        failure_code: String::new(),
    }
}
