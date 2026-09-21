"""Scoped assessment of a retained strict recovery failure; never repairs state."""


def assess_unknown(before, after, frames, later_commands, reloads_before):
    command = before['command_id'].replace('-', '').lower()
    operation = before['operation_id'].replace('-', '').lower()
    assert before['database_state'] == after['database_state'] == 'unknown'
    assert before['command_id'] == after['command_id']
    assert before['operation_id'] == after['operation_id']
    for snapshot in (before, after):
        assert snapshot['journal_count_state_error_receipt'] == '1|unknown|manual_reconciliation_required|'
        assert snapshot['root_count_state_response'] == '0||' and snapshot['root'] == []
        assert len(snapshot['journal']) == 1
        journal = snapshot['journal'][0]
        assert journal['command_id'].lower() == command
        assert journal['state'] == 'unknown' and journal['error_code'] == 'manual_reconciliation_required'
        assert journal['receipt_hex'] == '' and journal['payload_hash_version'] == 2
        assert snapshot['native_reload_count'] == reloads_before
        assert snapshot['node']['connection_state'] == 'online'
    assert before['journal'][0]['semantic_hash'] == after['journal'][0]['semantic_hash']
    assert later_commands == [before['command_id']]
    assert len(frames) >= 2 and frames[0]['delivery_mode'] == 1
    assert all(frame['delivery_mode'] == 2 for frame in frames[1:])
    for frame in frames:
        assert frame['command_id'] == command and frame['operation_id'] == operation
        assert frame['semantic_payload_sha256'] == before['journal'][0]['semantic_hash'].lower()
        assert frame['semantic_payload_hash_version'] == 2
        assert frame['required_capability'] == 'ocserv.service.reload'
        assert frame['owner_epoch'] > 0 and frame['owner_fence_id'] and frame['connection_id']
    assert frames[-1]['owner_epoch'] == after['owner']['owner_epoch']
    return {'status': 'EXPECTED-UNKNOWN', 'strict_probe_status': 'FAIL',
            'operation_id': before['operation_id'], 'command_id': before['command_id'],
            'semantic_hash': before['journal'][0]['semantic_hash'].lower(),
            'execute_frames': 1, 'query_only_frames': len(frames)-1,
            'conflicting_writes': 'PAUSED', 'root_receipt': 'ABSENT',
            'durable_state': 'PRESERVED', 'native_reload_delta': 0,
            'automatic_completion': 'NOT_PROVEN',
            'not_run': ['uncertain mutation idempotency replay', 'consumed approval mutation replay']}
