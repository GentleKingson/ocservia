//! Fixed, typed Agent-to-privd protocol over a Unix domain socket.

#![forbid(unsafe_code)]

use std::io;

use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    ArtifactGrantV1, CommandEnvelope,
};
use prost::Message;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};

/// Maximum encoded local RPC frame size.
pub const MAX_FRAME_BYTES: usize = 384 * 1024;

/// Maximum users, groups, members in one group, and aggregate memberships per node.
///
/// This bound keeps every complete supported snapshot and mutation below the
/// fixed local RPC and telemetry frame limits without partial-state semantics.
pub const MAX_MANAGED_RESOURCES: usize = 384;

/// Empty marker used by every fixed read-only request.
#[derive(Clone, Copy, PartialEq, Eq, Message)]
pub struct ReadRequest {}

/// A request from the unprivileged Agent to privd.
#[derive(Clone, PartialEq, Message)]
pub struct PrivdRequest {
    /// `UUIDv7` bytes used to correlate logs and responses.
    #[prost(bytes = "vec", tag = "1")]
    pub request_id: Vec<u8>,
    /// Absolute Unix epoch deadline in milliseconds.
    #[prost(uint64, tag = "2")]
    pub deadline_unix_ms: u64,
    /// Original Controller-signed command. Privd derives every privileged
    /// operation and effect identity from this carrier after independent
    /// signature and semantic-hash verification.
    #[prost(message, optional, tag = "7")]
    pub authorization_command: Option<CommandEnvelope>,
    /// Whether the signed command is being executed or reconciled.
    #[prost(enumeration = "PrivilegedRequestMode", tag = "8")]
    pub privileged_mode: i32,
    /// One of the permanently fixed operations.
    #[prost(
        oneof = "privd_request::Operation",
        tags = "10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40, 41, 42, 43"
    )]
    pub operation: Option<privd_request::Operation>,
}

/// Privileged local-RPC intent. The Controller authorization separately binds
/// command delivery mode, so Agent cannot turn reconciliation into execution.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, PartialOrd, Ord, prost::Enumeration)]
#[repr(i32)]
pub enum PrivilegedRequestMode {
    Unspecified = 0,
    Execute = 1,
    Reconcile = 2,
}

/// Fixed request variants. There is intentionally no raw command or path variant.
pub mod privd_request {
    use prost::Oneof;

    use super::{
        ArtifactConsumeRequest, ArtifactReadRequest, CertificateCsrRequest, CertificateP12Request,
        CertificateRevokeRequest, ConfigApplyRequest, ConfigPlanRequest,
        DesiredEffectObserveRequest, GroupApplyRequest, IpBanRemoveRequest, ReadRequest,
        ServiceReloadRequest, SessionMutationRequest, UserDisableRequest, UserEnableRequest,
        UserSecretRequest,
    };

