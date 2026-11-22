use super::{
    Adapter, AdapterError, AsyncReadExt, AsyncWriteExt, Component, DesiredEffectObservation,
    EffectIdentity, File, GroupList, HashMap, HashSet, MAX_MANAGED_RESOURCES, MAX_USER_FILE_BYTES,
    MetadataExt, MutationResult, ObservedGroup, ObservedUser, OsString, Path, PathBuf,
    PrepareEffect, StagingFile, USER_FILE_MODE, UserList, Uuid, Zeroizing, valid_key_id,
    validate_effect_identity, validate_name,
};

impl Adapter {
    /// Lists fixed ocpasswd identities without returning password hashes.
    ///
    /// # Errors
    ///
    /// Returns a stable adapter error for unreadable or malformed data.
    pub async fn user_list(&self) -> Result<UserList, AdapterError> {
        let bytes = read_optional_user_file(&self.resources.user_file).await?;
        parse_user_file(&bytes)
    }

    /// Lists group membership from the group field in the fixed ocpasswd file.
    ///
    /// # Errors
    ///
    /// Returns a stable adapter error for unreadable or malformed data.
    pub async fn group_list(&self) -> Result<GroupList, AdapterError> {
        let bytes = read_optional_user_file(&self.resources.user_file).await?;
        parse_groups_from_user_file(&bytes)
    }

    /// Creates a user only when the authoritative password file has no matching record.
    ///
    /// # Errors
    ///
    /// Rejects invalid inputs and propagates bounded fixed-command failures.
    pub async fn user_create(
        &self,
        username: &str,
        key_id: &str,
        sealed_password: &[u8],
        desired_revision: u64,
        effect: EffectIdentity<'_>,
    ) -> Result<MutationResult, AdapterError> {
        self.user_secret_apply(
            username,
            key_id,
            sealed_password,
            desired_revision,
            effect,
            SecretApplyMode::MustBeAbsent,
        )
        .await
    }

    /// Rotates a password only when the authoritative password file contains the user.
    ///
    /// # Errors
    ///
    /// Rejects missing users and propagates bounded fixed-command failures.
    pub async fn user_password_rotate(
        &self,
        username: &str,
        key_id: &str,
        sealed_password: &[u8],
        desired_revision: u64,
        effect: EffectIdentity<'_>,
    ) -> Result<MutationResult, AdapterError> {
        self.user_secret_apply(
            username,
            key_id,
            sealed_password,
            desired_revision,
            effect,
            SecretApplyMode::MustExist,
        )
        .await
    }

