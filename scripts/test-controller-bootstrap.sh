#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BOOTSTRAP="${ROOT}/deploy/production/controller-bootstrap.sh"
REPOSITORY_URL="https://github.com/GentleKingson/ocservia"

# The bootstrap fixture asserts file modes with GNU stat; skip on hosts
# without the GNU userland (mirrors the guard in test-controller-install.sh).
stat -c '%u' . >/dev/null 2>&1 || {
  echo "Controller bootstrap tests skipped: GNU stat is unavailable" >&2
  exit 0
}
command -v git >/dev/null 2>&1 || {
  echo "Controller bootstrap tests skipped: git is unavailable" >&2
  exit 0
}
[[ -x "${BOOTSTRAP}" ]] || {
  echo "Controller bootstrap tests require an executable bootstrap script" >&2
  exit 1
}
umask 022

fixture="$(mktemp -d "${HOME}/.ocservia-controller-bootstrap-test.XXXXXX")"
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  rm -rf -- "${fixture}"
  exit "${status}"
}
trap cleanup EXIT INT TERM

origin_work="${fixture}/origin-work"
origin="${fixture}/origin.git"
config="${fixture}/config"
source_root="${fixture}/source-root"
bin="${fixture}/bin"
logs="${fixture}/logs"
git_log="${logs}/git.log"
install_log="${logs}/install.log"
EXTRA_ENV=()
RUN_STATUS=0
RUN_OUTPUT=""

die() {
  echo "Controller bootstrap tests: $1" >&2
  [[ -n "${RUN_OUTPUT:-}" ]] && printf '%s\n' "${RUN_OUTPUT}" >&2
  exit 1
}

mkdir -m 700 -- "${bin}" "${logs}" "${config}"

mkdir -p -- "${origin_work}"
git -C "${origin_work}" init -q
git -C "${origin_work}" config user.name test
git -C "${origin_work}" config user.email test@example.invalid
# v0.1.0 predates the production installer; v0.1.2 ships a mocked installer
# that records how it was handed off to (the bootstrap must exec exactly
# this entrypoint from the operator's configuration directory, never a
# sibling lifecycle script); v0.1.3 is a later release on another commit.
mkdir -p -- "${origin_work}/docs"
printf 'old release\n' >"${origin_work}/docs/readme.md"
git -C "${origin_work}" add -A
git -C "${origin_work}" commit -qm base
git -C "${origin_work}" tag v0.1.0
mkdir -p -- "${origin_work}/deploy/production"
cat >"${origin_work}/deploy/production/install.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
log="${BOOTSTRAP_TEST_INSTALL_LOG:?}"
printf 'invoked-as:%s\n' "$0" >>"${log}"
printf 'pwd:%s\n' "$(pwd -P)" >>"${log}"
printf 'args:%s\n' "$*" >>"${log}"
printf 'env-resolved-marker:%s\n' "${OCSERV_INSTALL_ENV_RESOLVED:-<unset>}" >>"${log}"
exit "${MOCK_INSTALL_EXIT:-0}"
EOF
chmod 0755 -- "${origin_work}/deploy/production/install.sh"
git -C "${origin_work}" add -A
git -C "${origin_work}" commit -qm installer
git -C "${origin_work}" tag v0.1.2
git -C "${origin_work}" commit -q --allow-empty -m next
git -C "${origin_work}" tag v0.1.3
git clone -q --bare "${origin_work}" "${origin}"
v012_commit="$(git -C "${origin}" rev-parse 'v0.1.2^{commit}')"

# The git wrapper rewrites the production repository URL to the fixture
# origin for the two network-facing subcommands (clone and ls-remote) and
# logs them. After a successful clone it restores the production URL as the
# recorded origin, so the bootstrap's origin-identity validation runs against
# exactly the state a production clone would have. Every other git call
# passes through untouched.
REAL_GIT="$(command -v git)"
cat >"${bin}/git" <<EOF
#!/usr/bin/env bash
set -euo pipefail
args=()
for arg in "\$@"; do
  if [[ "\${arg}" == "${REPOSITORY_URL}" ]]; then
    arg="${origin}"
  fi
  args+=("\${arg}")
done
case "\${1:-}" in
  clone)
    printf 'clone\n' >>"\${BOOTSTRAP_TEST_GIT_LOG:?}"
    clone_target="\${args[\${#args[@]}-1]}"
    if "${REAL_GIT}" "\${args[@]}"; then
      "${REAL_GIT}" -C "\${clone_target}" remote set-url origin "${REPOSITORY_URL}"
    else
      exit \$?
    fi
    exit 0
    ;;
  ls-remote)
    printf 'ls-remote\n' >>"\${BOOTSTRAP_TEST_GIT_LOG:?}"
    ;;
esac
exec "${REAL_GIT}" "\${args[@]}"
EOF