    /// Read-only operation allowlist.
    #[derive(Clone, PartialEq, Eq, Oneof)]
    pub enum Operation {
        /// Read the fixed ocserv systemd unit state.
        #[prost(message, tag = "10")]
        ServiceStatus(ReadRequest),
        /// Read the installed ocserv version.
        #[prost(message, tag = "11")]
        OcservVersion(ReadRequest),
        /// List current ocserv sessions.
        #[prost(message, tag = "12")]
        SessionList(ReadRequest),
        /// List current ocserv IP bans.
        #[prost(message, tag = "13")]
        IpBanList(ReadRequest),
        /// Fingerprint the fixed ocserv configuration file.
        #[prost(message, tag = "14")]
        ConfigFingerprint(ReadRequest),
        /// List users from the fixed ocpasswd file without hashes.
        #[prost(message, tag = "15")]
        UserList(ReadRequest),
        /// List groups derived from authoritative ocpasswd records.
        #[prost(message, tag = "16")]
        GroupList(ReadRequest),
        /// Check bounded non-secret evidence for one desired-state replacement.
        #[prost(message, tag = "17")]
        DesiredEffectObserve(DesiredEffectObserveRequest),
        /// Validate one candidate in a fixed staging directory without changing current config.
        #[prost(message, tag = "18")]
        ConfigPlan(ConfigPlanRequest),
        /// Read one bounded chunk under a Controller-signed artifact lease.
        #[prost(message, tag = "19")]
        ArtifactRead(ArtifactReadRequest),
        /// Disconnect one numeric session without invalidating its cookie.
        #[prost(message, tag = "30")]
        SessionDisconnect(SessionMutationRequest),
        /// Terminate one numeric session and invalidate its cookie.
        #[prost(message, tag = "31")]
        SessionTerminate(SessionMutationRequest),
        /// Remove one canonical address from the ban list.
        #[prost(message, tag = "32")]
        IpBanRemove(IpBanRemoveRequest),
        /// Reload only the fixed `ocserv.service` unit.
        #[prost(message, tag = "33")]
        ServiceReload(ServiceReloadRequest),
        /// Create a user through the fixed ocpasswd resource.
        #[prost(message, tag = "34")]
        UserCreate(UserSecretRequest),
        /// Disable a user through the fixed ocpasswd resource.
        #[prost(message, tag = "35")]
        UserDisable(UserDisableRequest),
        /// Rotate a user's write-only password.
        #[prost(message, tag = "36")]
        UserPasswordRotate(UserSecretRequest),
        /// Atomically replace one group membership record.
        #[prost(message, tag = "37")]
        GroupApply(GroupApplyRequest),
        /// Enable a user without changing its password or groups.
        #[prost(message, tag = "38")]
        UserEnable(UserEnableRequest),
        /// Atomically apply one approved immutable configuration candidate.
        #[prost(message, tag = "39")]
        ConfigApply(ConfigApplyRequest),
        /// Generate one private key locally and return only its CSR and public-key digest.
        #[prost(message, tag = "40")]
        CertificateCsr(CertificateCsrRequest),
        /// Remove one locally held certificate private key by certificate ID.
        #[prost(message, tag = "41")]
        CertificateRevoke(CertificateRevokeRequest),
        /// Build an encrypted P12 in the fixed artifact spool.
        #[prost(message, tag = "42")]
        CertificateP12(CertificateP12Request),
        /// Durably consume and delete an artifact after successful delivery.
        #[prost(message, tag = "43")]
        ArtifactConsume(ArtifactConsumeRequest),
    }
}

#[derive(Clone, PartialEq, Eq, Message)]
pub struct CertificateCsrRequest {
    #[prost(bytes = "vec", tag = "1")]
    pub certificate_id: Vec<u8>,
    #[prost(string, tag = "2")]
    pub common_name: String,
    #[prost(string, repeated, tag = "3")]
    pub dns_names: Vec<String>,
    #[prost(uint32, tag = "4")]
    pub key_bits: u32,
}

#[derive(Clone, PartialEq, Eq, Message)]
pub struct CertificateCsrResult {
    #[prost(bytes = "vec", tag = "1")]
    pub certificate_id: Vec<u8>,
    #[prost(bytes = "vec", tag = "2")]
    pub csr_der: Vec<u8>,
    #[prost(bytes = "vec", tag = "3")]
    pub public_key_sha256: Vec<u8>,
}

#[derive(Clone, PartialEq, Eq, Message)]
pub struct CertificateRevokeRequest {
    #[prost(bytes = "vec", tag = "1")]
    pub certificate_id: Vec<u8>,
    #[prost(uint64, tag = "2")]
    pub certificate_version: u64,
}

#[derive(Clone, PartialEq, Eq, Message)]
pub struct CertificateRevokeResult {
    #[prost(bytes = "vec", tag = "1")]
    pub certificate_id: Vec<u8>,
    #[prost(bool, tag = "2")]
    pub key_removed: bool,
}

