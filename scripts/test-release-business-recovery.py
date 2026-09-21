import copy
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('recovery', Path(__file__).with_name('release-business-recovery.py'))
recovery = importlib.util.module_from_spec(spec)
spec.loader.exec_module(recovery)


class AssessmentTests(unittest.TestCase):
    def fixture(self):
        snapshot = {'command_id': 'a-b', 'operation_id': 'c-d', 'database_state': 'unknown',
                    'journal_count_state_error_receipt': '1|unknown|manual_reconciliation_required|',
                    'root_count_state_response': '0||', 'root': [],
                    'journal': [{'command_id': 'AB', 'semantic_hash': 'EF', 'payload_hash_version': 2,
                                 'state': 'unknown', 'error_code': 'manual_reconciliation_required', 'receipt_hex': ''}],
                    'native_reload_count': 3, 'node': {'connection_state': 'online'}, 'owner': {'owner_epoch': 4}}
        frame = {'command_id': 'ab', 'operation_id': 'cd', 'semantic_payload_sha256': 'ef',
                 'semantic_payload_hash_version': 2, 'required_capability': 'ocserv.service.reload',
                 'owner_epoch': 4, 'owner_fence_id': 'fence', 'connection_id': 'connection', 'delivery_mode': 1}
        return snapshot, copy.deepcopy(snapshot), [frame, {**frame, 'delivery_mode': 2}], ['a-b'], 3

    def test_strict_failure_is_not_relabelled_success(self):
        result = recovery.assess_unknown(*self.fixture())
        self.assertEqual(result['status'], 'EXPECTED-UNKNOWN')
        self.assertEqual(result['strict_probe_status'], 'FAIL')
        self.assertEqual(result['automatic_completion'], 'NOT_PROVEN')

    def test_incomplete_or_conflicting_observation_is_not_waived(self):
        for change in (
            lambda args: args[1].update(database_state='succeeded'),
            lambda args: args[1].update(root=[{'state': 'applied'}]),
            lambda args: args[1]['journal'][0].update(receipt_hex='00'),
            lambda args: args[1]['journal'][0].update(semantic_hash='00'),
            lambda args: args[1].update(native_reload_count=4),
            lambda args: args[3].append('new-command'),
            lambda args: args[2][1].update(delivery_mode=1),
            lambda args: args[2][1].update(owner_epoch=3),
            lambda args: args[2].clear(),
        ):
            args = self.fixture()
            change(args)
            with self.assertRaises((AssertionError, KeyError, IndexError)):
                recovery.assess_unknown(*args)


if __name__ == '__main__':
    unittest.main()
