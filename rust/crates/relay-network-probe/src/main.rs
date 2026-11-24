//! Bounded test of authenticated TCP and QUIC address discovery through one Relay.
use std::{
    net::{SocketAddr, ToSocketAddrs},
    path::Path,
    time::Duration,
};

use iroh::{
    Endpoint, NetReportConfig, RelayMap, RelayMode, RelayUrl, SecretKey, Watcher, endpoint::presets,
};
use iroh_relay::quic::{QUIC_ADDR_DISC_CLOSE_CODE, QUIC_ADDR_DISC_CLOSE_REASON, QuicClient};
use rustls_pki_types::{CertificateDer, pem::PemObject};

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    tracing_subscriber::fmt()
        .with_env_filter("warn,iroh::net_report=debug")
        .init();
    let args: Vec<_> = std::env::args().collect();
    let (tcp_only, expected_quic_leaf) = match args.get(4..) {
        Some([]) => (false, None),
        Some([mode]) if mode == "--tcp-only" => (true, None),
        Some([mode, path]) if mode == "--expect-quic-leaf" => (false, Some(path)),
        _ => {
            return Err(
                "relay-network URL CA_FILE TOKEN_FILE [--tcp-only | --expect-quic-leaf PEM]".into(),
            );
        }
    };
    let url: RelayUrl = args[1].parse()?;
    let roots = rustls_pki_types::CertificateDer::pem_file_iter(Path::new(&args[2]))?
        .collect::<Result<Vec<_>, _>>()?;
    let ca = iroh::tls::CaTlsConfig::default().with_extra_roots(roots);
    let token = std::fs::read_to_string(&args[3])?.trim().to_owned();
    let builder = iroh_relay::client::ClientBuilder::new(
        url.clone(),
        SecretKey::generate(),
        iroh::dns::DnsResolver::builder()
            .with_system_defaults()
            .disable_fallback()
            .build(),
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
        builder
            .auth_token("deliberately-wrong-relay-token")
            .connect(),
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
    if tcp_only {
        return Ok(());
    }
    if let Some(path) = expected_quic_leaf {
        let cert = CertificateDer::from_pem_file(path)?;
        verify_quic_leaf(&url, &ca, &cert).await?;
    }
    let endpoint = Endpoint::builder(presets::N0)
        .relay_mode(RelayMode::Custom(
            RelayMap::from_iter([url]).with_auth_token(token),
        ))
        .ca_tls_config(ca)
        .clear_address_lookup()
        .dns_resolver(
            iroh::dns::DnsResolver::builder()
                .with_system_defaults()
                .disable_fallback()
                .build(),
        )
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

async fn verify_quic_leaf(
    url: &RelayUrl,
    ca: &iroh::tls::CaTlsConfig,
    expected: &CertificateDer<'_>,
) -> Result<(), Box<dyn std::error::Error>> {
    let host = url.host_str().ok_or("Relay URL requires a hostname")?;
    let addr = (host, iroh_relay::defaults::DEFAULT_RELAY_QUIC_PORT)
        .to_socket_addrs()?
        .find(SocketAddr::is_ipv4)
        .ok_or("Relay hostname has no IPv4 address")?;
    let endpoint = noq::Endpoint::client("0.0.0.0:0".parse()?)?;
    let client = QuicClient::new(
        endpoint,
        ca.client_config(iroh_relay::tls::default_provider())?,
    );
    let connection =
        tokio::time::timeout(Duration::from_secs(20), client.create_conn(addr, host)).await??;
    let peer = connection
        .peer_identity()
        .and_then(|identity| identity.downcast::<Vec<CertificateDer<'static>>>().ok())
        .ok_or("Relay QUIC peer certificate unavailable")?;
    connection.close(QUIC_ADDR_DISC_CLOSE_CODE, QUIC_ADDR_DISC_CLOSE_REASON);
    if peer.first() != Some(expected) {
        return Err("Relay QUIC peer leaf differs from the HTTPS leaf".into());
    }
    println!("quic_leaf_matches=PASS");
    Ok(())
}
