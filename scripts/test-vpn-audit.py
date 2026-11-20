#!/usr/bin/env python3
import json
from pathlib import Path
import runpy
import unittest

audit = runpy.run_path(str(Path(__file__).resolve().parents[1] / "deploy/managed-node/vpn-audit.py"))


class AuditTest(unittest.TestCase):
    def test_flow_keeps_original_owner_across_address_reuse(self):
        db = audit["database"](":memory:")
        self.addCleanup(db.close)
        env = dict(REASON="connect", ID="42", USERNAME="alice", IP_REAL="192.0.2.10", IP_REMOTE="10.0.0.1", PASSWORD="secret")
        session, flow = audit["session_event"], audit["flow_event"]
        session(db, "boot", env, 1000)
        cache = {}
        fields = " tcp 6 src=10.0.0.1 dst=203.0.113.1 sport=555 dport=443 bytes=123 src=203.0.113.1 dst=10.0.0.1 sport=443 dport=555 bytes=456 id=42"
        flow(db, "boot", cache, "[NEW]" + fields, 1100)
        session(db, "boot", dict(env, REASON="disconnect", STATS_BYTES_IN="0"), 1200)
        session(db, "boot", dict(env, USERNAME="bob", ID="43"), 1300)
        flow(db, "boot", cache, "[DESTROY]" + fields, 1400)
        flow(db, "boot", cache, "[DESTROY]" + fields, 1500)
        records = [json.loads(s) for s, in db.execute("SELECT body FROM pending ORDER BY rowid")]
        self.assertEqual([r["event_type"] for r in records], ["connect", "disconnect", "connect", "flow"])
        self.assertIsNone(records[1]["bytes_out"])
        self.assertEqual(records[1]["bytes_in"], 0)
        self.assertEqual(records[-1]["username"], "alice")
        self.assertEqual((records[-1]["bytes_in"], records[-1]["bytes_out"]), (123, 456))
        self.assertEqual(records[-1]["destination_ip"], "203.0.113.1")
        self.assertNotIn("secret", json.dumps(records))

    def test_invalid_statistics(self):
        for value in ("-1", "NaN", str(2**63)):
            self.assertIsNone(audit["counter"]({"count": value}, "count"))


if __name__ == "__main__":
    unittest.main()
