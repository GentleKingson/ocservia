#!/usr/bin/env python3
"""Run one phase of the release business acceptance suite."""
import sys

import release_business_scenarios as scenarios


if __name__ == '__main__':
    phase = sys.argv[1]
    if phase not in ('local', 'transport_ready', 'trust_controller', 'bootstrap_token', 'bootstrap_verify', 'certificate', 'config_prepare', 'configuration',
                     'browser_prepare', 'browser_verify', 'business', 'smoke_user', 'rollout_ready', 'rollout_verify',
                     'vpn_after_config_apply', 'vpn_after_rollback', 'resilience', 'revocation', 'uninstall'):
        raise SystemExit('unknown phase')
    if phase.startswith('vpn_'):
        scenarios.vpn_smoke(phase.removeprefix('vpn_'))
    else:
        getattr(scenarios, phase)()
