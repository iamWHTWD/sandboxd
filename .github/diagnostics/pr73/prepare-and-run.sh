#!/usr/bin/env bash
# Draft for a dedicated GitHub-hosted runner ONLY. Never run against ECS.
set -Eeuo pipefail
[[ "${GITHUB_ACTIONS:-}" == true && "${RUNNER_ENVIRONMENT:-}" == github-hosted ]] || {
  printf 'Refusing to run outside a dedicated GitHub-hosted runner.\n' >&2
  exit 2
}
task_root="${FC_DIAG_ROOT:?}"
deadline=$(( ${FC_DIAG_JOB_START:-$(date +%s)} + 75*60 ))
scripts="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
mkdir -p "${task_root}/evidence" "${task_root}/source"
evidence="${task_root}/evidence"
base=7f16edd2254c94d968bce3fcdea8a6df4e40116e
pr=5954c7a286112e4cc88bf0e8a853896d1260ffe5
printf 'base=%s\npr-ci-merge=%s\n' "${base}" "${pr}" >"${evidence}/revisions.txt"
capture_host() {
  date -u '+%Y-%m-%dT%H:%M:%S.%NZ'
  uname -a
  lscpu
  systemd-detect-virt || true
  docker version
  docker info
  containerd --version || true
  runc --version || true
  go version
  df -h
  cat /proc/uptime
  for module in kvm kvm_intel kvm_amd; do
    modinfo "${module}" 2>&1 || true
    for file in /sys/module/"${module}"/parameters/*; do
      [[ -r "${file}" ]] || continue
      printf '\n[%s]\n' "${file}"; sudo cat "${file}" || true
    done
  done
  for file in /sys/devices/system/clocksource/clocksource0/*clocksource \
    /proc/pressure/cpu /proc/pressure/memory /proc/pressure/io; do
    printf '\n[%s]\n' "${file}"; cat "${file}" || true
  done
}
capture_host >"${evidence}/host-before.txt" 2>&1
sudo losetup --list --output NAME,BACK-FILE,AUTOCLEAR,RO >"${evidence}/loops-before.txt"
findmnt --list >"${evidence}/mounts-before.txt"
git init --quiet "${task_root}/verified-git"

for variant in base pr; do
  sha="${!variant}"
  destination="${task_root}/source/${variant}"
  mkdir -p "${destination}"
  curl --fail --location --retry 5 --connect-timeout 15 --max-time 180 \
    "https://api.github.com/repos/inclusionAI/sandboxd/git/commits/${sha}" \
    -o "${evidence}/commit-${variant}.json"
  python3 - "${evidence}/commit-${variant}.json" "${sha}" "${variant}" <<'PY'
import json, sys
commit = json.load(open(sys.argv[1]))
assert commit['sha'] == sys.argv[2]
if sys.argv[3] == 'pr':
    assert {p['sha'] for p in commit['parents']} == {
        '7f16edd2254c94d968bce3fcdea8a6df4e40116e',
        '8d52e1dd9fa02ab2ffba848e63374b7e999ad42f',
    }
PY
  archive="${task_root}/${variant}.tar.gz"
  timeout --signal=TERM --kill-after=5s 180s git -C "${task_root}/verified-git" \
    fetch --no-tags --depth=1 https://github.com/inclusionAI/sandboxd.git "${sha}"
  [[ "$(git -C "${task_root}/verified-git" rev-parse FETCH_HEAD)" == "${sha}" ]]
  [[ "$(git -C "${task_root}/verified-git" cat-file -t "${sha}")" == commit ]]
  git -C "${task_root}/verified-git" archive --format=tar.gz "${sha}" >"${archive}"
  sha256sum "${archive}" >>"${evidence}/source-archives.sha256"
  tar -xzf "${archive}" -C "${destination}"
done

for file in .github/workflows/ci.yml test/e2e/run.sh test/e2e/e2e-run.sh \
  test/e2e/checkpoint-restore/main.go test/e2e/build-binaries.sh \
  test/e2e/runtime-suite.Dockerfile third_party/runtime-versions.env \
  pkg/runtime/firecracker/checkpoint_handler.go cmd/firecracker-agent/main.go; do
  cmp "${task_root}/source/base/${file}" "${task_root}/source/pr/${file}"
  sha256sum "${task_root}/source/base/${file}" >>"${evidence}/common-source.sha256"
done
for variant in base pr; do
  (cd "${task_root}/source/${variant}"; make e2e-runtime-binaries) \
    >"${evidence}/build-${variant}.log" 2>&1
  sha256sum "${task_root}/source/${variant}/output/sandboxd" \
    >>"${evidence}/sandboxd-binaries.sha256"
done

cd "${task_root}/source/base"
source third_party/runtime-versions.env
build_args=(--build-arg E2E_RUNTIME=firecracker --build-arg E2E_VIRTIOFS=0)
for name in VIRTIOFSD_REVISION GVISOR_RELEASE GVISOR_AMD64_SHA512 GVISOR_AMD64_URL \
  RUNC_VERSION RUNC_AMD64_SHA256 RUNC_RELEASE_BASE_URL KATA_RELEASE \
  KATA_AMD64_SHA256 KATA_RELEASE_BASE_URL FIRECRACKER_RELEASE \
  FIRECRACKER_AMD64_SHA256 FIRECRACKER_AMD64_URL; do
  build_args+=(--build-arg "${name}=${!name}")
done
tag="pr73-firecracker-diagnostic:${GITHUB_RUN_ID:?}"
docker build "${build_args[@]}" -f test/e2e/runtime-suite.Dockerfile -t "${tag}" . \
  >"${evidence}/image-build.log" 2>&1
image="$(docker image inspect --format '{{.Id}}' "${tag}")"
printf '%s\n' "${image}" >"${evidence}/common-image.txt"
docker run --rm --entrypoint /bin/sh "${image}" -c \
  'sha256sum /usr/local/bin/sbox /usr/local/bin/firecracker-agent /usr/local/bin/checkpoint-restore /usr/local/bin/firecracker /opt/firecracker/vmlinux /opt/firecracker/initrd.img /usr/local/bin/sandboxd-e2e-run; cat /opt/firecracker/manifest.json' \
  >"${evidence}/common-runtime-manifest.txt"
export FC_DIAG_REAL_DOCKER="$(command -v docker)" FC_DIAG_SCRIPTS="${scripts}"
export DOCKER="${scripts}/docker-observer.sh"
chmod +x "${DOCKER}"
round=0
any_failure=0
for variant in base pr pr base base pr pr base base pr pr base base pr pr base base pr pr base; do
  remaining=$(( deadline - $(date +%s) - 120 ))
  if (( remaining < 120 )); then
    echo 'INCOMPLETE: batch deadline reached; preserving time for cleanup/upload.' \
      | tee -a "${evidence}/results.tsv"
    exit 1
  fi
  round_timeout=900
  (( remaining >= round_timeout )) || round_timeout=${remaining}
  (( round += 1 ))
  export FC_DIAG_CONTAINER="pr73-diag-${GITHUB_RUN_ID}-${round}-${variant}"
  export FC_DIAG_EVIDENCE="${evidence}/round-${round}-${variant}"
  export FC_DIAG_BINARY="${task_root}/source/${variant}/output/sandboxd"
  mkdir -p "${FC_DIAG_EVIDENCE}"
  free_kib="$(df -Pk "${task_root}" | awk 'NR==2 {print $4}')"
  (( free_kib >= 8*1024*1024 )) || { echo 'Low disk space; stopping.'; exit 1; }
  date -u '+%Y-%m-%dT%H:%M:%S.%NZ' >"${FC_DIAG_EVIDENCE}/started.txt"
  set +e
  E2E_RUNTIME=firecracker E2E_RUNSC_PLATFORM=systrap E2E_RUN_CGROUP_DISABLED=0 \
    E2E_FIRECRACKER_CHECKPOINT_MODE='' E2E_FIRECRACKER_VIRTIOFS=0 \
    E2E_NETWORK_SOAK=1 E2E_SKIP_BUILD=1 RUN_UNIT_TESTS=0 \
    SANDBOXD_E2E_IMAGE="${image}" SANDBOXD_E2E_CONTAINER="${FC_DIAG_CONTAINER}" \
    timeout --signal=TERM --kill-after=75s "${round_timeout}s" bash test/e2e/run.sh \
    >"${FC_DIAG_EVIDENCE}/stdout.log" 2>&1
  status=$?
  set -e
  printf '%s\t%s\t%s\n' "${round}" "${variant}" "${status}" \
    | tee -a "${evidence}/results.tsv"
  (( status == 0 )) || any_failure=1
  docker ps -a --format '{{.Names}}' >"${FC_DIAG_EVIDENCE}/remaining-containers.txt"
  df -h >"${FC_DIAG_EVIDENCE}/disk-after.txt"
  sudo losetup --list --output NAME,BACK-FILE,AUTOCLEAR,RO \
    >"${FC_DIAG_EVIDENCE}/loops-after.txt"
  findmnt --list >"${FC_DIAG_EVIDENCE}/mounts-after.txt"
  sudo find /sys/fs/cgroup/sandboxd-e2e -maxdepth 4 -type f -name cgroup.procs \
    -print -exec cat {} \; >"${FC_DIAG_EVIDENCE}/cgroups-after.txt" 2>&1 || true
  if grep -Fq "${FC_DIAG_CONTAINER}" "${FC_DIAG_EVIDENCE}/remaining-containers.txt"; then
    echo 'Test container cleanup incomplete; stopping.' >&2; exit 1
  fi
  if grep -Fq '/sandboxd-e2e-home.' "${FC_DIAG_EVIDENCE}/loops-after.txt" || \
     grep -Fq '/sandboxd-e2e-home.' "${FC_DIAG_EVIDENCE}/mounts-after.txt"; then
    echo 'E2E fixture loop/mount cleanup incomplete; stopping.' >&2; exit 1
  fi
  if grep -Eq '^[0-9]+$' "${FC_DIAG_EVIDENCE}/cgroups-after.txt"; then
    echo 'E2E cgroup has remaining processes; stopping.' >&2; exit 1
  fi
  if grep -Eq 'restored process stopped: [0-9]+ -> [0-9]+' "${FC_DIAG_EVIDENCE}/stdout.log"; then
    echo 'Original stall assertion reproduced; stop batch and preserve evidence.'
    exit 1
  fi
done
capture_host >"${evidence}/host-after.txt" 2>&1
exit "${any_failure}"