cat >"${bin}/sudo" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${BOOTSTRAP_TEST_SUDO_LOG:?}"
exec "$@"
EOF

chmod 0755 -- "${bin}/git" "${bin}/sudo"

# The bootstrap subprocesses run with PATH="${bin}" only: a self-contained
# toolset with no Docker client by default, so the Docker-absent scenarios
# stay deterministic even on hosts (and CI runners) that have Docker.
link_tool() {
  local tool="$1" path
  path="$(command -v "${tool}" || true)"
  [[ -n "${path}" ]] || die "required test tool not found: ${tool}"
  ln -s "${path}" "${bin}/${tool}"
}
for tool in bash env stat realpath mkdir id dirname rm curl; do
  link_tool "${tool}"
done

install_docker_client() {
  cat >"${bin}/docker" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
  chmod 0755 -- "${bin}/docker"
}

install_docker_denied() {
  cat >"${bin}/docker" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == info ]]; then
  echo "Got permission denied while trying to connect to the Docker daemon socket" >&2
  exit 1
fi
exit 0
EOF
  chmod 0755 -- "${bin}/docker"
}

reset_state() {
  rm -rf -- "${source_root}" "${fixture}/source-root-b"
  : >"${git_log}"
  : >"${install_log}"
  rm -f -- "${bin}/docker" "${config}/install.env"
  EXTRA_ENV=()
}

write_install_env() {
  cat >"${config}/install.env" <<EOF
OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY=${fixture}/controller-release-signing.pub.pem
OCSERV_PUBLIC_HOST=controller-bootstrap.example.test
EOF
}
: >"${fixture}/controller-release-signing.pub.pem"

run_bootstrap() {
  BOOTSTRAP_TEST_INSTALL_LOG="${install_log}" \
    BOOTSTRAP_TEST_GIT_LOG="${git_log}" \
    PATH="${bin}" \
    OCSERV_CONTROLLER_SOURCE_ROOT="${source_root}" \
    env "${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"}" "${BOOTSTRAP}" "$@"
}

capture() {
  RUN_STATUS=0
  RUN_OUTPUT="$(run_bootstrap "$@" 2>&1)" || RUN_STATUS=$?
}

capture_from() {
  local directory="$1"
  shift
  RUN_STATUS=0
  RUN_OUTPUT="$( (cd -- "${directory}" && run_bootstrap "$@") 2>&1)" || RUN_STATUS=$?
}

assert_status() {
  (( RUN_STATUS == "$1" )) ||
    die "expected exit status $1 but got ${RUN_STATUS}"
}

assert_output() {
  grep -q -- "$1" <<<"${RUN_OUTPUT}" ||
    die "expected output to contain '$1'"
}

assert_log_contains() {
  grep -qF -- "$2" "$1" || die "expected $1 to contain '$2'"
}

assert_log_empty() {
  [[ ! -s "$1" ]] || die "expected $1 to stay empty, got: $(cat -- "$1")"
}

git_calls() {
  grep -c "^$1$" "${git_log}" || true
}

# 1. usage errors.
reset_state
capture
assert_status 2 "no arguments must be a usage error"
capture --version
assert_status 2 "a valueless --version must be a usage error"
capture --nonsense
assert_status 2 "an unknown flag must be a usage error"
capture --version v0.1.2 --version v0.1.3
assert_status 2 "a repeated --version must be a usage error"
assert_log_empty "${git_log}"
echo "usage errors fail with status 2"

# 2. only exact vX.Y.Z versions are accepted.
reset_state
for bad_version in latest main 0.1.2 v0.1 v0.1.2-rc1 v0.1.2.3; do
  capture --version "${bad_version}"
  assert_status 1 "version '${bad_version}' must fail closed"
  assert_output "exact vX.Y.Z release tag"
done
assert_log_empty "${git_log}"
[[ ! -e "${source_root}" ]] ||
  die "an invalid version must not create the source root"
echo "non-exact versions are rejected before any action"

# 3. a non-existent release tag fails closed and leaves no target.
reset_state
write_install_env
capture_from "${config}" --version v9.9.9
assert_status 1 "a non-existent version must fail closed"
assert_output "release tag may not exist"
assert_log_contains "${git_log}" "clone"
[[ ! -e "${source_root}/v9.9.9" ]] ||
  die "a failed clone must not leave a target behind"
assert_log_empty "${install_log}"
echo "a non-existent release tag fails closed without leaving a target"

# 4. --check rejects an invalid install.env before anything else.
reset_state
printf 'OCSERV_NOT_A_SETTING=x\n' >"${config}/install.env"
capture_from "${config}" --version v0.1.2 --check
assert_status 1 "an invalid install.env must fail the check"
assert_output "unknown configuration variable OCSERV_NOT_A_SETTING"
assert_log_empty "${git_log}"
[[ ! -e "${source_root}" ]] ||
  die "the check must not create the source root"
