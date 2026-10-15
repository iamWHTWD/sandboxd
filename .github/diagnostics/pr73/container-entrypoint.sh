#!/usr/bin/env bash
set -Eeuo pipefail
export EVIDENCE=/evidence
export SANDBOXD_LOG_FILE=/evidence/sandboxd.log
observer_pid=''
test_pid=''
wait_bounded() {
  local pid="$1" seconds="$2" end=$(( SECONDS + $2 ))
  while kill -0 "${pid}" 2>/dev/null && (( SECONDS < end )); do sleep 0.2; done
  if kill -0 "${pid}" 2>/dev/null; then
    printf 'Diagnostic wrapper wait expired pid=%s bound=%ss\n' "${pid}" "${seconds}" \
      >>/evidence/wrapper-cleanup.log
    kill -KILL "${pid}" 2>/dev/null || true
  fi
  wait "${pid}" 2>/dev/null || true
}
finish() {
  local status=$?
  trap - EXIT TERM INT
  set +e
  if [[ -n "${test_pid}" ]]; then
    kill -TERM "${test_pid}" 2>/dev/null
    wait_bounded "${test_pid}" 45
  fi
  [[ -z "${observer_pid}" ]] || kill -TERM "${observer_pid}" 2>/dev/null
  [[ -z "${observer_pid}" ]] || wait_bounded "${observer_pid}" 25
  for file in /var/log/sandboxd/checkpoint-workload.stdout \
    /var/log/sandboxd/checkpoint-runtime.stderr /home/akernel/sandboxd/config.toml; do
    [[ ! -f "${file}" ]] || cp -- "${file}" /evidence/
  done
  printf '%s\n' "${status}" >/evidence/container-exit-code.txt
  exit "${status}"
}
terminate() {
  exit 143
}
trap finish EXIT
trap terminate TERM INT
mkdir -p /evidence
sha256sum /usr/local/bin/sandboxd >/evidence/bound-sandboxd.sha256
bash /diagnostic/observe.sh > /evidence/observer.stdout 2>&1 &
observer_pid=$!
set +e
bash /usr/local/bin/sandboxd-e2e-run &
test_pid=$!
wait "${test_pid}"
status=$?
test_pid=''
exit "${status}"
