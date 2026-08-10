use std::env;
use std::io;
use std::path::PathBuf;
use std::time::Duration;

use ocservia_command_authorization::{ControllerCommandKeyring, load_verification_key};
use ocservia_ocserv_adapter::{Adapter, FixedResources, Limits};
use ocservia_privd::{ServerConfig, bind_socket, remove_socket, serve};
use uuid::Uuid;

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
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

async fn shutdown() {
    let _ = tokio::signal::ctrl_c().await;
}

fn parse_args() -> Result<(ServerConfig, FixedResources, Limits), io::Error> {
    parse_args_from(env::args().skip(1))
}

fn parse_args_from(
    mut args: impl Iterator<Item = String>,
) -> Result<(ServerConfig, FixedResources, Limits), io::Error> {
    let mut socket = PathBuf::from("/run/ocserv-platform/privd.sock");
    let mut agent_uid = None;
    let mut node_id = None;
    let mut command_key_files = Vec::new();
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
    Ok((
        ServerConfig {
            socket,
            agent_uid: agent_uid.ok_or_else(|| invalid("--agent-uid is required"))?,
            node_id: node_id.ok_or_else(|| invalid("--node-id is required"))?,
            command_keys,
        },
        FixedResources::default(),
        Limits::default(),
    ))
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
}