    async fn user_secret_apply(
        &self,
        username: &str,
        key_id: &str,
        sealed_password: &[u8],
        desired_revision: u64,
        effect: EffectIdentity<'_>,
        mode: SecretApplyMode,
    ) -> Result<MutationResult, AdapterError> {
        validate_name(username)?;
        if !valid_key_id(key_id)
            || key_id != self.resources.user_secret_key_id
            || sealed_password.len() < 32
            || sealed_password.len() > 4096
            || desired_revision == 0
        {
            return Err(AdapterError::InvalidRequest);
        }
        let _guard = self.user_file_lock.lock().await;
        let current = read_optional_user_file(&self.resources.user_file).await?;
        let records = parse_secret_user_records(&current)?;
        let metadata = records
            .iter()
            .find(|record| record.username == username)
            .map(|record| (record.groups.clone(), record.hash.starts_with('!')));
        if (mode == SecretApplyMode::MustBeAbsent && metadata.is_some())
            || (mode == SecretApplyMode::MustExist && metadata.is_none())
        {
            return Err(AdapterError::InvalidRequest);
        }
        if mode == SecretApplyMode::MustBeAbsent && records.len() >= MAX_MANAGED_RESOURCES {
            return Err(AdapterError::CapacityExceeded);
        }
        drop(records);
        let key = self
            .resources
            .user_secret_key
            .to_str()
            .ok_or(AdapterError::InvalidResource)?;
        let output = self
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
                sealed_password,
            )
            .await?;
        let mut password = Zeroizing::new(output.stdout);
        if password.is_empty()
            || password.len() > 1024
            || password
                .iter()
                .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
        {
            return Err(AdapterError::MalformedOutput);
        }
        let mut input = Zeroizing::new(Vec::with_capacity(password.len() * 2 + 2));
        input.extend_from_slice(&password);
        input.push(b'\n');
        input.extend_from_slice(&password);
        input.push(b'\n');
        password.fill(0);
        let mut staging = self.stage_user_file(&current).await?;
        let staging_text = staging
            .path()
            .to_str()
            .ok_or(AdapterError::InvalidResource)?
            .to_owned();
        // ocpasswd owns a second file until rename; cancellation must remove it too.
        let _scratch = StagingFile::new(PathBuf::from(format!("{staging_text}.tmp")));
        async {
            let mut args = vec!["-c", staging_text.as_str()];
            if let Some((groups, _)) = metadata.as_ref() {
                args.extend(["-g", groups.as_str()]);
            }
            args.push(username);
            self.execute_with_input(&self.resources.ocpasswd, &args, &input)
                .await?;
            if metadata.as_ref().is_some_and(|(_, disabled)| *disabled) {
                self.execute(
                    &self.resources.ocpasswd,
                    &["-c", staging_text.as_str(), "-l", username],
                )
                .await?;
            }
            self.commit_desired_staging(
                &mut staging,
                &current,
                mode.mutation_kind(),
                username,
                desired_revision,
                effect,
            )
            .await
        }
        .await?;
        Ok(MutationResult { applied: true })
    }

    /// Disables exactly one validated user in the fixed password file.
    ///
    /// # Errors
    ///
    /// Rejects invalid names and propagates bounded fixed-command failures.
    pub async fn user_disable(
        &self,
        username: &str,
        desired_revision: u64,
        effect: EffectIdentity<'_>,
    ) -> Result<MutationResult, AdapterError> {
        self.user_lock_state(username, desired_revision, effect, true)
            .await
    }

    /// Enables exactly one validated user without changing its password or groups.
    ///
    /// # Errors
    ///
    /// Rejects invalid names and propagates bounded fixed-command failures.
    pub async fn user_enable(
        &self,
        username: &str,
        desired_revision: u64,
        effect: EffectIdentity<'_>,
    ) -> Result<MutationResult, AdapterError> {
        self.user_lock_state(username, desired_revision, effect, false)
            .await
    }

    async fn user_lock_state(
        &self,
        username: &str,
        desired_revision: u64,
        effect: EffectIdentity<'_>,
        locked: bool,
    ) -> Result<MutationResult, AdapterError> {
        validate_name(username)?;
        if desired_revision == 0 {
            return Err(AdapterError::InvalidRequest);
        }
        let _guard = self.user_file_lock.lock().await;
        let current = read_optional_user_file(&self.resources.user_file).await?;
        if find_user_metadata(&current, username)?.is_none() {
            return Err(AdapterError::InvalidRequest);
        }
        let mut staging = self.stage_user_file(&current).await?;
        let staging_text = staging
            .path()
            .to_str()
            .ok_or(AdapterError::InvalidResource)?
            .to_owned();
        let _scratch = StagingFile::new(PathBuf::from(format!("{staging_text}.tmp")));
        let action = if locked { "-l" } else { "-u" };
        async {
            self.execute(
                &self.resources.ocpasswd,
                &["-c", staging_text.as_str(), action, username],
            )
            .await?;
            let mutation_kind = if locked {
                "user_disable"
            } else {
                "user_enable"
            };
            self.commit_desired_staging(
                &mut staging,
                &current,
                mutation_kind,
                username,
                desired_revision,
                effect,
            )
            .await
        }
        .await?;
        Ok(MutationResult { applied: true })
    }

    /// Atomically replaces one group across Ocserv's authoritative ocpasswd records.
    ///
    /// # Errors
    ///
    /// Rejects noncanonical inputs and propagates bounded file failures.
    pub async fn group_apply(
        &self,
        group_name: &str,
        members: &[String],
        desired_revision: u64,
        effect: EffectIdentity<'_>,
    ) -> Result<MutationResult, AdapterError> {
        validate_name(group_name)?;
        if members.len() > MAX_MANAGED_RESOURCES || desired_revision == 0 {
            return Err(AdapterError::InvalidRequest);
        }
        let mut previous = "";
        for member in members {
            validate_name(member)?;
            if member.as_str() <= previous {
                return Err(AdapterError::InvalidRequest);
            }
            previous = member;
        }
        let _guard = self.user_file_lock.lock().await;
        let bytes = read_optional_user_file(&self.resources.user_file).await?;
        let records = parse_secret_user_records(&bytes)?;
        let requested: HashSet<&str> = members.iter().map(String::as_str).collect();
        let existing: HashSet<&str> = records
            .iter()
            .map(|record| record.username.as_str())
            .collect();
        if !requested.is_subset(&existing) {
            return Err(AdapterError::InvalidRequest);
        }
        let mut output = Zeroizing::new(Vec::with_capacity(bytes.len().saturating_add(128)));
        for record in records {
            let mut groups: Vec<String> = record
                .groups
                .split(',')
                .filter(|group| {
                    !group.is_empty() && *group != "*" && *group != "x" && *group != group_name
                })
                .map(str::to_owned)
                .collect();
            if requested.contains(record.username.as_str()) {
                groups.push(group_name.to_owned());
            }
            groups.sort();
            groups.dedup();
            let group_field = if groups.is_empty() {
                "*".to_owned()
            } else {
                groups.join(",")
            };
            output.extend_from_slice(record.username.as_bytes());
            output.push(b':');
            output.extend_from_slice(group_field.as_bytes());
            output.push(b':');
            output.extend_from_slice(record.hash.as_bytes());
            output.push(b'\n');
        }
        parse_secret_user_records(&output).map_err(|error| match error {
            AdapterError::OutputLimit => AdapterError::CapacityExceeded,
            other => other,
        })?;
        let mut staging = self.stage_user_file(&output).await?;
        self.commit_desired_staging(
            &mut staging,
            &bytes,
            "group_apply",
            group_name,
            desired_revision,
            effect,
        )
        .await?;
        Ok(MutationResult { applied: true })
    }

    /// Checks the bounded authenticated evidence for one authoritative replacement.
    ///
    /// # Errors
    ///
    /// Rejects unknown mutation kinds or invalid resource identities.
    pub async fn desired_effect_observe(
        &self,
        mutation_kind: &str,
        resource_key: &str,
        desired_revision: u64,
        effect: EffectIdentity<'_>,
    ) -> Result<DesiredEffectObservation, AdapterError> {
        validate_effect_identity(mutation_kind, resource_key, desired_revision)?;
        if mutation_kind == "config_apply" {
            let _guard = self.config_plan_lock.lock().await;
            let current = tokio::fs::read(&self.resources.config)
                .await
                .map_err(AdapterError::Io)?;
            return self.with_effect_store(false, |store| {
                store.observe(
                    mutation_kind,
                    resource_key,
                    desired_revision,
                    effect,
                    &current,
                )
            });
        }
        let _guard = self.user_file_lock.lock().await;
        let current = read_optional_user_file(&self.resources.user_file).await?;
        self.with_effect_store(false, |store| {
            store.observe(
                mutation_kind,
                resource_key,
                desired_revision,
                effect,
                &current,
            )
        })
    }

    pub(super) async fn commit_desired_staging(
        &self,
        staging: &mut StagingFile,
        before: &[u8],
        mutation_kind: &str,
        resource_key: &str,
        desired_revision: u64,
        effect: EffectIdentity<'_>,
    ) -> Result<(), AdapterError> {
        let after = normalize_and_read_user_staging(staging.path()).await?;
        parse_secret_user_records(&after)?;
        let preparation = self.with_effect_store(true, |store| {
            store.prepare(
                mutation_kind,
                resource_key,
                desired_revision,
                effect,
                before,
                &after,
            )
        })?;
        match preparation {
            PrepareEffect::AlreadyApplied => {
                staging.remove().await?;
                return Ok(());
            }
            PrepareEffect::Proceed => {}
        }
        publish_user_staging(staging.path(), &self.resources.user_file)?;
        staging.disarm();
        self.with_effect_store(true, |store| {
            store.mark_applied(mutation_kind, resource_key, desired_revision, effect)
        })
    }

    pub(super) async fn stage_user_file(&self, bytes: &[u8]) -> Result<StagingFile, AdapterError> {
        let path = &self.resources.user_file;
        let parent = path.parent().ok_or(AdapterError::InvalidResource)?;
        let name = path
            .file_name()
            .and_then(|value| value.to_str())
            .ok_or(AdapterError::InvalidResource)?;
        let staging = StagingFile::new(parent.join(format!(".{name}.ocservia-{}", Uuid::now_v7())));
        // Recheck the authoritative target before creating a replacement. A
        // legacy permissive, symlinked, or hard-linked ocpasswd file is never
        // inherited into a new root trust boundary.
        let _ = read_optional_user_file(path).await?;
        let (directory, _, uid, gid) = open_user_directory(path)?;
        let staging_name = staging
            .path()
            .file_name()
            .ok_or(AdapterError::InvalidResource)?;
        let file = rustix::fs::openat(
            &directory,
            staging_name,
            rustix::fs::OFlags::WRONLY
                | rustix::fs::OFlags::CREATE
                | rustix::fs::OFlags::EXCL
                | rustix::fs::OFlags::NOFOLLOW
                | rustix::fs::OFlags::CLOEXEC,
            rustix::fs::Mode::RUSR | rustix::fs::Mode::WUSR,
        )
        .map_err(|error| AdapterError::Io(error.into()))?;
        let file = File::from(file);
        set_authoritative_user_metadata(&file, uid, gid)?;
        let mut file = tokio::fs::File::from_std(file);
        async {
            file.write_all(bytes).await.map_err(AdapterError::Io)?;
            file.sync_all().await.map_err(AdapterError::Io)
        }
        .await?;
        Ok(staging)
    }

    /// Removes only stale staging files created by this adapter.
    ///
    /// # Errors
    ///
    /// Returns an error if the fixed user-file directory cannot be inspected or cleaned.
    pub async fn cleanup_stale_user_staging(&self) -> Result<(), AdapterError> {
        let _guard = self.user_file_lock.lock().await;
        let (directory, _, uid, gid) = open_user_directory(&self.resources.user_file)?;
        let parent = self
            .resources
            .user_file
            .parent()
            .ok_or(AdapterError::InvalidResource)?;
        let name = self
            .resources
            .user_file
            .file_name()
            .and_then(|value| value.to_str())
            .ok_or(AdapterError::InvalidResource)?;
        let prefix = format!(".{name}.ocservia-");
        let mut entries = tokio::fs::read_dir(parent)
            .await
            .map_err(AdapterError::Io)?;
        while let Some(entry) = entries.next_entry().await.map_err(AdapterError::Io)? {
            let Some(candidate) = entry.file_name().to_str().map(str::to_owned) else {
                continue;
            };
            let Some(suffix) = candidate.strip_prefix(&prefix) else {
                continue;
            };
            let suffix = suffix.strip_suffix(".tmp").unwrap_or(suffix);
            let Ok(id) = Uuid::parse_str(suffix) else {
                continue;
            };
            if id.get_version_num() == 7 {
                let stale = rustix::fs::openat(
                    &directory,
                    candidate.as_str(),
                    rustix::fs::OFlags::RDONLY
                        | rustix::fs::OFlags::NOFOLLOW
                        | rustix::fs::OFlags::CLOEXEC,
                    rustix::fs::Mode::empty(),
                )
                .map(File::from)
                .map_err(|error| AdapterError::Io(error.into()))?;
                // A crash can leave the child's group on either .tmp or its
                // renamed staging file, before root:root normalization.
                let expected_gid = if stale.metadata().map_err(AdapterError::Io)?.gid() == gid {
                    gid
                } else {
                    rustix::process::getegid().as_raw()
                };
                validate_authoritative_user_file(&stale, uid, expected_gid)?;
                rustix::fs::unlinkat(&directory, candidate.as_str(), rustix::fs::AtFlags::empty())
                    .map_err(|error| AdapterError::Io(error.into()))?;
            }
        }
        Ok(())
    }
}
/// Parses a bounded ocpasswd file while discarding every hash.
///
/// # Errors
///
/// Rejects malformed lines, unsafe names, and oversized input.
pub fn parse_user_file(bytes: &[u8]) -> Result<UserList, AdapterError> {
    let mut users: Vec<_> = parse_secret_user_records(bytes)?
        .into_iter()
        .map(|record| ObservedUser {
            username: record.username,
            enabled: !record.hash.starts_with('!'),
        })
        .collect();
    users.sort_by(|left, right| left.username.cmp(&right.username));
    Ok(UserList { users })
}

