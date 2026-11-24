#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
: "${ARTIFACT_DIR:?}" "${RUN_ID:?}"
work="$(mktemp -d)"
tag="ocservia-native-smoke-${work##*.}"
tag="${tag,,}"
containers=()
images=()
cleanup() {
  local container
  for container in "${containers[@]:-}"; do
    [[ -n "${container}" ]] || continue
    docker rm -fv "${container}" >/dev/null 2>&1 || true
  done
  if ((${#images[@]})); then docker image rm "${images[@]}" >/dev/null 2>&1 || true; fi
  rm -rf -- "${work}"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
case "$(uname -m)" in
  x86_64) arch=amd64; rpm_arch=x86_64 ;;
  aarch64) arch=arm64; rpm_arch=aarch64 ;;
  *) echo 'native smoke requires amd64 or arm64' >&2; exit 2 ;;
esac
mkdir -p "${ARTIFACT_DIR}" "${work}/packages"
packages="${work}/packages"
if [[ -n "${PRODUCTS_DIR:-}" ]]; then
  versions=("${VERSION:?}")
  packages="$(cd "${PRODUCTS_DIR}" && pwd)"
else
  versions=(2.0.0 2.0.1)
  if [[ "${RC_LIFECYCLE:-false}" == true ]]; then versions=(2.0.0-rc.1 2.0.0-rc.2 2.0.0); fi
  for version in "${versions[@]}"; do
    VERSION="${version}" PACKAGE_ARCH="${arch}" OUTPUT_DIR="${packages}" SOURCE_DATE_EPOCH=1786147200 \
      bash "${ROOT}/scripts/build-release-agent.sh"
  done
fi

rocky_image="$(awk 'NR == 1 && $1 == "FROM" {print $2}' "${ROOT}/rust/agent-build.Dockerfile")"
[[ "${rocky_image}" =~ ^rockylinux:9@sha256:[0-9a-f]{64}$ ]]
images+=("${tag}:rpm" "${tag}:deb")
docker build --build-arg "ROCKY_IMAGE=${rocky_image}" -t "${tag}:rpm" - <<'DOCKERFILE' >"${work}/build-rpm.log" 2>&1 &
ARG ROCKY_IMAGE
FROM ${ROCKY_IMAGE}
RUN dnf install -y systemd python3 openssl diffutils && dnf clean all
DOCKERFILE
rpm_build_pid=$!

docker build -t "${tag}:deb" - <<'DOCKERFILE' >"${work}/build-deb.log" 2>&1 &
FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251
RUN apt-get update && apt-get install -y --no-install-recommends systemd-sysv python3 openssl curl && rm -rf /var/lib/apt/lists/*
DOCKERFILE
deb_build_pid=$!

rpm_build_status=0
deb_build_status=0
wait "${rpm_build_pid}" || rpm_build_status=$?
wait "${deb_build_pid}" || deb_build_status=$?
cat "${work}/build-rpm.log" "${work}/build-deb.log"
if ((rpm_build_status != 0 || deb_build_status != 0)); then
  exit 1
fi

run_family_lifecycle() {
  local family="$1" container="${tag}-${1}" status=""
  # Package scriptlets run against the distro's real systemd and package database.
  docker run -d --name "${container}" --privileged --cgroupns=private --tmpfs /run --tmpfs /tmp \
    -v "${packages}:/packages:ro" "${tag}:${family}" /sbin/init >/dev/null
  for _ in {1..80}; do
    status="$(docker exec "${container}" systemctl is-system-running 2>/dev/null || true)"
    case "${status}" in running|degraded) break ;; esac
    sleep 0.5
  done
  [[ "${status}" == running || "${status}" == degraded ]]
  docker exec -i "${container}" bash -s -- "${family}" "${arch}" "${rpm_arch}" "${versions[@]}" \
    <<'LIFECYCLE' >"${ARTIFACT_DIR}/${family}-lifecycle.log"
set -euo pipefail
family="$1"; arch="$2"; rpm_arch="$3"; shift 3
first="$1"
apply() {
  local version="$1"
  if [[ "${family}" == deb ]]; then
    dpkg -i "/packages/ocservia-agent_${version}-1_${arch}.deb"
  else
    rpm -U --replacepkgs --oldpackage "/packages/ocservia-agent-${version}-1.${rpm_arch}.rpm"
  fi
  for binary in ocservia-agent ocservia-privd ocservia-upgrader; do
    [[ "$("/usr/libexec/ocservia/${binary}" --version)" == "${binary} ${version}" ]]
  done
  for path in /usr/libexec/ocservia/* /usr/lib/systemd/system/ocservia-*.service \
      /usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf; do
    if [[ "${family}" == deb ]]; then
      dpkg-query -S "${path}" | grep -Fq 'ocservia-agent: '
    else
      [[ "$(rpm -qf --queryformat '%{NAME}' "${path}")" == ocservia-agent ]]
    fi
  done
  if [[ "${family}" == rpm ]]; then rpm -V ocservia-agent; else dpkg -V ocservia-agent; fi
}
apply "${first}"
test "$(stat -c '%U:%G:%a' /var/lib/ocservia-vpn-audit)" = ocserv-agent:ocserv-agent:700
for unit in ocservia-privd ocservia-agent ocservia-network-audit; do
  [[ "$(systemctl is-active "${unit}" || true)" == inactive ]]
done
echo node-identity >/var/lib/ocservia-agent/identity/probe
echo '# operator configuration' >>/etc/ocservia-agent/agent.env
cp /etc/ocservia-agent/agent.env /tmp/original-agent.env
shift
for version in "$@"; do apply "${version}"; done
apply "${first}"
cmp /tmp/original-agent.env /etc/ocservia-agent/agent.env
if [[ "${family}" == deb ]]; then dpkg -r ocservia-agent; else rpm -e ocservia-agent; fi
test ! -e /usr/libexec/ocservia/ocservia-agent
test ! -e /usr/lib/systemd/system/ocservia-agent.service
cmp /tmp/original-agent.env /etc/ocservia-agent/agent.env
grep -Fxq node-identity /var/lib/ocservia-agent/identity/probe
LIFECYCLE
  docker rm -fv "${container}" >/dev/null
}

containers=("${tag}-deb" "${tag}-rpm")
run_family_lifecycle deb >"${work}/deb-runner.log" 2>&1 &
deb_pid=$!
run_family_lifecycle rpm >"${work}/rpm-runner.log" 2>&1 &
rpm_pid=$!

deb_status=0
rpm_status=0
wait "${deb_pid}" || deb_status=$?
wait "${rpm_pid}" || rpm_status=$?
cat "${work}/deb-runner.log" "${work}/rpm-runner.log"
for family in deb rpm; do
  if [[ -f "${ARTIFACT_DIR}/${family}-lifecycle.log" ]]; then
    cat "${ARTIFACT_DIR}/${family}-lifecycle.log"
  fi
done
if ((deb_status != 0 || rpm_status != 0)); then
  exit 1
fi
printf 'arch=%s\nversions=%s\ndeb=pass\nrpm=pass\n' "${arch}" "${versions[*]}" >"${ARTIFACT_DIR}/native-package-summary.txt"
