use std::env;
use std::io;
use std::path::PathBuf;

use ocservia_ocserv_adapter::{Adapter, FixedResources, Limits};
use ocservia_privd::{ServerConfig, bind_socket, remove_socket, serve};

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    ocservia_observability::init("ocservia-privd")?;
    let (config, resources, limits) = parse_args()?;
    let adapter = Adapter::new(resources, limits);
    adapter.cleanup_stale_user_staging().await?;
    let listener = bind_socket(&config)?;
    tracing::info!(socket = %config.socket.display(), agent_uid = config.agent_uid, "privd serving on AF_UNIX");
    let result = serve(listener, config.clone(), adapter, shutdown()).await;
    let cleanup = remove_socket(&config.socket);
    result?;
    cleanup?;
    Ok(())
}

async fn shutdown() {
    let _ = tokio::signal::ctrl_c().await;
}

fn parse_args() -> Result<(ServerConfig, FixedResources, Limits), io::Error> {
    let mut socket = PathBuf::from("/run/ocserv-platform/privd.sock");
    let mut agent_uid = None;
    let mut args = env::args().skip(1);
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
            _ => return Err(invalid("unknown privd argument")),
        }
    }
    Ok((
        ServerConfig {
            socket,
            agent_uid: agent_uid.ok_or_else(|| invalid("--agent-uid is required"))?,
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