pub(super) fn parse_groups_from_user_file(bytes: &[u8]) -> Result<GroupList, AdapterError> {
    let records = parse_secret_user_records(bytes)?;
    let mut grouped: HashMap<String, Vec<String>> = HashMap::new();
    for record in records {
        for group in record
            .groups
            .split(',')
            .filter(|group| !group.is_empty() && !matches!(*group, "*" | "x"))
        {
            grouped
                .entry(group.to_owned())
                .or_default()
                .push(record.username.clone());
        }
    }
    let mut groups: Vec<_> = grouped
        .into_iter()
        .map(|(group_name, mut members)| {
            members.sort();
            ObservedGroup {
                group_name,
                members,
            }
        })
        .collect();
    groups.sort_by(|left, right| left.group_name.cmp(&right.group_name));
    Ok(GroupList { groups })
}

#[derive(Debug)]
pub(super) struct SecretUserRecord {
    pub(super) username: String,
    pub(super) groups: String,
    pub(super) hash: Zeroizing<String>,
}

pub(super) fn parse_secret_user_records(
    bytes: &[u8],
) -> Result<Vec<SecretUserRecord>, AdapterError> {
    if bytes.len() > MAX_USER_FILE_BYTES {
        return Err(AdapterError::OutputLimit);
    }
    let mut records = Vec::new();
    let mut memberships = 0_usize;
    let mut seen = HashMap::new();
    let text = std::str::from_utf8(bytes).map_err(|_| AdapterError::UserFileFormat {
        reason: "invalid_utf8",
        line: 0,
        fields: 0,
        first_line: None,
    })?;
    for (index, line) in text.lines().enumerate() {
        if line.is_empty() {
            continue;
        }
        let line_number = index + 1;
        let fields = line.split(':').count();
        let malformed = |reason| AdapterError::UserFileFormat {
            reason,
            line: line_number,
            fields,
            first_line: None,
        };
        if records.len() == MAX_MANAGED_RESOURCES {
            return Err(AdapterError::OutputLimit);
        }
        if fields != 3 {
            return Err(malformed("field_count"));
        }
        let mut parts = line.split(':');
        let username = parts.next().ok_or_else(|| malformed("field_count"))?;
        let groups = parts.next().ok_or_else(|| malformed("field_count"))?;
        let hash = parts.next().ok_or_else(|| malformed("field_count"))?;
        if hash.is_empty() {
            return Err(malformed("empty_hash"));
        }
        validate_name(username).map_err(|_| malformed("invalid_username"))?;
        if let Some(first_line) = seen.insert(username, line_number) {
            return Err(AdapterError::UserFileFormat {
                reason: "duplicate_username",
                line: line_number,
                fields,
                first_line: Some(first_line),
            });
        }
        for group in groups.split(',').filter(|group| !group.is_empty()) {
            if matches!(group, "*" | "x") {
                continue;
            }
            validate_name(group).map_err(|_| malformed("invalid_group"))?;
            memberships = memberships.saturating_add(1);
            if memberships > MAX_MANAGED_RESOURCES {
                return Err(AdapterError::OutputLimit);
            }
        }
        records.push(SecretUserRecord {
            username: username.to_owned(),
            groups: groups.to_owned(),
            hash: Zeroizing::new(hash.to_owned()),
        });
    }
    records.sort_by(|left, right| left.username.cmp(&right.username));
    Ok(records)
}