#[derive(Clone, PartialEq, Eq, Message)]
pub struct CertificateP12Request {
    #[prost(bytes = "vec", tag = "1")]
    pub certificate_id: Vec<u8>,
    #[prost(bytes = "vec", tag = "2")]
    pub artifact_id: Vec<u8>,
    #[prost(bytes = "vec", tag = "3")]
    pub certificate_chain_pem: Vec<u8>,
    #[prost(message, optional, tag = "4")]
    pub sealed_password:
        Option<ocservia_contracts::generated::ocserv::platform::agent::v1::SealedSecretV1>,
    #[prost(uint64, tag = "5")]
    pub certificate_version: u64,
    #[prost(bytes = "vec", tag = "6")]
    pub operation_id: Vec<u8>,
    #[prost(int64, tag = "7")]
    pub artifact_expires_at_unix_seconds: i64,
}

#[derive(Clone, PartialEq, Eq, Message)]
pub struct ArtifactReadRequest {
    #[prost(message, optional, tag = "1")]
    pub grant: Option<ArtifactGrantV1>,
    #[prost(uint64, tag = "2")]
    pub offset: u64,
}

#[derive(Clone, PartialEq, Eq, Message)]
pub struct ArtifactConsumeRequest {
    #[prost(message, optional, tag = "1")]
    pub grant: Option<ArtifactGrantV1>,
    #[prost(bytes = "vec", tag = "2")]
    pub sha256: Vec<u8>,
    #[prost(uint64, tag = "3")]
    pub size: u64,
}

#[derive(Clone, PartialEq, Eq, Message)]
pub struct ArtifactData {
    #[prost(bytes = "vec", tag = "1")]
    pub artifact_id: Vec<u8>,
    #[prost(bytes = "vec", tag = "2")]
    pub grant_id: Vec<u8>,
    #[prost(uint64, tag = "3")]
    pub offset: u64,
    #[prost(bytes = "vec", tag = "4")]
    pub data: Vec<u8>,
    #[prost(bool, tag = "5")]
    pub eof: bool,
    #[prost(bytes = "vec", tag = "6")]
    pub sha256: Vec<u8>,
}

#[derive(Clone, PartialEq, Eq, Message)]
pub struct CertificateArtifactResult {
    #[prost(bytes = "vec", tag = "1")]
    pub certificate_id: Vec<u8>,
    #[prost(bytes = "vec", tag = "2")]
    pub artifact_id: Vec<u8>,
    #[prost(bytes = "vec", tag = "3")]
    pub artifact_sha256: Vec<u8>,
    #[prost(uint64, tag = "4")]
    pub artifact_size: u64,
}

/// Side-effect-free candidate validation request. No path is accepted from the caller.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct ConfigPlanRequest {
    #[prost(bytes = "vec", tag = "1")]
    pub candidate: Vec<u8>,
    #[prost(bytes = "vec", tag = "2")]
    pub candidate_hash: Vec<u8>,
}

/// Secret-safe result of validating a staged candidate.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct ConfigPlanResult {
    #[prost(bytes = "vec", tag = "1")]
    pub candidate_hash: Vec<u8>,
    #[prost(string, tag = "2")]
    pub diff_redacted: String,
    #[prost(string, repeated, tag = "3")]
    pub warnings: Vec<String>,
    #[prost(bool, tag = "4")]
    pub current_unchanged: bool,
    #[prost(bool, tag = "5")]
    pub staging_cleaned: bool,
    #[prost(bytes = "vec", tag = "6")]
    pub current_hash: Vec<u8>,
}

/// Approved immutable configuration apply request. No caller-selected path is accepted.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct ConfigApplyRequest {
    #[prost(bytes = "vec", tag = "1")]
    pub candidate: Vec<u8>,
    #[prost(bytes = "vec", tag = "2")]
    pub candidate_hash: Vec<u8>,
    #[prost(bytes = "vec", tag = "3")]
    pub expected_current_hash: Vec<u8>,
    #[prost(uint64, tag = "4")]
    pub desired_revision: u64,
}

