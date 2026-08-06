#!/usr/bin/env bash
set -euo pipefail

uname -a
df -h
docker version
docker compose version
java -version
jq --version
shellcheck --version