fn find_user_metadata(
    bytes: &[u8],
    username: &str,
) -> Result<Option<(String, bool)>, AdapterError> {
    Ok(parse_secret_user_records(bytes)?
        .into_iter()
        .find(|record| record.username == username)
        .map(|record| (record.groups, record.hash.starts_with('!'))))
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum SecretApplyMode {
    MustBeAbsent,
    MustExist,
}

impl SecretApplyMode {
    const fn mutation_kind(self) -> &'static str {
        match self {
            Self::MustBeAbsent => "user_create",
            Self::MustExist => "user_password_rotate",
        }
    }
}

pub(super) fn authoritative_user_owner() -> (u32, u32) {
    let uid = rustix::process::geteuid().as_raw();
    let gid = if uid == 0 {
        0
    } else {
        rustix::process::getegid().as_raw()
    };
    (uid, gid)
}

fn open_user_directory(path: &Path) -> Result<(File, OsString, u32, u32), AdapterError> {
    let name = path
        .file_name()
        .ok_or(AdapterError::InvalidResource)?
        .to_os_string();
    let (uid, gid) = authoritative_user_owner();
    if uid == 0 && !cfg!(test) {
        if !path.is_absolute() {
            return Err(AdapterError::InvalidResource);
        }
        let mut components = path.components();
        if components.next() != Some(Component::RootDir) {
            return Err(AdapterError::InvalidResource);
        }
        let names = components
            .map(|component| match component {
                Component::Normal(name) => Ok(name),
                _ => Err(AdapterError::InvalidResource),
            })
            .collect::<Result<Vec<_>, _>>()?;
        let (_, directories) = names.split_last().ok_or(AdapterError::InvalidResource)?;
        let root = rustix::fs::open(
            "/",
            rustix::fs::OFlags::RDONLY
                | rustix::fs::OFlags::DIRECTORY
                | rustix::fs::OFlags::CLOEXEC,
            rustix::fs::Mode::empty(),
        )
        .map_err(|error| AdapterError::Io(error.into()))?;
        let mut directory = File::from(root);
        validate_user_directory(&directory, uid)?;
        for component in directories {
            let next = rustix::fs::openat(
                &directory,
                *component,
                rustix::fs::OFlags::RDONLY
                    | rustix::fs::OFlags::DIRECTORY
                    | rustix::fs::OFlags::NOFOLLOW
                    | rustix::fs::OFlags::CLOEXEC,
                rustix::fs::Mode::empty(),
            )
            .map_err(|error| AdapterError::Io(error.into()))?;
            let next = File::from(next);
            validate_user_directory(&next, uid)?;
            directory = next;
        }
        return Ok((directory, name, uid, gid));
    }

    let parent = path.parent().ok_or(AdapterError::InvalidResource)?;
    let directory = rustix::fs::open(
        parent,
        rustix::fs::OFlags::RDONLY
            | rustix::fs::OFlags::DIRECTORY
            | rustix::fs::OFlags::NOFOLLOW
            | rustix::fs::OFlags::CLOEXEC,
        rustix::fs::Mode::empty(),
    )
    .map_err(|error| AdapterError::Io(error.into()))?;
    let directory = File::from(directory);
    validate_user_directory(&directory, uid)?;
    Ok((directory, name, uid, gid))
}