/// Secret-safe final transaction outcome.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct ConfigApplyResult {
    #[prost(bytes = "vec", tag = "1")]
    pub candidate_hash: Vec<u8>,
    #[prost(bytes = "vec", tag = "2")]
    pub previous_hash: Vec<u8>,
    #[prost(bytes = "vec", tag = "3")]
    pub observed_hash: Vec<u8>,
    #[prost(uint64, tag = "4")]
    pub applied_revision: u64,
    #[prost(bool, tag = "5")]
    pub healthy: bool,
    #[prost(bool, tag = "6")]
    pub rolled_back: bool,
    #[prost(bool, tag = "7")]
    pub failed_critical: bool,
    #[prost(string, tag = "8")]
    pub failure_code: String,
}

/// One stable numeric session scoped to the current boot.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct SessionMutationRequest {
    #[prost(string, tag = "1")]
    pub session_id: String,
    #[prost(string, tag = "2")]
    pub boot_id: String,
}

/// One canonical IP address to remove from the fixed Ocserv ban list.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct IpBanRemoveRequest {
    #[prost(string, tag = "1")]
    pub ip: String,
}

/// Empty marker for reloading the fixed Ocserv systemd unit.
#[derive(Clone, Copy, PartialEq, Eq, Message)]
pub struct ServiceReloadRequest {}

/// User request carrying only ciphertext sealed for the node's root helper.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct UserSecretRequest {
    #[prost(string, tag = "1")]
    pub username: String,
    #[prost(bytes = "vec", tag = "2")]
    pub sealed_password: Vec<u8>,
    #[prost(string, tag = "3")]
    pub secret_key_id: String,
    #[prost(uint64, tag = "4")]
    pub desired_revision: u64,
}

/// User disable request with no password material.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct UserDisableRequest {
    #[prost(string, tag = "1")]
    pub username: String,
    #[prost(uint64, tag = "2")]
    pub desired_revision: u64,
}

/// User enable request with no password material.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct UserEnableRequest {
    #[prost(string, tag = "1")]
    pub username: String,
    #[prost(uint64, tag = "2")]
    pub desired_revision: u64,
}

/// Canonical group membership replacement.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct GroupApplyRequest {
    #[prost(string, tag = "1")]
    pub group_name: String,
    #[prost(string, repeated, tag = "2")]
    pub members: Vec<String>,
    #[prost(uint64, tag = "3")]
    pub desired_revision: u64,
}

/// Identifies one desired-state effect without carrying password material or hashes.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct DesiredEffectObserveRequest {
    #[prost(string, tag = "1")]
    pub mutation_kind: String,
    #[prost(string, tag = "2")]
    pub resource_key: String,
    #[prost(uint64, tag = "3")]
    pub desired_revision: u64,
}

/// Stable result of independently checking one desired-state effect.
#[derive(Clone, Copy, Debug, PartialEq, Eq, prost::Enumeration)]
#[repr(i32)]
pub enum DesiredEffectState {
    /// No trustworthy conclusion was possible.
    Unspecified = 0,
    /// This exact command and revision was durably applied.
    AppliedExact = 1,
    /// A newer revision of the same mutation kind is authoritative.
    SupersededByNewerRevision = 2,
    /// The store proves that the effect was not applied.
    Absent = 3,
    /// Evidence exists but does not match the current authoritative state.
    Unknown = 4,
}

/// Result of checking the bounded root-owned desired-effect store.
#[derive(Clone, Copy, PartialEq, Eq, Message)]
pub struct DesiredEffectObservation {
    #[prost(enumeration = "DesiredEffectState", tag = "1")]
    pub state: i32,
    /// Latest stored revision for this mutation kind and resource, if any.
    #[prost(uint64, tag = "2")]
    pub observed_revision: u64,
}

/// Observed user without password material.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct ObservedUser {
    #[prost(string, tag = "1")]
    pub username: String,
    #[prost(bool, tag = "2")]
    pub enabled: bool,
}

/// Bounded observed user collection.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct UserList {
    #[prost(message, repeated, tag = "1")]
    pub users: Vec<ObservedUser>,
}

