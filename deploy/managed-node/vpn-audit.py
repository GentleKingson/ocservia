#!/usr/bin/python3 -I
"""Node-local ocserv session accounting and optional conntrack accounting."""
import contextlib
import ipaddress
import json
import os
import pwd
import re
import sqlite3
import subprocess
import sys
import syslog
import time
import uuid

DATABASE = "/var/lib/ocservia-vpn-audit/events.sqlite"
BOOT_ID = "/proc/sys/kernel/random/boot_id"
MAX_PENDING = 16384


def counter(env, name):
    value = env.get(name, "")
    if value.isascii() and value.isdecimal() and len(value) <= 19:
        number = int(value)
        if number <= 2**63 - 1:
            return number
    return None


def database(path):
    db = sqlite3.connect(path, timeout=1.0)
    db.execute("PRAGMA journal_mode=WAL")
    db.execute("PRAGMA max_page_count=16384")
    db.execute("CREATE TABLE IF NOT EXISTS pending (event_id TEXT PRIMARY KEY, body TEXT NOT NULL)")
    db.execute("""CREATE TABLE IF NOT EXISTS leases (
        boot TEXT, session_id TEXT, username TEXT, client_ip TEXT, ipv4 TEXT, ipv6 TEXT,
        connected_ms INTEGER, updated_ms INTEGER, active INTEGER, PRIMARY KEY(boot,session_id))""")
    db.execute("CREATE INDEX IF NOT EXISTS leases_v4 ON leases(boot,ipv4)")
    db.execute("CREATE INDEX IF NOT EXISTS leases_v6 ON leases(boot,ipv6)")
    return db


def enqueue(db, record):
    event_id = str(uuid.uuid4())
    # Producers hold the SQLite writer lock from eviction through insertion.
    # ROWID follows queue order and keeps the cutoff indexed even during outages.
    db.execute("DELETE FROM pending WHERE rowid <= (SELECT max(rowid) FROM pending) - ? + 1",
               (MAX_PENDING,))
    db.execute("INSERT INTO pending VALUES(?,?)", (event_id, json.dumps(record, separators=(",", ":"))))


def ip(value):
    return str(ipaddress.ip_address(value.split("%", 1)[0])) if value else ""


def session_event(db, boot, env, now):
    kind = env.get("REASON")
    if kind not in ("connect", "disconnect"):
        return
    sid, user = env["ID"], env["USERNAME"]
    real, assigned, v6 = ip(env.get("IP_REAL")), ip(env.get("IP_REMOTE")), ip(env.get("IPV6_REMOTE"))
    if not sid or not sid.isprintable() or len(sid.encode()) > 256 or not user or len(user.encode()) > 128 or not user.isprintable() or not real or not (assigned or v6):
        return
    with db:
        db.execute("DELETE FROM leases WHERE boot<>? OR (active=0 AND updated_ms<?)", (boot, now - 120000))
        if kind == "connect":
            started = now
            db.execute("INSERT OR REPLACE INTO leases VALUES(?,?,?,?,?,?,?,?,?)",
                       (boot, sid, user, real, assigned, v6, started, now, 1))
        else:
            row = db.execute("SELECT connected_ms FROM leases WHERE boot=? AND session_id=? AND username=? AND client_ip=?",
                             (boot, sid, user, real)).fetchone()
            duration = counter(env, "STATS_DURATION")
            # The node clock may have stepped backwards since login.
            started = row[0] if row and 0 < row[0] <= now else 0
            if not started and duration is not None:
                started = max(0, now - duration * 1000)
            db.execute("INSERT OR REPLACE INTO leases VALUES(?,?,?,?,?,?,?,?,?)",
                       (boot, sid, user, real, assigned, v6, started, now, 0))
        enqueue(db, dict(event_type=kind, session_id=sid, username=user, client_ip=real,
                         vpn_ip=assigned or v6, connected_ms=started, observed_ms=now,
                         bytes_in=counter(env, "STATS_BYTES_IN"), bytes_out=counter(env, "STATS_BYTES_OUT")))


def flow_event(db, boot, active, line, now):
    # conntrack timestamps events at receipt, before this consumer can queue them.
    stamp = re.match(r"^\[(\d{1,12})\.(\d{1,9})\]\s", line.lstrip())
    if stamp:
        now = int(stamp[1]) * 1000 + int(stamp[2].ljust(3, "0")[:3])
    fields = line.split()
    kind = next((x for x in fields if x in ("[NEW]", "[DESTROY]")), None)
    if not kind:
        return
    attrs = re.findall(r"(?:^|\s)(src|dst|sport|dport|id|bytes|zone)=([^\s]+)", line)
    first = dict(reversed(attrs))
    protocol = next((x for x in fields if x in ("tcp", "udp", "icmp", "icmpv6", "sctp")), None)
    if not protocol or not first.get("src") or not first.get("dst"):
        return
    source, dest = ip(first["src"]), ip(first["dst"])
    key = (first.get("zone"), protocol, source, dest, first.get("sport"), first.get("dport"), first.get("id"))
    if kind == "[NEW]":
        active.pop(key, None)
        rows = db.execute("""SELECT session_id,username,client_ip,connected_ms FROM leases
            WHERE boot=? AND active=1 AND connected_ms<=? AND (ipv4=? OR ipv6=?) LIMIT 2""", (boot, now, source, source)).fetchall()
        if len(rows) == 1 and len(active) < 65536:
            active[key] = (rows[0], now)
        return
    saved = active.pop(key, None)
    if saved:
        (sid, user, client, connected), began = saved
        if now < began:
            return
        counts = [counter({"n": v}, "n") for k, v in attrs if k == "bytes"]
        with db:
            enqueue(db, dict(event_type="flow", session_id=sid, username=user, client_ip=client,
                             vpn_ip=source, destination_ip=dest, destination_port=counter(first, "dport"),
                             protocol=protocol, connected_ms=connected, flow_started_ms=began,
                             observed_ms=now, bytes_in=counts[0] if len(counts) == 2 else None,
                             bytes_out=counts[1] if len(counts) == 2 else None))


def main():
    os.umask(0o077)
    if os.geteuid() == 0:
        account = pwd.getpwnam("ocserv-agent")
        os.setgroups([])
        os.setgid(account.pw_gid)
        os.setuid(account.pw_uid)
    with open(BOOT_ID, encoding="ascii") as source:
        boot = source.read().strip()
    with contextlib.closing(database(DATABASE)) as db:
        if sys.argv[1:] == ["--conntrack"]:
            active = {}
            with subprocess.Popen(["/usr/sbin/conntrack", "-E", "-e", "NEW,DESTROY", "-o", "timestamp,extended,id"],
                                  stdout=subprocess.PIPE, text=True) as process:
                for line in process.stdout:
                    flow_event(db, boot, active, line, time.time_ns() // 1000000)
            raise RuntimeError("conntrack stream closed")
        session_event(db, boot, os.environ, time.time_ns() // 1000000)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        syslog.syslog(syslog.LOG_WARNING, "VPN audit collection failed: " + type(error).__name__)
        sys.exit(1 if sys.argv[1:] == ["--conntrack"] else 0)