fn validate_user_directory(directory: &File, uid: u32) -> Result<(), AdapterError> {
    let metadata = directory.metadata().map_err(AdapterError::Io)?;
    check_resource(
        "ocpasswd_parent",
        "file_type_directory",
        u64::from(metadata.is_dir()),
        1,
    )?;
    check_resource(
        "ocpasswd_parent",
        "owner_uid",
        u64::from(metadata.uid()),
        u64::from(uid),
    )?;
    check_resource(
        "ocpasswd_parent",
        "writable_mode_bits",
        u64::from(metadata.mode() & 0o022),
        0,
    )?;
    Ok(())
}

fn check_resource(
    resource: &'static str,
    reason: &'static str,
    actual: u64,
    expected: u64,
) -> Result<(), AdapterError> {
    if actual != expected {
        return Err(AdapterError::ResourceMetadata {
            resource,
            reason,
            actual,
            expected,
        });
    }
    Ok(())
}

pub(super) fn validate_authoritative_user_file(
    file: &File,
    uid: u32,
    gid: u32,
) -> Result<std::fs::Metadata, AdapterError> {
    let metadata = file.metadata().map_err(AdapterError::Io)?;
    check_resource(
        "ocpasswd",
        "file_type_regular",
        u64::from(metadata.is_file()),
        1,
    )?;
    check_resource("ocpasswd", "link_count", metadata.nlink(), 1)?;
    check_resource(
        "ocpasswd",
        "owner_uid",
        u64::from(metadata.uid()),
        u64::from(uid),
    )?;
    check_resource(
        "ocpasswd",
        "group_gid",
        u64::from(metadata.gid()),
        u64::from(gid),
    )?;
    check_resource(
        "ocpasswd",
        "mode",
        u64::from(metadata.mode() & 0o777),
        u64::from(USER_FILE_MODE),
    )?;
    Ok(metadata)
}