/// Observed group membership.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct ObservedGroup {
    #[prost(string, tag = "1")]
    pub group_name: String,
    #[prost(string, repeated, tag = "2")]
    pub members: Vec<String>,
}

/// Bounded observed group collection.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct GroupList {
    #[prost(message, repeated, tag = "1")]
    pub groups: Vec<ObservedGroup>,
}

/// Bounded acknowledgement for a fixed mutation.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct MutationResult {
    #[prost(bool, tag = "1")]
    pub applied: bool,
}

/// Stable service state DTO.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct ServiceStatus {
    /// systemd load state.
    #[prost(string, tag = "1")]
    pub load_state: String,
    /// systemd active state.
    #[prost(string, tag = "2")]
    pub active_state: String,
    /// systemd sub-state.
    #[prost(string, tag = "3")]
    pub sub_state: String,
}

/// Stable version DTO.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct OcservVersion {
    /// Normalized semantic version text reported by ocserv.
    #[prost(string, tag = "1")]
    pub version: String,
}

/// Stable session DTO with no raw occtl output.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct Session {
    /// Ocserv session identifier.
    #[prost(string, tag = "1")]
    pub id: String,
    /// Authenticated username.
    #[prost(string, tag = "2")]
    pub username: String,
    /// Remote client IP address.
    #[prost(string, tag = "3")]
    pub remote_ip: String,
    /// Assigned VPN IP address.
    #[prost(string, tag = "4")]
    pub vpn_ip: String,
}

/// Stable list of sessions.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct SessionList {
    /// Parsed sessions.
    #[prost(message, repeated, tag = "1")]
    pub sessions: Vec<Session>,
}

/// Stable IP ban DTO.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct IpBan {
    /// Canonical IPv4 or IPv6 address.
    #[prost(string, tag = "1")]
    pub ip: String,
    /// Remaining ban duration in seconds, when reported.
    #[prost(uint64, optional, tag = "2")]
    pub seconds_remaining: Option<u64>,
}

/// Stable list of IP bans.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct IpBanList {
    /// Parsed bans.
    #[prost(message, repeated, tag = "1")]
    pub bans: Vec<IpBan>,
}

/// Fingerprint of the fixed ocserv configuration.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct ConfigFingerprint {
    /// Lowercase SHA-256 digest.
    #[prost(string, tag = "1")]
    pub sha256: String,
    /// Bytes included in the fingerprint.
    #[prost(uint64, tag = "2")]
    pub size_bytes: u64,
}

/// Stable error returned across the privilege boundary.
#[derive(Clone, PartialEq, Eq, Message)]
pub struct PrivdError {
    /// Machine-readable error kind.
    #[prost(enumeration = "ErrorKind", tag = "1")]
    pub kind: i32,
    /// Safe, bounded diagnostic without command output.
    #[prost(string, tag = "2")]
    pub detail: String,
}

/// Error classes understood by the Agent.
#[derive(Clone, Copy, Debug, PartialEq, Eq, prost::Enumeration)]
#[repr(i32)]
pub enum ErrorKind {
    /// Required enum zero value.
    Unspecified = 0,
    /// Request failed validation.
    InvalidRequest = 1,
    /// Caller credentials were refused.
    PermissionDenied = 2,
    /// The request deadline elapsed.
    DeadlineExceeded = 3,
    /// A fixed child process failed.
    CommandFailed = 4,
    /// Child output exceeded its bound.
    OutputLimit = 5,
    /// External output did not match a supported format.
    MalformedOutput = 6,
    /// Fixed local resource was unavailable.
    Unavailable = 7,
    /// A mutation would exceed a bounded authoritative-state capacity.
    CapacityExceeded = 8,
}

/// A response from privd.
#[derive(Clone, PartialEq, Message)]
pub struct PrivdResponse {
    /// Correlates the response to a request.
    #[prost(bytes = "vec", tag = "1")]
    pub request_id: Vec<u8>,
    /// Exactly one stable result or error.
    #[prost(
        oneof = "privd_response::Result",
        tags = "10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25"
    )]
    pub result: Option<privd_response::Result>,
}

