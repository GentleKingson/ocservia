#!/usr/bin/env bash
# Shared Business phases; sourced by the hosted driver and its offline self-tests.

configure_auth_peers() {
  # --no-deps makes ordering our responsibility. The launcher stops both peers;
  # wait for the trust backend before starting the unchanged transport container.
  compose up -d --no-deps --wait control-plane
  compose start transportd
  python3 "${ROOT}/scripts/release-business-api.py" transport_ready
}
configure_managed_options() {
  managed_options=(--version "v${VERSION}")
  if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true ]]; then managed_options+=(--root-lifecycle); fi
}

run_business_browser() {
  local work="$1"
  if [[ "${BUSINESS_PROFILE}" == extended ]]; then
    next_stage certificate
    python3 "${ROOT}/scripts/release-business-api.py" certificate
    record real_certificate_lifecycle
  fi
  next_stage browser
  npm --prefix "${ROOT}/web" ci --ignore-scripts
  (cd "${ROOT}/web" && npx playwright install --with-deps chromium)
  sudo apt-get install -y --no-install-recommends libnss3-tools
  sudo install -m 644 "${work}/ca.crt" /usr/local/share/ca-certificates/ocservia-business-test-ca.crt
  sudo update-ca-certificates
  mkdir -p "${HOME}/.pki/nssdb"
  if [[ ! -f "${HOME}/.pki/nssdb/cert9.db" ]]; then certutil -N --empty-password -d "sql:${HOME}/.pki/nssdb"; fi
  certutil -A -d "sql:${HOME}/.pki/nssdb" -n ocservia-business-test-ca -t C,, -i "${work}/ca.crt"
  python3 "${ROOT}/scripts/release-business-api.py" browser_prepare
  NODE_EXTRA_CA_CERTS="${work}/ca.crt" node "${ROOT}/scripts/release-business-browser.mjs"
  python3 "${ROOT}/scripts/release-business-api.py" browser_verify
  record real_browser_subset
  if [[ "${BUSINESS_PROFILE}" == smoke ]]; then
    next_stage signer_vpn_user
    python3 "${ROOT}/scripts/release-business-api.py" smoke_user
    record approved_config_apply_and_vpn_user
  fi
}

run_business_vpn() {
  next_stage business_vpn
  python3 "${ROOT}/scripts/release-business-api.py" vpn_after_config_apply
  record real_vpn_after_config_apply
  if [[ "${BUSINESS_PROFILE}" == extended || "${OCSERV_PACKAGE:-distribution}" == source-1.6.0 ]]; then
    next_stage configuration_rollback
    python3 "${ROOT}/scripts/release-business-api.py" configuration
    record config_plan_automatic_rollback
    next_stage vpn_after_rollback
    python3 "${ROOT}/scripts/release-business-api.py" vpn_after_rollback
    record real_vpn_after_rollback
  fi
  if [[ "${BUSINESS_PROFILE}" == extended ]]; then
    next_stage supplemental_business
    python3 "${ROOT}/scripts/release-business-api.py" business
    record real_vpn_business_and_recovery
  fi
}

run_business_completion() {
  if [[ "$BUSINESS_RUN_RESILIENCE" == true ]]; then
    next_stage single_instance_recovery
    python3 "${ROOT}/scripts/release-business-api.py" resilience
    # shellcheck disable=SC2034 # Consumed by the driver's cleanup result writer.
    resilience_result=PASS
    record single_instance_recovery
  fi
  if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true && "$BUSINESS_PROFILE" == extended ]]; then
    next_stage integrated_recovery
    python3 "$ROOT/scripts/release-integrated-acceptance.py" recovery
    record integrated_recreation_and_authorized_sse
  fi
  if [[ "$BUSINESS_PROFILE" == extended ]]; then
    # Last version-dependent stage: the rollout replaces the Agent, so no later check may assume VERSION.
    next_stage agent_rollout
    bash "${ROOT}/scripts/release-business-rollout.sh"
    record real_browser_agent_rollout
  fi
  run_business_terminal
}

run_business_terminal() {
  # Default release acceptance retains terminal revocation; manual diagnostics
  # may instead prove package removal without changing the node's trust.
  next_stage "node_${BUSINESS_TERMINAL}"
  python3 "${ROOT}/scripts/release-business-api.py" "$BUSINESS_TERMINAL"
  if [[ "$BUSINESS_TERMINAL" == uninstall ]]; then
    record real_node_uninstall_and_offline
  else
    record real_node_revocation_and_disconnect
  fi
  next_stage complete
}
