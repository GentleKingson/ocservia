//! Generated controller-to-transport and controller-to-agent contracts.
//!
//! Files below `generated` are replaced by `make generate` and must not be
//! edited by hand.

#![forbid(unsafe_code)]

/// Generated Protocol Buffer types.
#[allow(clippy::all, clippy::pedantic)]
pub mod generated;

pub mod agent_upgrade;
pub mod config_profile;
#[cfg(feature = "relay")]
pub mod relay;
pub mod session;
pub mod strict_wire;
pub mod traceparent;
#[cfg(feature = "transport")]
pub mod uds;
pub mod unix;

pub use strict_wire::{
    StrictWireError, decode_strict_command_envelope, validate_strict_command_envelope,
};