/// Fixed response variants.
pub mod privd_response {
    use prost::Oneof;

    use super::{
        ArtifactData, CertificateArtifactResult, CertificateCsrResult, CertificateRevokeResult,
        ConfigApplyResult, ConfigFingerprint, ConfigPlanResult, DesiredEffectObservation,
        GroupList, IpBanList, MutationResult, OcservVersion, PrivdError, ServiceStatus,
        SessionList, UserList,
    };

    /// Result allowlist.
    #[derive(Clone, PartialEq, Eq, Oneof)]
    pub enum Result {
        /// Fixed ocserv systemd state.
        #[prost(message, tag = "10")]
        ServiceStatus(ServiceStatus),
        /// Installed ocserv version.
        #[prost(message, tag = "11")]
        OcservVersion(OcservVersion),
        /// Current sessions.
        #[prost(message, tag = "12")]
        SessionList(SessionList),
        /// Current IP bans.
        #[prost(message, tag = "13")]
        IpBanList(IpBanList),
        /// Configuration fingerprint.
        #[prost(message, tag = "14")]
        ConfigFingerprint(ConfigFingerprint),
        /// Fixed mutation acknowledgement.
        #[prost(message, tag = "15")]
        Mutation(MutationResult),
        /// Observed users without password material.
        #[prost(message, tag = "16")]
        UserList(UserList),
        /// Observed groups.
        #[prost(message, tag = "17")]
        GroupList(GroupList),
        /// Non-secret authoritative desired-effect observation.
        #[prost(message, tag = "18")]
        DesiredEffectObservation(DesiredEffectObservation),
        /// Side-effect-free staged configuration validation result.
        #[prost(message, tag = "19")]
        ConfigPlan(ConfigPlanResult),
        #[prost(message, tag = "21")]
        ConfigApply(ConfigApplyResult),
        #[prost(message, tag = "22")]
        CertificateCsr(CertificateCsrResult),
        #[prost(message, tag = "23")]
        CertificateRevoke(CertificateRevokeResult),
        #[prost(message, tag = "24")]
        CertificateP12(CertificateArtifactResult),
        /// One bounded artifact chunk read by privd under a verified grant.
        #[prost(message, tag = "25")]
        ArtifactData(ArtifactData),
        /// Stable failure.
        #[prost(message, tag = "20")]
        Error(PrivdError),
    }
}

/// Reads one bounded length-delimited protobuf message.
///
/// # Errors
///
/// Returns invalid data for empty, oversized, truncated, or malformed frames.
pub async fn read_frame<M, R>(reader: &mut R) -> Result<M, io::Error>
where
    M: Message + Default,
    R: AsyncRead + Unpin,
{
    let mut length = [0_u8; 4];
    reader.read_exact(&mut length).await?;
    let length = u32::from_be_bytes(length) as usize;
    if length == 0 || length > MAX_FRAME_BYTES {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "local RPC frame size invalid",
        ));
    }
    let mut bytes = vec![0_u8; length];
    reader.read_exact(&mut bytes).await?;
    M::decode(bytes.as_slice())
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "local RPC protobuf invalid"))
}

