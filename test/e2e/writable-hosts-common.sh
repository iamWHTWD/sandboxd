#!/usr/bin/env bash
# Copyright (c) 2026 Ant Group Corporation.
# Licensed under the Apache License, Version 2.0.

# Shared first-process contract. Callers supply guest, sbox_cmd and fail helpers
# so their RPC timeout and test-owned daemon remain unchanged.
HOSTS_WORKLOAD='set -eu
test -f /etc/hosts
test "$(stat -c %a /etc/hosts)" = 644
test "$(stat -c %u:%g /etc/hosts)" = 0:0
cat /etc/hosts > /var/hosts-at-entry
# A replayed entrypoint is observable independently from mutable hosts.
echo started >> /var/hosts-entry-count
while :; do sleep 1; done'

verify_initial_hosts() {
    local id=$1 response=$2 evidence=$3 attempts=${4:-100} ip
    guest "${id}" "i=0; until test -f /var/hosts-entry-count; do i=\$((i+1)); test \"\$i\" -lt ${attempts}; sleep 0.1; done"
    ip="$(jq -er '.sandbox_ip' "${response}")"
    printf '127.0.0.1 localhost\n::1 localhost ip6-localhost ip6-loopback\n%s %s\n' \
        "${ip}" "${EXPECTED_HOSTNAME}" > "${evidence}.expected-initial-hosts"
    sbox_cmd exec "${id}" /bin/cat /var/hosts-at-entry > "${evidence}.actual-initial-hosts"
    cmp "${evidence}.expected-initial-hosts" "${evidence}.actual-initial-hosts"
    guest "${id}" '! grep -q " /etc/hosts " /proc/mounts'
}

assert_hosts_inventory_absent() {
    local inventory=$1 id
    shift
    # A failed List must exit before the absence predicate: an if pipeline
    # would otherwise treat a transport error as an empty sandbox inventory.
    sbox_cmd list > "${inventory}"
    for id in "$@"; do
        if awk 'NR > 1 {print $1}' "${inventory}" | grep -Fxq "${id}"; then
            fail "${id} leaked a sandbox record"
        fi
    done
}
