//! Bounded test of authenticated TCP and QUIC address discovery through one Relay.
use std::{path::Path, time::Duration};

use iroh::{
    Endpoint, NetReportConfig, RelayMap, RelayMode, RelayUrl, SecretKey, Watcher, endpoint::presets,
};
use rustls_pki_types::pem::PemObject;

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    tracing_subscriber::fmt()
        .with_env_filter("warn,iroh::net_report=debug")
        .init();
    let args: Vec<_> = std::env::args().collect();
    assert_eq!(args.len(), 4, "relay-network URL CA_FILE TOKEN_FILE");
    let url: RelayUrl = args[1].parse()?;
    let roots = rustls_pki_types::CertificateDer::pem_file_iter(Path::new(&args[2]))?
        .collect::<Result<Vec<_>, _>>()?;
    let ca = iroh::tls::CaTlsConfig::default().with_extra_roots(roots);
    let token = std::fs::read_to_string(&args[3])?.trim().to_owned();
    let builder = iroh_relay::client::ClientBuilder::new(
        url.clone(),
        SecretKey::from_bytes(&[71; 32]),
        iroh::dns::DnsResolver::default(),
    )
    .tls_client_config(ca.client_config(iroh_relay::tls::default_provider())?);
    let client = tokio::time::timeout(
        Duration::from_secs(20),
        builder.clone().auth_token(&token).connect(),
    )
    .await??;
    drop(client);
    println!("authenticated_tcp=PASS");
    let rejected = tokio::time::timeout(
        Duration::from_secs(20),
        builder.auth_token("p1-deliberately-wrong-token").connect(),
    )
    .await?;
    let error = rejected.expect_err("wrong token unexpectedly accepted");
    assert!(
        matches!(&error, iroh_relay::client::ConnectError::Handshake {
            source: iroh_relay::protos::handshake::Error::ServerDeniedAuth { reason, .. }, ..
        } if reason == "not authorized"),
        "not an authentication rejection: {error:?}"
    );
    println!("wrong_token=PASS error={error:?}");
    let endpoint = Endpoint::builder(presets::N0)
        .relay_mode(RelayMode::Custom(
            RelayMap::from_iter([url]).with_auth_token(token),
        ))
        .ca_tls_config(ca)
        .net_report_config(NetReportConfig::minimal())
        .bind()
        .await?;
    let report =
        tokio::time::timeout(Duration::from_secs(25), endpoint.net_report().initialized()).await?;
    println!("quic_report={report:?}");
    assert!(
        report.global_v4.is_some(),
        "QUIC discovery returned no IPv4 address"
    );
    println!("quic_global_v4={}", report.global_v4.unwrap());
    endpoint.close().await;
    println!("quic_address_discovery=PASS");
    Ok(())
}