/// Writes one bounded length-delimited protobuf message.
///
/// # Errors
///
/// Returns invalid data when the encoded message is too large.
pub async fn write_frame<M, W>(writer: &mut W, message: &M) -> Result<(), io::Error>
where
    M: Message,
    W: AsyncWrite + Unpin,
{
    let bytes = message.encode_to_vec();
    if bytes.is_empty() || bytes.len() > MAX_FRAME_BYTES {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "local RPC frame size invalid",
        ));
    }
    let length = u32::try_from(bytes.len())
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "local RPC frame too large"))?;
    writer.write_all(&length.to_be_bytes()).await?;
    writer.write_all(&bytes).await?;
    writer.flush().await
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn frame_round_trip_and_oversize_rejection() {
        let request = PrivdRequest {
            request_id: vec![7; 16],
            deadline_unix_ms: 42,
            authorization_command: None,
            privileged_mode: PrivilegedRequestMode::Unspecified.into(),
            operation: Some(privd_request::Operation::ServiceStatus(ReadRequest {})),
        };
        let mut bytes = Vec::new();
        write_frame(&mut bytes, &request)
            .await
            .expect("encode request");
        let decoded: PrivdRequest = read_frame(&mut bytes.as_slice())
            .await
            .expect("decode request");
        assert_eq!(decoded.request_id, request.request_id);

        let mut oversized = (u32::try_from(MAX_FRAME_BYTES + 1).expect("bounded test"))
            .to_be_bytes()
            .to_vec();
        oversized.extend_from_slice(&[0]);
        assert_eq!(
            read_frame::<PrivdRequest, _>(&mut oversized.as_slice())
                .await
                .expect_err("oversize must fail")
                .kind(),
            io::ErrorKind::InvalidData
        );
    }

    fn maximum_name(prefix: &str, index: usize) -> String {
        format!("{prefix}{index:06}{}", "x".repeat(64 - prefix.len() - 6))
    }

    #[tokio::test]
    async fn maximum_supported_user_group_frames_round_trip() {
        let members = (0..MAX_MANAGED_RESOURCES)
            .map(|index| maximum_name("m", index))
            .collect::<Vec<_>>();
        let request = PrivdRequest {
            request_id: vec![7; 16],
            deadline_unix_ms: u64::MAX,
            authorization_command: Some(CommandEnvelope {
                payload: Some(
                    ocservia_contracts::generated::ocserv::platform::agent::v1::command_envelope::Payload::GroupApply(
                        ocservia_contracts::generated::ocserv::platform::agent::v1::GroupApply {
                            group_name: maximum_name("g", 0),
                            members,
                            desired_revision: u64::MAX,
                        },
                    ),
                ),
                ..CommandEnvelope::default()
            }),
            privileged_mode: PrivilegedRequestMode::Execute.into(),
            operation: None,
        };
        let (mut request_writer, mut request_reader) =
            tokio::net::UnixStream::pair().expect("pair");
        let (write_result, read_result) = tokio::join!(
            write_frame(&mut request_writer, &request),
            read_frame::<PrivdRequest, _>(&mut request_reader)
        );
        write_result.expect("maximum group apply frame");
        let decoded = read_result.expect("decode maximum group apply");
        assert_eq!(decoded, request);

        let users = PrivdResponse {
            request_id: vec![7; 16],
            result: Some(privd_response::Result::UserList(UserList {
                users: (0..MAX_MANAGED_RESOURCES)
                    .map(|index| ObservedUser {
                        username: maximum_name("u", index),
                        enabled: true,
                    })
                    .collect(),
            })),
        };
        let (mut user_writer, mut user_reader) = tokio::net::UnixStream::pair().expect("pair");
        let (write_result, read_result) = tokio::join!(
            write_frame(&mut user_writer, &users),
            read_frame::<PrivdResponse, _>(&mut user_reader)
        );
        write_result.expect("maximum user list frame");
        let decoded = read_result.expect("decode maximum user list");
        assert_eq!(decoded, users);

        let groups = PrivdResponse {
            request_id: vec![7; 16],
            result: Some(privd_response::Result::GroupList(GroupList {
                groups: (0..MAX_MANAGED_RESOURCES)
                    .map(|index| ObservedGroup {
                        group_name: maximum_name("g", index),
                        members: vec![maximum_name("u", index)],
                    })
                    .collect(),
            })),
        };
        let (mut group_writer, mut group_reader) = tokio::net::UnixStream::pair().expect("pair");
        let (write_result, read_result) = tokio::join!(
            write_frame(&mut group_writer, &groups),
            read_frame::<PrivdResponse, _>(&mut group_reader)
        );
        write_result.expect("maximum group list frame");
        let decoded = read_result.expect("decode maximum group list");
        assert_eq!(decoded, groups);
    }
}
