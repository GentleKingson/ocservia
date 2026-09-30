#!/usr/bin/python3
"""Real SQLite/subprocess retention tests in an isolated root container."""
from contextlib import closing
import importlib.util
import os
from pathlib import Path
import sqlite3
import subprocess
import tempfile
import time
import types
import unittest
from unittest.mock import patch
import uuid

spec = importlib.util.spec_from_file_location('retention', Path(__file__).with_name('retain-agent.py'))
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
r = m.r


class RetentionTests(unittest.TestCase):
    def setUp(self):
        if os.geteuid() != 0:
            self.skipTest('requires isolated root container')
        Path('/retention-test-fixtures').mkdir(exist_ok=True, mode=0o755)
        self.temp = tempfile.TemporaryDirectory(dir='/retention-test-fixtures')
        self.root = Path(self.temp.name)
        self.root.chmod(0o755)
        self.patches = []
        for name, leaf in [('CONF', 'conf'), ('STATE', 'state'), ('AGENT_STATE', 'agent'),
                           ('PRIVD_STATE', 'privd'), ('UPGRADE_STATE', 'upgrade')]:
            path = self.root / leaf
            path.mkdir(mode=0o700)
            p = patch.object(r, name, path)
            p.start()
            self.patches.append(p)
        os.chown(r.AGENT_STATE, 1000, 1000)
        self.account = types.SimpleNamespace(pw_uid=1000, pw_gid=1000)
        self.source = dict(node='018f0c2e-7b1a-7c3d-8e9f-0123456789ab', controller='11'*32,
                           endpoint='22'*32, key='33'*32, blocked=False,
                           identity=str(r.AGENT_STATE / 'identity'), journal=str(r.AGENT_STATE / 'agent.db'),
                           effects=str(r.PRIVD_STATE / 'desired-effects.sqlite3'))
        self.operation = r.STATE / str(uuid.uuid4())
        self.operation.mkdir(mode=0o700)
        self.now = int(time.time())
        self.state = dict(id=self.operation.name, phase='verified', source=self.source,
                          node='018f0c2e-7b1a-7c3d-8e9f-0123456789ac', controller='44'*32, key='55'*32,
                          record_hex='aa', source_disposition='unreachable', mutations_blocked=False,
                          verified_at=self.now-400*86400, reason='old detail', token_hash='token hash')
        self.active = dict(node=self.state['node'], endpoint=self.source['endpoint'])
        r.atomic_write(self.operation / 'token', b'old-token')
        with closing(sqlite3.connect(self.source['effects'])) as db, db:
            db.executescript('CREATE TABLE desired_effects(state TEXT); CREATE TABLE authorized_effects(state TEXT);')
        os.chmod(self.source['effects'], 0o600)
        self.database = self.source['journal']
        with closing(sqlite3.connect(self.database)) as db, db:
            db.executescript('''
CREATE TABLE agent_metadata(key TEXT PRIMARY KEY,value BLOB);
CREATE TABLE command_journal(idempotency_key BLOB PRIMARY KEY,command_id BLOB,payload_sha256 BLOB,state TEXT,result BLOB,error_code TEXT,privileged_result_proof BLOB,updated_at INTEGER);
CREATE TABLE synthetic_effects(idempotency_key BLOB PRIMARY KEY,payload_sha256 BLOB,result BLOB);
CREATE TABLE applied_resource_revisions(resource_key TEXT,revision INTEGER);
INSERT INTO applied_resource_revisions VALUES('user:alice',42);
INSERT INTO agent_metadata VALUES('owner_fence_floor',x'00000009');
''')
            for i in range(35):
                key = i.to_bytes(16, 'big')
                db.execute('INSERT INTO command_journal VALUES(?,?,?,?,?,?,?,?)', (key,key,b'h'*32,'succeeded',b'verbose',None,None,1))
                db.execute('INSERT INTO synthetic_effects VALUES(?,?,?)', (key,b'h'*32,b'verbose'))
            db.execute('INSERT INTO command_journal VALUES(?,?,?,?,?,?,?,?)', (b'p'*16,b'p'*16,b'h'*32,'succeeded',b'privileged-result',None,b'proof',1))
        os.chown(self.database, 1000, 1000)
        os.chmod(self.database, 0o600)

    def tearDown(self):
        for p in reversed(self.patches):
            p.stop()
        self.temp.cleanup()

    def retain(self):
        m.retain_one(self.operation, self.state, self.active, self.account, 30, 365, self.now)

    def query(self, sql):
        with closing(sqlite3.connect(self.database)) as db:
            return db.execute(sql).fetchall()

    def test_bounded_resumable_compaction_preserves_proofs_floors_and_tombstones(self):
        self.retain()
        self.assertEqual(self.state['source_state'], 'compacting')
        self.assertEqual(self.query('SELECT count(*) FROM retired_command_details'), [(32,)])
        self.assertTrue((self.operation / 'token').exists())
        self.retain()
        self.retain()
        self.assertEqual(self.query('SELECT count(*) FROM retired_command_details'), [(35,)])
        self.assertEqual(self.query('SELECT count(*) FROM command_journal'), [(36,)])
        self.assertEqual(self.query('SELECT result,privileged_result_proof FROM command_journal WHERE privileged_result_proof IS NOT NULL'), [(b'privileged-result', b'proof')])
        self.assertEqual(self.query('SELECT revision FROM applied_resource_revisions'), [(42,)])
        self.assertEqual(self.query("SELECT value FROM agent_metadata WHERE key='owner_fence_floor'"), [(b'\x00\x00\x00\x09',)])
        self.assertFalse((self.operation / 'token').exists())
        self.assertNotIn('reason', self.state)
        self.assertEqual(self.state['source_disposition'], 'unreachable')
        self.assertEqual(self.state['record_hex'], 'aa')

    def test_active_recent_quarantined_and_unverified_epochs_stay_intact(self):
        for changes in ({'mutations_blocked': True}, {'verified_at': self.now}, {'phase': 'committed'}):
            with patch.dict(self.state, changes):
                self.retain()
        with patch.dict(self.active, node=self.source['node']):
            self.retain()
        self.assertEqual(self.query("SELECT count(*) FROM sqlite_master WHERE name='retired_command_details'"), [(0,)])
        self.assertTrue((self.operation / 'token').exists())

    def test_unresolved_journal_and_foreign_authority_are_not_compacted(self):
        for state, error in [('accepted', None), ('running', None), ('unknown', None), ('failed','manual_reconciliation_required')]:
            with closing(sqlite3.connect(self.database)) as db, db:
                db.execute('UPDATE command_journal SET state=?,error_code=? WHERE idempotency_key=?', (state,error,bytes(16)))
            with self.assertRaises(subprocess.CalledProcessError):
                self.retain()
        with closing(sqlite3.connect(self.database)) as db, db:
            db.execute("UPDATE command_journal SET state='succeeded',error_code=NULL")
            db.execute("INSERT INTO agent_metadata VALUES('controller_binding',?)", (b'foreign',))
        with self.assertRaises(subprocess.CalledProcessError):
            self.retain()
        self.assertEqual(self.query("SELECT count(*) FROM sqlite_master WHERE name='retired_command_details'"), [(0,)])

    def test_prepared_root_effect_blocks_every_cleanup(self):
        with closing(sqlite3.connect(self.source['effects'])) as db, db:
            db.executescript("INSERT INTO desired_effects VALUES('prepared');")
        os.chmod(self.source['effects'], 0o600)
        self.retain()
        self.assertNotIn('source_state', self.state)
        self.assertTrue((self.operation / 'token').exists())

    def test_root_and_hardlinks_refused(self):
        with self.assertRaisesRegex(RuntimeError, 'unprivileged'):
            m.compact_journal(self.database, self.source['node'], self.source['controller'], True, self.now)
        os.link(self.database, r.AGENT_STATE / 'other.db')
        with self.assertRaises(subprocess.CalledProcessError):
            self.retain()

    def test_defaults_ranges_and_invalid_configuration(self):
        self.assertEqual(m.policy(), (30,365))
        for content in (b'OCSERV_AGENT_RETIRED_BINDING_RETENTION_DAYS=0', b'OCSERV_REBIND_HISTORY_RETENTION_DAYS=89', b'UNKNOWN=30'):
            r.atomic_write(r.CONF / 'retention.env', content)
            with self.assertRaises(RuntimeError):
                m.policy()
        r.atomic_write(r.CONF / 'retention.env', b'OCSERV_AGENT_RETIRED_BINDING_RETENTION_DAYS=7\nOCSERV_REBIND_HISTORY_RETENTION_DAYS=2555\n')
        self.assertEqual(m.policy(), (7,2555))


if __name__ == '__main__':
    unittest.main()