echo "an invalid install.env fails the check"

# 5. --check rejects a missing release trust path.
reset_state
EXTRA_ENV=("OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY=")
capture_from "${config}" --version v0.1.2 --check
assert_status 1 "a missing trust path must fail the check"
assert_output "OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY is not set"
echo "a missing release trust path fails the check"

# 5a. --check rejects an unusable trust path value.
reset_state
EXTRA_ENV=("OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY=relative.pub.pem")
capture_from "${config}" --version v0.1.2 --check
assert_status 1 "a relative trust path must fail the check"
assert_output "absolute path"
EXTRA_ENV=("OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY=${fixture}/missing.pub.pem")
capture_from "${config}" --version v0.1.2 --check
assert_status 1 "a missing trust file must fail the check"
assert_output "readable regular file"
echo "an unusable trust path value fails the check"

# 6. --check passes read-only for the launcher lifecycle.
reset_state
write_install_env
install_docker_client
capture_from "${config}" --version v0.1.2 --check
assert_status 0 "the launcher check must pass"
assert_output "lifecycle: launcher"
assert_output "check passed"
[[ "$(git_calls clone)" == 0 ]] ||
  die "the check must not clone"
[[ "$(git_calls ls-remote)" == 1 ]] ||
  die "the check must probe the release tag read-only exactly once"
[[ ! -e "${source_root}" ]] ||
  die "the check must not create the source root"
assert_log_empty "${install_log}"
echo "check passes for the launcher lifecycle without mutation"

# 7. --check reports the fresh-host root lifecycle when no Docker exists.
reset_state
write_install_env
capture_from "${config}" --version v0.1.2 --check
assert_status 0 "the Docker-less check must pass"
assert_output "lifecycle: root (no Docker client installed"
assert_output "check passed"
[[ "$(git_calls clone)" == 0 ]] ||
  die "the check must not clone"
echo "check reports the fresh-host root lifecycle"

# 8. --check fails closed when the Docker client is unusable.
reset_state
write_install_env
install_docker_denied
capture_from "${config}" --version v0.1.2 --check
assert_status 1 "an unusable Docker client must fail the check"
assert_output "cannot use the Docker daemon"
assert_output "--root-lifecycle"
assert_log_empty "${install_log}"
echo "check predicts the Docker fail-closed path"

# 9. a fresh clone selects the root lifecycle and hands off from the
# configuration directory.
reset_state
write_install_env
capture_from "${config}" --version v0.1.2
assert_status 0 "the Docker-less bootstrap must succeed"
assert_output "lifecycle: root (no Docker client installed"
assert_output "cloned and verified clean v0.1.2 checkout"
target="${source_root}/v0.1.2"
[[ -d "${target}" ]] || die "the durable checkout is missing"
[[ "$(git -C "${target}" rev-parse HEAD)" == "${v012_commit}" ]] ||
  die "the checkout HEAD must be the v0.1.2 release commit"
[[ "$(git -C "${target}" tag --points-at HEAD)" == "v0.1.2" ]] ||
  die "the checkout must carry the v0.1.2 tag at HEAD"
[[ -z "$(git -C "${target}" status --porcelain --untracked-files=all)" ]] ||
  die "the cloned checkout must be clean"
[[ "$(git_calls clone)" == 1 ]] ||
  die "exactly one clone was expected"
assert_log_contains "${install_log}" "invoked-as:${target}/deploy/production/install.sh"
assert_log_contains "${install_log}" "pwd:${config}"
assert_log_contains "${install_log}" "args:--root-lifecycle"
assert_log_contains "${install_log}" "env-resolved-marker:<unset>"
echo "a fresh clone hands off to the installer with the root lifecycle"

# 10. an existing verified checkout is reused without recloning.
capture_from "${config}" --version v0.1.2
assert_status 0 "the rerun must succeed"
assert_output "reusing verified clean v0.1.2 checkout"
[[ "$(git_calls clone)" == 1 ]] ||
  die "the rerun must not clone again"
[[ "$(wc -l <"${install_log}" | tr -d ' ')" == 8 ]] ||
  die "the installer must have been handed off to exactly twice"
echo "an existing clean checkout is reused without recloning"

# 11. a dirty checkout is refused, never recloned or deleted.
reset_state
write_install_env
capture_from "${config}" --version v0.1.2
assert_status 0 "the initial clone must succeed"
printf 'local operator edit\n' >"${target}/local-note.txt"
capture_from "${config}" --version v0.1.2
assert_status 1 "a dirty checkout must fail closed"
assert_output "dirty"
[[ "$(git_calls clone)" == 1 ]] ||
  die "a dirty checkout must not trigger a reclone"
[[ "$(wc -l <"${install_log}" | tr -d ' ')" == 4 ]] ||
  die "a dirty checkout must not hand off to the installer"
