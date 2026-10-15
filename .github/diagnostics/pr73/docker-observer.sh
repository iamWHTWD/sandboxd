#!/usr/bin/env bash
# External adapter: leave all upstream E2E scripts and non-test Docker calls intact.
set -Eeuo pipefail
if [[ "${1:-}" == run ]]; then
  args=("$@")
  name=''
  for (( i=1; i<${#args[@]}; i++ )); do
    case "${args[i]}" in
      --name) name="${args[i+1]:-}"; break ;;
      --name=*) name="${args[i]#--name=}"; break ;;
    esac
  done
  if [[ "${name}" == "${FC_DIAG_CONTAINER:?}" ]]; then
    exec "${FC_DIAG_REAL_DOCKER:?}" run \
      --mount "type=bind,src=${FC_DIAG_SCRIPTS:?},dst=/diagnostic,readonly" \
      --mount "type=bind,src=${FC_DIAG_EVIDENCE:?},dst=/evidence" \
      --mount "type=bind,src=${FC_DIAG_BINARY:?},dst=/usr/local/bin/sandboxd,readonly" \
      --entrypoint /bin/bash \
      "${@:2}" /diagnostic/container-entrypoint.sh
  fi
fi
exec "${FC_DIAG_REAL_DOCKER:?}" "$@"
