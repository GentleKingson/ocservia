#!/usr/bin/python3
"""Bounded, independent retention for sealed local binding epochs."""
from contextlib import closing
import fcntl
import hashlib
import importlib.machinery
import importlib.util
import os
from pathlib import Path
import pwd
import sqlite3
import stat
import subprocess
import sys
import time
import uuid

# Both files are installed together in the root-owned libexec directory.
helper = Path(__file__).with_name('rebind-agent.py' if __file__.endswith('.py') else 'ocservia-agent-rebind')
spec = importlib.util.spec_from_loader('rebind', importlib.machinery.SourceFileLoader('rebind', str(helper)))
r = importlib.util.module_from_spec(spec)
spec.loader.exec_module(r)
BATCH = 32


def policy():
    values = {'OCSERV_AGENT_RETIRED_BINDING_RETENTION_DAYS': 30,
              'OCSERV_REBIND_HISTORY_RETENTION_DAYS': 365}
    try:
        raw = r.secure_read(r.CONF / 'retention.env').decode()
    except FileNotFoundError:
        return tuple(values.values())
    seen = set()
    for line in raw.splitlines():
        if not line.strip() or line.lstrip().startswith('#'):
            continue
        key, separator, value = line.partition('=')
        r.require(separator and key in values and key not in seen and value.isascii() and value.isdecimal(),
                  'invalid retention assignment')
        values[key] = int(value)
        seen.add(key)
    local, history = values.values()
    r.require(7 <= local <= 180 and 90 <= history <= 2555, 'retention days outside permitted range')
    return local, history