fn set_authoritative_user_metadata(file: &File, uid: u32, gid: u32) -> Result<(), AdapterError> {
    rustix::fs::fchown(
        file,
        Some(rustix::fs::Uid::from_raw(uid)),
        Some(rustix::fs::Gid::from_raw(gid)),
    )
    .map_err(|error| AdapterError::Io(error.into()))?;
    rustix::fs::fchmod(file, rustix::fs::Mode::RUSR | rustix::fs::Mode::WUSR)
        .map_err(|error| AdapterError::Io(error.into()))
}

pub(super) async fn read_user_file(
    path: &Path,
    optional: bool,
) -> Result<Zeroizing<Vec<u8>>, AdapterError> {
    let (directory, name, uid, gid) = open_user_directory(path)?;
    let file = match rustix::fs::openat(
        &directory,
        &name,
        rustix::fs::OFlags::RDONLY | rustix::fs::OFlags::NOFOLLOW | rustix::fs::OFlags::CLOEXEC,
        rustix::fs::Mode::empty(),
    ) {
        Ok(file) => File::from(file),
        Err(rustix::io::Errno::NOENT) if optional => return Ok(Zeroizing::new(Vec::new())),
        Err(error) => {
            return Err(AdapterError::ResourceIo {
                resource: "ocpasswd",
                reason: if error == rustix::io::Errno::LOOP {
                    "symlink"
                } else {
                    "open_failed"
                },
                source: error.into(),
            });
        }
    };
    validate_authoritative_user_file(&file, uid, gid)?;
    let mut file = tokio::fs::File::from_std(file).take((MAX_USER_FILE_BYTES + 1) as u64);
    let mut value = Zeroizing::new(Vec::new());
    file.read_to_end(&mut value)
        .await
        .map_err(AdapterError::Io)?;
    if value.len() > MAX_USER_FILE_BYTES {
        return Err(AdapterError::OutputLimit);
    }
    Ok(value)
}

