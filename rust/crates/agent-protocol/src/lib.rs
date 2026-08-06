//! Fixed, typed Agent-to-privd protocol over a Unix domain socket.

#![forbid(unsafe_code)]

use std::io;

use prost::Message;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};

/// Maximum encoded local RPC frame size.
pub const MAX_FRAME_BYTES: usize = 64 * 1024;

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
    /// One of the permanently fixed operations.
    #[prost(
        oneof = "privd_request::Operation",
        tags = "10, 11, 12, 13, 14, 15, 16, 30, 31, 32, 33, 34, 35, 36, 37"
    )]
    pub operation: Option<privd_request::Operation>,
}

/// Fixed request variants. There is intentionally no raw command or path variant.
pub mod privd_request {
    use prost::Oneof;

    use super::{
        GroupApplyRequest, IpBanRemoveRequest, ReadRequest, ServiceReloadRequest,
        SessionMutationRequest, UserDisableRequest, UserSecretRequest,
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
        /// List groups from the fixed group file.
        #[prost(message, tag = "16")]
        GroupList(ReadRequest),
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
    }
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
        tags = "10, 11, 12, 13, 14, 15, 16, 17, 20"
    )]
    pub result: Option<privd_response::Result>,
}

/// Fixed response variants.
pub mod privd_response {
    use prost::Oneof;

    use super::{
        ConfigFingerprint, GroupList, IpBanList, MutationResult, OcservVersion, PrivdError,
        ServiceStatus, SessionList, UserList,
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
}