[[ -d "${target}" ]] ||
  die "a dirty pre-existing checkout must never be deleted"
echo "a dirty checkout is refused without recloning or deleting"

# 12. a checkout from a different origin is refused.
git -C "${target}" remote set-url origin https://example.invalid/other.git
capture_from "${config}" --version v0.1.2
assert_status 1 "a foreign-origin checkout must fail closed"
assert_output "instead of ${REPOSITORY_URL}"
[[ -d "${target}" ]] || die "a foreign-origin checkout must never be deleted"
[[ "$(git_calls clone)" == 1 ]] ||
  die "a foreign-origin checkout must not trigger a reclone"
echo "a checkout tracking another origin is refused"

# 13. a checkout at a different release tag is refused.
reset_state
write_install_env
source_root_b="${fixture}/source-root-b"
mkdir -p -- "${source_root_b}"
git clone -q --branch v0.1.3 --single-branch "${origin}" "${source_root_b}/v0.1.2"
git -C "${source_root_b}/v0.1.2" remote set-url origin "${REPOSITORY_URL}"
EXTRA_ENV=("OCSERV_CONTROLLER_SOURCE_ROOT=${source_root_b}")
capture_from "${config}" --version v0.1.2
assert_status 1 "a wrong-tag checkout must fail closed"
assert_output "not exactly at the requested tag v0.1.2"
echo "a checkout at a different release tag is refused"

# 14. a symlinked target path is refused.
reset_state
write_install_env
mkdir -p -- "${source_root}"
ln -s -- "${config}" "${source_root}/v0.1.2"
capture_from "${config}" --version v0.1.2
assert_status 1 "a symlinked target must fail closed"
assert_output "not a regular directory"
[[ -L "${source_root}/v0.1.2" ]] ||
  die "the bootstrap must not follow or replace the symlink"
echo "a symlinked target path is refused"

# 15. symlinked source-root ancestry is refused.
reset_state
write_install_env
mkdir -m 700 -- "${fixture}/real-root"
ln -s -- "${fixture}/real-root" "${fixture}/link-root"
EXTRA_ENV=("OCSERV_CONTROLLER_SOURCE_ROOT=${fixture}/link-root/releases")
capture_from "${config}" --version v0.1.2
assert_status 1 "symlinked source-root ancestry must fail closed"
assert_output "symlink"
echo "symlinked source-root ancestry is refused"

# 16. a group/world-writable source-root ancestor is refused.
reset_state
write_install_env
mkdir -m 0777 -- "${fixture}/loose-parent"
EXTRA_ENV=("OCSERV_CONTROLLER_SOURCE_ROOT=${fixture}/loose-parent/releases")
capture_from "${config}" --version v0.1.2
assert_status 1 "a world-writable ancestor must fail closed"
assert_output "group/world-writable"
echo "a world-writable source-root ancestor is refused"

# 17. a usable Docker daemon selects the launcher lifecycle.
reset_state
write_install_env
install_docker_client
capture_from "${config}" --version v0.1.2
assert_status 0 "the launcher bootstrap must succeed"
assert_output "lifecycle: launcher"
assert_log_contains "${install_log}" "args:"
if grep -q -- "--root-lifecycle" "${install_log}"; then
  die "the launcher lifecycle must not pass --root-lifecycle: $(cat -- "${install_log}")"
fi
assert_log_contains "${install_log}" "pwd:${config}"
echo "a usable Docker daemon selects the launcher lifecycle"

# 18. a Docker client without daemon access fails closed before mutation.
reset_state
write_install_env
install_docker_denied
capture_from "${config}" --version v0.1.2
assert_status 1 "an unusable Docker client must fail closed"
assert_output "cannot use the Docker daemon"
assert_output "--root-lifecycle"
assert_log_empty "${install_log}"
[[ ! -e "${source_root}" ]] ||
  die "the Docker fail-closed path must not create the source root"
echo "a Docker client without daemon access fails closed before mutation"

# 18a. an explicit --root-lifecycle overrides the Docker probe.
capture_from "${config}" --version v0.1.2 --root-lifecycle
assert_status 0 "an explicit root lifecycle must ignore broken Docker"
assert_output "lifecycle: root (explicit"
assert_log_contains "${install_log}" "args:--root-lifecycle"
echo "an explicit --root-lifecycle overrides the Docker probe"

# 19. a release without the production installer fails closed after cloning.
reset_state
write_install_env
capture_from "${config}" --version v0.1.0
assert_status 1 "a release without the installer must fail closed"
assert_output "does not ship the production installer"
[[ -d "${source_root}/v0.1.0" ]] ||
  die "a clean checkout of an old release must be kept for inspection"
assert_log_empty "${install_log}"
echo "a release without the installer is refused but keeps the checkout"

echo "Controller bootstrap tests passed"
