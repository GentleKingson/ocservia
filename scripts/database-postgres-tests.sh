#!/usr/bin/env bash
# Current-schema business suites, shared with the offline routing self-test.

run_postgres_business_tests() {
  local container="$1" major="$2" port="$3" runtime_url="$4" owner_url="$5"
  local latest_runtime_url="$6" latest_owner_url="$7" package fixture_database group
  (cd "${TEST_CONTROL_PLANE}" && OCSERV_TEST_DATABASE_URL="${runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${owner_url}" \
    bash "${ROOT}/scripts/required-go-tests.sh" backend-enrollment -p 1 ./internal/operations ./internal/enrollment ./internal/transportevents -run Integration)
  (cd "${ROOT}/control-plane" && OCSERV_TEST_DATABASE_URL="${latest_runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${latest_owner_url}" \
    go test -p 1 ./internal/telemetry -run Integration -count=1)
  OCSERV_TEST_DATABASE_URL="${runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${owner_url}" \
    bash "${ROOT}/scripts/test-enrollment-restart.sh" "${container}"
  # User workflows retain revision-zero commands and singleton lease state.
  # Keep their fixtures out of other packages.
  for package in userstate useroperations; do
    fixture_database="ocservia_${package}_${major}"
    clone_database "${container}" ocservia "${fixture_database}"
    group="backend-policy-${package}"
    (cd "${TEST_CONTROL_PLANE}" && OCSERV_TEST_DATABASE_URL="postgres://ocservia_app:test-runtime-only@127.0.0.1:${port}/${fixture_database}?sslmode=disable" \
      OCSERV_TEST_OWNER_DATABASE_URL="postgres://ocservia_owner:test-owner-only@127.0.0.1:${port}/${fixture_database}?sslmode=disable" \
      bash "${ROOT}/scripts/required-go-tests.sh" "${group}" -p 1 "./internal/${package}" -run Integration)
    docker exec "${container}" dropdb -U ocservia_owner "${fixture_database}"
  done
  (cd "${TEST_CONTROL_PLANE}" && OCSERV_TEST_DATABASE_URL="${runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${owner_url}" \
    bash "${ROOT}/scripts/required-go-tests.sh" backend-policy-config -p 1 ./internal/configplan -run Integration)
  (cd "${TEST_CONTROL_PLANE}" && OCSERV_TEST_DATABASE_URL="${runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${owner_url}" \
    bash "${ROOT}/scripts/required-go-tests.sh" backend-policy-certificates -p 1 ./internal/certificates -run Integration)
  (cd "${TEST_CONTROL_PLANE}" && OCSERV_TEST_DATABASE_URL="${runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${owner_url}" \
    go test -p 1 ./internal/approvals ./internal/audit ./internal/privdattestation -run Integration -count=1)
  # Local/RBAC fixtures retain the current production runtime privileges.
  # Clone before any auth fixture writes. Lifecycle bootstrap requires no prior
  # singleton/admin grants; its audit constraints must never affect other suites.
  clone_database "${container}" ocservia_latest ocservia_lifecycle
  clone_database "${container}" ocservia_latest ocservia_auth_backend
  OCSERV_TEST_DATABASE_URL="${latest_runtime_url/ocservia_latest/ocservia_auth_backend}" \
    OCSERV_TEST_OWNER_DATABASE_URL="${latest_owner_url/ocservia_latest/ocservia_auth_backend}" \
    checked_go_tests backend-auth -p 1 -parallel 1 ./internal/api -run '^Test(AuthenticationBackend(HTTP|Safety|Legacy)|LocalResetApprovalPrecheckBackendHTTP)Integration$'
  assert_auth_fixture_cleanup "${container}" ocservia_auth_backend
  docker exec "${container}" dropdb -U ocservia_owner ocservia_auth_backend
  clone_database "${container}" ocservia_latest ocservia_policy_api
  OCSERV_TEST_DATABASE_URL="${latest_runtime_url/ocservia_latest/ocservia_policy_api}" \
    OCSERV_TEST_OWNER_DATABASE_URL="${latest_owner_url/ocservia_latest/ocservia_policy_api}" \
    checked_go_tests backend-policy-api --select -race -p 1 -parallel 1
  docker exec "${container}" dropdb -U ocservia_owner ocservia_policy_api
  OCSERV_TEST_DATABASE_URL="${latest_runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${latest_owner_url}" \
    checked_go_tests database-auth -p 1 -parallel 1 ./internal/rbac ./internal/auth -run Integration
  assert_auth_fixture_cleanup "${container}" ocservia_latest
  OCSERV_TEST_DATABASE_URL="${latest_runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${latest_owner_url}" \
    checked_go_tests database-api -p 1 -parallel 1 ./internal/api \
      -run '^(TestLocalAccountHTTPSharedLimitsIntegration|TestAuthHTTPLoginLogoutIntegration|TestAuthLogSessionFailureIntegration|TestAuthenticationBackendHTTPIntegration)$'
  assert_auth_fixture_cleanup "${container}" ocservia_latest
  OCSERV_TEST_DATABASE_URL="${latest_runtime_url/ocservia_latest/ocservia_lifecycle}" \
    OCSERV_TEST_OWNER_DATABASE_URL="${latest_owner_url/ocservia_latest/ocservia_lifecycle}" \
    checked_go_tests database-lifecycle -p 1 -parallel 1 ./internal/api -run '^TestLocalUserLifecycleIntegration$'
  assert_auth_fixture_cleanup "${container}" ocservia_lifecycle
  docker exec "${container}" dropdb -U ocservia_owner ocservia_lifecycle
  (cd "${TEST_CONTROL_PLANE}" && OCSERV_TEST_DATABASE_URL="${runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${owner_url}" \
    bash "${ROOT}/scripts/required-go-tests.sh" backend-policy-upgrades --select -p 1)
  (cd "${TEST_CONTROL_PLANE}" && OCSERV_TEST_DATABASE_URL="${runtime_url}" \
    go test -p 1 ./internal/api -run '^TestApprovalDetailRequiresEveryAuthorityScopeIntegration$|^TestBrowserTrustBoundaryBlocksCrossSiteCookieMutations$' -count=1)
}