pub(super) async fn read_optional_user_file(
    path: &Path,
) -> Result<Zeroizing<Vec<u8>>, AdapterError> {
    read_user_file(path, true).await
}

async fn normalize_and_read_user_staging(path: &Path) -> Result<Zeroizing<Vec<u8>>, AdapterError> {
    let (directory, name, uid, gid) = open_user_directory(path)?;
    let file = rustix::fs::openat(
        &directory,
        &name,
        rustix::fs::OFlags::RDWR | rustix::fs::OFlags::NOFOLLOW | rustix::fs::OFlags::CLOEXEC,
        rustix::fs::Mode::empty(),
    )
    .map_err(|error| AdapterError::Io(error.into()))?;
    let file = File::from(file);
    let metadata = file.metadata().map_err(AdapterError::Io)?;
    if !metadata.is_file() || metadata.nlink() != 1 {
        return Err(AdapterError::InvalidResource);
    }
    set_authoritative_user_metadata(&file, uid, gid)?;
    rustix::fs::fsync(&file).map_err(|error| AdapterError::Io(error.into()))?;
    read_user_file(path, false).await
}

fn publish_user_staging(staging: &Path, target: &Path) -> Result<(), AdapterError> {
    if staging.parent() != target.parent() {
        return Err(AdapterError::InvalidResource);
    }
    let (directory, target_name, uid, gid) = open_user_directory(target)?;
    let staging_name = staging.file_name().ok_or(AdapterError::InvalidResource)?;
    let staging_file = rustix::fs::openat(
        &directory,
        staging_name,
        rustix::fs::OFlags::RDONLY | rustix::fs::OFlags::NOFOLLOW | rustix::fs::OFlags::CLOEXEC,
        rustix::fs::Mode::empty(),
    )
    .map(File::from)
    .map_err(|error| AdapterError::Io(error.into()))?;
    let staging_metadata = validate_authoritative_user_file(&staging_file, uid, gid)?;
    match rustix::fs::openat(
        &directory,
        &target_name,
        rustix::fs::OFlags::RDONLY | rustix::fs::OFlags::NOFOLLOW | rustix::fs::OFlags::CLOEXEC,
        rustix::fs::Mode::empty(),
    ) {
        Ok(file) => {
            validate_authoritative_user_file(&File::from(file), uid, gid)?;
        }
        Err(rustix::io::Errno::NOENT) => {}
        Err(error) => return Err(AdapterError::Io(error.into())),
    }
    rustix::fs::renameat(&directory, staging_name, &directory, &target_name)
        .map_err(|error| AdapterError::Io(error.into()))?;
    let published = rustix::fs::openat(
        &directory,
        &target_name,
        rustix::fs::OFlags::RDONLY | rustix::fs::OFlags::NOFOLLOW | rustix::fs::OFlags::CLOEXEC,
        rustix::fs::Mode::empty(),
    )
    .map(File::from)
    .map_err(|error| AdapterError::Io(error.into()))?;
    let published_metadata = validate_authoritative_user_file(&published, uid, gid)?;
    if published_metadata.dev() != staging_metadata.dev()
        || published_metadata.ino() != staging_metadata.ino()
    {
        return Err(AdapterError::InvalidResource);
    }
    rustix::fs::fsync(&published).map_err(|error| AdapterError::Io(error.into()))?;
    rustix::fs::fsync(&directory).map_err(|error| AdapterError::Io(error.into()))
}