def compact_journal(path, node, controller, legacy, now):
    """Runs only as the Agent, never root on Agent-controlled SQLite paths."""
    r.require(os.geteuid() != 0, 'journal compaction must be unprivileged')
    path = Path(path)
    parent = r.directory_handle(path.parent, os.geteuid())
    try:
        meta = os.stat(path.name, dir_fd=parent, follow_symlinks=False)
        r.require(stat.S_ISREG(meta.st_mode) and meta.st_uid == os.geteuid() and meta.st_nlink == 1
                  and not meta.st_mode & 0o077, 'unsafe retired journal')
        # Pin the parent. SQLite and its sidecars stay within this unprivileged
        # process even if an adversarial Agent substitutes a pathname.
        database_path = Path('/proc/self/fd') / str(parent) / path.name
        with closing(sqlite3.connect(database_path.as_uri() + '?mode=rw', uri=True, timeout=5)) as db:
            db.execute('PRAGMA trusted_schema=OFF')
            db.execute('PRAGMA synchronous=FULL')
            db.set_progress_handler(lambda: 1, 1000000)
            db.execute('BEGIN IMMEDIATE')
            binding = uuid.UUID(node).bytes + bytes.fromhex(controller)
            stored = db.execute("SELECT value FROM agent_metadata WHERE key='controller_binding'").fetchone()
            r.require((stored and stored[0] == binding) or (not stored and legacy), 'retired journal authority mismatch')
            r.require(not db.execute("SELECT EXISTS(SELECT 1 FROM command_journal WHERE state NOT IN ('succeeded','failed') OR error_code LIKE '%reconciliation%')").fetchone()[0],
                      'retired journal has unresolved work')
            # Refuse unknown schema extensions rather than erase recovery data
            # introduced by a newer executable.
            tables = {row[0] for row in db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")}
            r.require(tables <= {'agent_metadata', 'command_journal', 'synthetic_effects', 'synthetic_effect_counter',
                                'telemetry_buffer', 'telemetry_drop_counters', 'applied_resource_revisions', 'retired_command_details'},
                      'unknown journal schema requires retention upgrade')
            db.execute('CREATE TABLE IF NOT EXISTS retired_command_details(idempotency_key BLOB PRIMARY KEY, result_sha256 BLOB NOT NULL, synthetic_sha256 BLOB, compacted_at INTEGER NOT NULL) STRICT')
            db.execute("INSERT INTO agent_metadata(key,value) VALUES('retired_binding',?) ON CONFLICT(key) DO NOTHING", (binding,))
            rows = db.execute("SELECT j.idempotency_key,j.result,s.result FROM command_journal j LEFT JOIN synthetic_effects s ON s.idempotency_key=j.idempotency_key WHERE j.result IS NOT NULL AND j.privileged_result_proof IS NULL AND NOT EXISTS(SELECT 1 FROM retired_command_details t WHERE t.idempotency_key=j.idempotency_key) ORDER BY j.updated_at,j.idempotency_key LIMIT ?", (BATCH,)).fetchall()
            for key, result, synthetic in rows:
                db.execute('INSERT INTO retired_command_details VALUES(?,?,?,?)',
                           (key, hashlib.sha256(result).digest(), hashlib.sha256(synthetic).digest() if synthetic is not None else None, now))
                db.execute('UPDATE command_journal SET result=NULL WHERE idempotency_key=?', (key,))
                if synthetic is not None:
                    db.execute("UPDATE synthetic_effects SET result=x'' WHERE idempotency_key=?", (key,))
            remaining = db.execute('SELECT EXISTS(SELECT 1 FROM command_journal WHERE result IS NOT NULL AND privileged_result_proof IS NULL)').fetchone()[0]
            db.commit()
            return not remaining
    finally:
        os.close(parent)


def source_paths(source):
    legacy = source['identity'] == str(r.AGENT_STATE / 'identity')
    base = r.AGENT_STATE if legacy else r.AGENT_STATE / 'bindings' / r.node_id(source['node'])
    effects = r.PRIVD_STATE if legacy else r.PRIVD_STATE / 'bindings' / source['node']
    r.require(source['identity'] == str(base / 'identity') and source['journal'] == str(base / 'agent.db')
              and source['effects'] == str(effects / 'desired-effects.sqlite3'), 'nonstandard retired state paths')
    return legacy


def root_unresolved(source):
    path = Path(source['effects'])
    if not path.exists():
        return True  # Missing privileged evidence cannot prove safe retirement.
    fd = r.directory_handle(path.parent)
    try:
        meta = os.stat(path.name, dir_fd=fd, follow_symlinks=False)
        r.require(stat.S_ISREG(meta.st_mode) and meta.st_uid == 0 and meta.st_nlink == 1
                  and not meta.st_mode & 0o077, 'unsafe privileged recovery evidence')
        if r.unresolved(path, ["SELECT EXISTS(SELECT 1 FROM authorized_effects WHERE state NOT IN ('applied'))",
                              "SELECT EXISTS(SELECT 1 FROM desired_effects WHERE state NOT IN ('applied','absent'))"], True):
            return True
        with closing(sqlite3.connect(path.as_uri() + '?mode=ro', uri=True, timeout=5)) as db:
            db.execute('PRAGMA trusted_schema=OFF')
            db.execute('PRAGMA query_only=ON')
            db.set_progress_handler(lambda: 1, 1000000)
            if db.execute("SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='certificate_artifacts')").fetchone()[0]:
                if db.execute("SELECT EXISTS(SELECT 1 FROM certificate_artifacts WHERE state IN ('prepared','leased'))").fetchone()[0]:
                    return True
    finally:
        os.close(fd)
    return r.unresolved_upgrades(source)


def retain_one(operation, state, active, account, local_days, history_days, now):
    source = state['source']
    if (state['phase'] != 'verified' or state.get('mutations_blocked', True) or source['blocked']
            or source['node'] == active['node'] or source['endpoint'] != active['endpoint']
            or state.get('verified_at', now) > now - local_days * 86400):
        return
    legacy = source_paths(source)
    if root_unresolved(source):
        return
    # A crash before the state update repeats the same SQLite transaction safely.
    result = subprocess.run(['/usr/bin/python3', str(Path(__file__).resolve()), '--compact-journal',
                             source['journal'], source['node'], source['controller'], str(int(legacy)), str(now)],
                            user=account.pw_uid, group=account.pw_gid, extra_groups=[],
                            env={'PATH': '/usr/bin:/bin'}, check=True, capture_output=True, timeout=15)
    r.require(result.stdout in (b'complete\n', b'partial\n'), 'invalid journal compaction result')
    state['source_state'] = 'compacted' if result.stdout == b'complete\n' else 'compacting'
    state.setdefault('retired_compacted_at', now)
    if state['source_state'] == 'compacted' and state['verified_at'] <= now - history_days * 86400:
        # Keep the authority transition and disposition forever. Only enrollment
        # credentials and human/detail fields expire; state remains resumable.
        token = operation / 'token'
        try:
            r.secure_read(token, 4096)
            token.unlink()
            r.sync_directory(operation)
        except FileNotFoundError:
            pass
        for field in ('reason', 'environment', 'token_hash', 'created_at', 'enrolled_at', 'commit_started_at'):
            state.pop(field, None)
        state.setdefault('details_compacted_at', now)
    r.save(operation, state)


def run():
    r.require(os.geteuid() == 0, 'retention coordinator must run as root')
    local_days, history_days = policy()  # invalid config fails even without state
    if not r.STATE.exists():
        return
    account = pwd.getpwnam('ocserv-agent')
    r.require(account.pw_uid != 0, 'Agent account must be unprivileged')
    fd = r.directory_handle(r.STATE)
    os.close(fd)
    fd = r.directory_handle(r.UPGRADE_STATE)
    os.close(fd)
    lock_fd = os.open(r.UPGRADE_STATE / '.binding-lifecycle.lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    with os.fdopen(lock_fd, 'wb') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        active = r.decode_binding(r.secure_read(r.ACTIVE, 1024))
        cursor_path = r.STATE / '.retention-cursor'
        try:
            cursor = r.secure_read(cursor_path, 128).decode().strip()
        except FileNotFoundError:
            cursor = ''
        names = sorted(path.name for path in r.STATE.iterdir() if not path.name.startswith('.'))
        candidates = [name for name in names if name > cursor][:BATCH]
        if not candidates:
            candidates = names[:BATCH]
        errors = []
        for name in candidates:
            try:
                operation, state = r.read_operation(name)
                retain_one(operation, state, active, account, local_days, history_days, int(time.time()))
            except (OSError, RuntimeError, ValueError, KeyError, sqlite3.Error, subprocess.SubprocessError) as error:
                errors.append(name + ': ' + str(error))
        if candidates:
            r.atomic_write(cursor_path, (candidates[-1] + '\n').encode())
        r.require(not errors, '; '.join(errors))


if __name__ == '__main__':
    try:
        if len(sys.argv) == 7 and sys.argv[1] == '--compact-journal':
            complete = compact_journal(sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5] == '1', int(sys.argv[6]))
            print('complete' if complete else 'partial')
        else:
            r.require(len(sys.argv) == 1, 'retention takes no arguments')
            run()
    except (OSError, RuntimeError, ValueError, KeyError, sqlite3.Error, subprocess.SubprocessError) as error:
        print('Local retention stopped: ' + str(error), file=sys.stderr)
        sys.exit(1)
