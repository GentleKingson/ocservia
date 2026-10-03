#!/usr/bin/env bash

# Shared backup/restore admission; keep aligned with the Controller connection
# contract. VERSION alone does not distinguish numeric-version forks.
mysql_server_version() {
  local server version comment numeric
  server="$(mysql --defaults-extra-file="$1" --batch --skip-column-names -e 'SELECT VERSION(), @@version_comment')" || return
  IFS=$'\t' read -r version comment <<<"${server}"
  numeric="${version}"
  case "${comment}" in
    'MySQL Community Server - GPL') ;;
    'MySQL Enterprise Server - Commercial') numeric="${version%-commercial}" ;;
    *) echo 'database server must be Oracle MySQL 8.4 LTS' >&2; return 1 ;;
  esac
  if [[ ! "${numeric}" =~ ^8\.4\.(0|[1-9][0-9]*)$ ]]; then
    echo 'database server must be stable Oracle MySQL 8.4 LTS' >&2
    return 1
  fi
  printf '%s\n' "${version}"
}
