use std::env;
use std::io;
use std::path::PathBuf;
use std::process::ExitCode;

use ocservia_upgrader::UpgradeRunner;

fn main() -> ExitCode {
    // The packaging pipeline verifies the binary's embedded release
    // identity before it is shipped; --version stays read-only.
    if env::args().nth(1).as_deref() == Some("--version") {
        println!(
            "ocservia-upgrader {}",
            ocservia_contracts::agent_upgrade::release_version()
        );
        return ExitCode::SUCCESS;
    }
    let mut args = env::args().skip(1);
    let (operation, root) = match parse_args(&mut args) {
        Ok(parsed) => parsed,
        Err(failure) => {
            eprintln!("ocservia-upgrader: {failure}");
            return ExitCode::from(2);
        }
    };
    if let Err(failure) = ocservia_observability::init("ocservia-upgrader") {
        eprintln!("ocservia-upgrader: observability unavailable: {failure}");
        return ExitCode::from(2);
    }
    let runner = UpgradeRunner::new(root);
    match runner.run(&operation) {
        Ok(state) => {
            println!(
                "ocservia-upgrader: operation {operation} is {}",
                state.as_str()
            );
            ExitCode::SUCCESS
        }
        Err(failure) => {
            eprintln!("ocservia-upgrader: operation {operation} failed: {failure}");
            ExitCode::FAILURE
        }
    }
}

fn parse_args(args: &mut impl Iterator<Item = String>) -> Result<(String, PathBuf), io::Error> {
    let mut operation = None;
    let mut root = PathBuf::from("/");
    while let Some(argument) = args.next() {
        match argument.as_str() {
            "--operation" => {
                operation = Some(required(args, "--operation")?);
            }
            "--root" => {
                root = PathBuf::from(required(args, "--root")?);
            }
            _ => return Err(invalid("unknown upgrader argument")),
        }
    }
    let operation = operation.ok_or_else(|| invalid("--operation is required"))?;
    if !root.is_absolute() {
        return Err(invalid("--root must be absolute"));
    }
    Ok((operation, root))
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
    fn arguments_require_a_canonical_shape() {
        let (operation, root) = parse_args(
            &mut [
                "--operation".to_owned(),
                "018f0c2e-7b1a-7c3d-8e9f-0123456789ab".to_owned(),
            ]
            .into_iter(),
        )
        .expect("default root");
        assert_eq!(operation, "018f0c2e-7b1a-7c3d-8e9f-0123456789ab");
        assert_eq!(root, PathBuf::from("/"));
        let failure = parse_args(&mut [].into_iter()).expect_err("operation is required");
        assert!(failure.to_string().contains("--operation is required"));
        let failure = parse_args(
            &mut [
                "--operation".to_owned(),
                "018f0c2e-7b1a-7c3d-8e9f-0123456789ab".to_owned(),
                "--surprise".to_owned(),
            ]
            .into_iter(),
        )
        .expect_err("unknown argument");
        assert!(failure.to_string().contains("unknown upgrader argument"));
    }
}
