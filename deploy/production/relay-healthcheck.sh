#!/usr/bin/env bash
set -euo pipefail

curl --fail --silent --show-error --max-time 2 http://127.0.0.1:8080/generate_204 >/dev/null
