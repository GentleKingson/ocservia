use crate::session::exchange_message;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    SessionObservation, VpnAuditBatch, VpnAuditReceipt, VpnAuditRecord,
};
use rusqlite::{Connection, OpenFlags};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

type Error = Box<dyn std::error::Error + Send + Sync>;
const DB: &str = "/var/lib/ocservia-vpn-audit/events.sqlite";

fn open_spool(path: &str, flags: OpenFlags) -> rusqlite::Result<Connection> {
    let db = Connection::open_with_flags(path, flags)?;
    db.busy_timeout(Duration::from_secs(1))?;
    Ok(db)
}

fn pending(path: &str) -> Result<Vec<VpnAuditRecord>, Error> {
    if !std::path::Path::new(path).try_exists()? {
        return Ok(Vec::new());
    }
    let db = open_spool(path, OpenFlags::SQLITE_OPEN_READ_ONLY)?;
    let mut stmt=db.prepare("SELECT event_id,
      body->>'event_type',body->>'session_id',body->>'username',body->>'client_ip',body->>'vpn_ip',
      body->>'connected_ms',body->>'observed_ms',body->>'bytes_in',body->>'bytes_out',
      coalesce(body->>'destination_ip',''),body->>'destination_port',coalesce(body->>'protocol',''),coalesce(body->>'flow_started_ms',0)
      FROM pending ORDER BY rowid LIMIT 32")?;
    let items = stmt
        .query_map([], |row| {
            Ok(VpnAuditRecord {
                event_id: row.get(0)?,
                event_type: row.get(1)?,
                session_id: row.get(2)?,
                username: row.get(3)?,
                client_ip: row.get(4)?,
                vpn_ip: row.get(5)?,
                connected_ms: row.get(6)?,
                observed_ms: row.get(7)?,
                bytes_in: row.get(8)?,
                bytes_out: row.get(9)?,
                destination_ip: row.get(10)?,
                destination_port: row.get(11)?,
                protocol: row.get(12)?,
                flow_started_ms: row.get(13)?,
            })
        })?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    Ok(items)
}

// A hook that ran after occtl started is newer evidence than this snapshot.
// Tombstones prevent an in-flight snapshot from resurrecting a disconnected user.
pub(super) async fn reconcile(
    boot: String,
    sampled: SystemTime,
    sessions: Vec<SessionObservation>,
) {
    let result =
        tokio::task::spawn_blocking(move || reconcile_snapshot(DB, &boot, sampled, &sessions))
            .await;
    if !matches!(result, Ok(Ok(()))) {
        tracing::warn!("VPN audit lease reconciliation deferred");
    }
}

fn reconcile_snapshot(
    path: &str,
    boot: &str,
    sampled: SystemTime,
    sessions: &[SessionObservation],
) -> Result<(), Error> {
    if !std::path::Path::new(path).try_exists()? {
        return Ok(());
    }
    let at = i64::try_from(sampled.duration_since(UNIX_EPOCH)?.as_millis())?;
    let mut db = open_spool(path, OpenFlags::SQLITE_OPEN_READ_WRITE)?;
    let tx = db.transaction()?;
    for s in sessions {
        let Some(start) = s.connected_at else {
            continue;
        };
        let started = start.seconds * 1000 + i64::from(start.nanos) / 1_000_000;
        tx.execute("INSERT INTO leases(boot,session_id,username,client_ip,ipv4,ipv6,connected_ms,updated_ms,active)
                VALUES(?1,?2,?3,?4,?5,'',?6,?7,1)
                ON CONFLICT(boot,session_id) DO UPDATE SET
                connected_ms=CASE WHEN leases.active=1 AND leases.username=excluded.username AND leases.client_ip=excluded.client_ip
                  AND excluded.ipv4 IN(leases.ipv4,leases.ipv6) THEN leases.connected_ms ELSE excluded.connected_ms END,
                ipv6=CASE WHEN leases.active=1 AND leases.username=excluded.username AND leases.client_ip=excluded.client_ip
                  AND excluded.ipv4=leases.ipv4 THEN leases.ipv6 ELSE '' END,
                username=excluded.username,client_ip=excluded.client_ip,ipv4=excluded.ipv4,updated_ms=excluded.updated_ms,active=1
                WHERE leases.updated_ms<excluded.updated_ms",
                rusqlite::params![boot,s.session_id,s.username,s.client_ip,s.vpn_ip,started,at])?;
    }
    tx.execute(
        "DELETE FROM leases WHERE boot=?1 AND updated_ms<?2",
        rusqlite::params![boot, at],
    )?;
    tx.commit()?;
    Ok(())
}

fn acknowledge(path: &str, ids: &[String], receipt: &VpnAuditReceipt) -> Result<(), Error> {
    if receipt.event_ids != ids {
        return Err("unexpected VPN audit receipt".into());
    }
    let mut db = open_spool(path, OpenFlags::SQLITE_OPEN_READ_WRITE)?;
    let tx = db.transaction()?;
    for id in ids {
        tx.execute("DELETE FROM pending WHERE event_id=?1", [id])?;
    }
    tx.commit()?;
    Ok(())
}

async fn upload(connection: &iroh::endpoint::Connection) -> Result<bool, Error> {
    let records = tokio::task::spawn_blocking(|| pending(DB)).await??;
    if records.is_empty() {
        return Ok(false);
    }
    let ids: Vec<_> = records.iter().map(|r| r.event_id.clone()).collect();
    let receipt: VpnAuditReceipt =
        exchange_message(connection, &VpnAuditBatch { records }, "VPN audit").await?;
    tokio::task::spawn_blocking(move || acknowledge(DB, &ids, &receipt)).await??;
    Ok(true)
}

pub(super) async fn upload_loop(connection: iroh::endpoint::Connection) {
    loop {
        let delay = match tokio::time::timeout(Duration::from_secs(10), upload(&connection)).await {
            Ok(Ok(true)) => Duration::from_millis(100),
            Ok(Ok(false)) => Duration::from_secs(5),
            _ => {
                tracing::warn!("VPN audit delivery deferred");
                Duration::from_secs(15)
            }
        };
        tokio::select! {()=tokio::time::sleep(delay)=>{},_=connection.closed()=>return}
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use ocservia_test_support::PrivateTempDir;

    #[test]
    fn lease_reconciliation_waits_for_the_wal_writer() {
        let dir = PrivateTempDir::new("agt-aud");
        let path = dir.join("audit.sqlite");
        let mut writer = Connection::open(&path).expect("open writer");
        writer
            .execute_batch(
                "PRAGMA journal_mode=WAL; CREATE TABLE leases(boot TEXT, updated_ms INTEGER);",
            )
            .expect("WAL schema");
        let tx = writer
            .transaction_with_behavior(rusqlite::TransactionBehavior::Immediate)
            .expect("hold writer lock");
        tx.commit().expect("release writer lock");
        reconcile_snapshot(path.to_str().expect("path"), "boot", SystemTime::now(), &[])
            .expect("reconcile");
    }

    #[test]
    fn spool_survives_reopen_and_requires_exact_commit_receipt() {
        let dir = PrivateTempDir::new("agt-aud-sp");
        let path_buf = dir.join("audit.sqlite");
        let path = path_buf.to_str().unwrap();
        let db = Connection::open(path).unwrap();
        db.execute_batch(r#"PRAGMA journal_mode=WAL;
            CREATE TABLE pending(event_id TEXT PRIMARY KEY,body TEXT);
            INSERT INTO pending VALUES('id-1','{"event_type":"connect","session_id":"42","username":"alice","client_ip":"192.0.2.1","vpn_ip":"10.1.0.1","connected_ms":10,"observed_ms":10}');"#).unwrap();
        drop(db);
        let saved = pending(path).unwrap();
        let ids = vec![saved[0].event_id.clone()];
        assert!(acknowledge(path, &ids, &VpnAuditReceipt::default()).is_err());
        assert_eq!(pending(path).unwrap(), saved);
        acknowledge(
            path,
            &ids,
            &VpnAuditReceipt {
                event_ids: ids.clone(),
            },
        )
        .unwrap();
        assert!(pending(path).unwrap().is_empty());
    }
}
