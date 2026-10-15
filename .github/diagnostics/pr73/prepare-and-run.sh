#!/usr/bin/env bash
# Diagnostic builds only; never run on ECS or modify the PR branch.
set -Eeuo pipefail
[[ "${GITHUB_ACTIONS:-}" == true && "${RUNNER_ENVIRONMENT:-}" == github-hosted ]] || {
  echo 'Refusing to run outside a dedicated GitHub-hosted runner.' >&2; exit 2;
}
mode="${1:?build|round|finish}"
task_root="${FC_DIAG_ROOT:?}"
deadline=$(( ${FC_DIAG_JOB_START:-$(date +%s)} + 75*60 ))
scripts="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
evidence="${task_root}/evidence"
compile="${task_root}/compile"
pr=5954c7a286112e4cc88bf0e8a853896d1260ffe5
mkdir -p "${evidence}"

capture_host() {
  date -u '+%Y-%m-%dT%H:%M:%S.%NZ'
  uname -a; lscpu
  systemd-detect-virt || true
  docker version; docker info
  containerd --version || true
  runc --version || true
  go version; df -h; cat /proc/uptime
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

source_manifest() {
  python3 - "${task_root}/verified-git" "${pr}" "$1" "$2" <<'PY_MANIFEST'
import hashlib, json, os, pathlib, subprocess, sys
root = pathlib.Path(sys.argv[3])
entries = subprocess.check_output(['git', '-C', sys.argv[1], 'ls-tree', '-r', '-z', sys.argv[2]])
manifest = {}
for entry in entries.split(b'\0'):
    if not entry:
        continue
    metadata, relative = entry.split(b'\t', 1)
    mode, kind, oid = metadata.decode().split()
    name = os.fsdecode(relative)
    path = root / name
    data = os.fsencode(os.readlink(path)) if mode == '120000' else (
        oid.encode() if kind == 'commit' else path.read_bytes())
    manifest[name] = hashlib.sha256(data).hexdigest()
pathlib.Path(sys.argv[4]).write_text(json.dumps(manifest, sort_keys=True, indent=2) + '\n')
PY_MANIFEST
}

build_variants() {
  capture_host >"${evidence}/host-before.txt" 2>&1
  sudo losetup --list --output NAME,BACK-FILE,AUTOCLEAR,RO >"${evidence}/loops-before.txt"
  findmnt --list >"${evidence}/mounts-before.txt"
  printf 'pr-ci-merge=%s\npr-head=%s\nvariable=resource Start position only\n' \
    "${pr}" 8d52e1dd9fa02ab2ffba848e63374b7e999ad42f >"${evidence}/revisions.txt"
  git init --quiet "${task_root}/verified-git"
  timeout --signal=TERM --kill-after=5s 180s git -C "${task_root}/verified-git" \
    fetch --no-tags --depth=1 https://github.com/inclusionAI/sandboxd.git "${pr}"
  [[ "$(git -C "${task_root}/verified-git" rev-parse FETCH_HEAD)" == "${pr}" ]]
  python3 - "${task_root}/verified-git" "${pr}" "${evidence}/commit-pr.json" <<'PY_COMMIT'
import json, pathlib, subprocess, sys
raw = subprocess.check_output(['git', '-C', sys.argv[1], 'cat-file', '-p', sys.argv[2]], text=True)
headers = raw.split('\n\n', 1)[0].splitlines()
parents = [line.split(' ', 1)[1] for line in headers if line.startswith('parent ')]
assert set(parents) == {'7f16edd2254c94d968bce3fcdea8a6df4e40116e', '8d52e1dd9fa02ab2ffba848e63374b7e999ad42f'}, parents
tree = next(line.split(' ', 1)[1] for line in headers if line.startswith('tree '))
pathlib.Path(sys.argv[3]).write_text(json.dumps({'sha': sys.argv[2], 'tree': tree, 'parents': parents}, indent=2) + '\n')
PY_COMMIT
  git -C "${task_root}/verified-git" archive --format=tar.gz "${pr}" >"${task_root}/pr.tar.gz"
  sha256sum "${task_root}/pr.tar.gz" >"${evidence}/source-archives.sha256"
  mkdir -p "${compile}" "${task_root}/source/pr"
  tar -xzf "${task_root}/pr.tar.gz" -C "${compile}"
  tar -xzf "${task_root}/pr.tar.gz" -C "${task_root}/source/pr"
  cp -a "${task_root}/source/pr" "${task_root}/source/early"
  # This experiment is only valid for the original opt-out E2E configuration.
  ! grep -Fq '[plugin.resource.memory_limit]' "${compile}/test/e2e/e2e-run.sh"
  source_manifest "${compile}" "${evidence}/source-pr.json"
  echo 'Building unchanged PR sandboxd and common helpers'
  (cd "${compile}"; make e2e-runtime-binaries) >"${evidence}/build-pr.log" 2>&1
  source_manifest "${compile}" "${evidence}/source-pr-after-build.json"
  cmp "${evidence}/source-pr.json" "${evidence}/source-pr-after-build.json"
  cp -a "${compile}/output" "${task_root}/source/pr/output"

  python3 - "${compile}/internal/server/server.go" "${evidence}" <<'PY_PATCH'
import difflib, hashlib, json, pathlib, sys
path = pathlib.Path(sys.argv[1])
evidence = pathlib.Path(sys.argv[2])
original = path.read_text()
start = '\n\t// Reporting starts with the effective parent ceiling already installed.\n'
end = '\n\t// health check from sandbox manager housekeeping.'
anchor = '\n\t// Construct the in-process image manager before sandboxService'
assert original.count(start) == original.count(end) == original.count(anchor) == 1
begin, finish = original.index(start), original.index(end, original.index(start))
block = original[begin:finish]
assert block.count('nodeResMod.Start()') == 1 and original.count('nodeResMod.Start()') == 1
assert block.count('node-resource module ready') == 1
body = original[:begin] + original[finish:]
moved = block.replace(start, '\n\t// Diagnostic variant: start reporting before image/cgroup initialization.\n', 1)
patched = body.replace(anchor, moved + anchor, 1)
# Reverse the mechanical movement and comment replacement to prove no other edit.
reversed_body = patched.replace(moved + anchor, anchor, 1)
reversed_source = reversed_body.replace(end, block + end, 1)
assert reversed_source == original
assert patched.index('nodeResMod.Start()') < patched.index('imagemanager.NewModule(')
assert patched.index('defer func() {', patched.index('mod.SetXPUProvider')) < patched.index('nodeResMod.Start()')
path.write_text(patched)
(evidence / 'early-start.patch').write_text(''.join(difflib.unified_diff(original.splitlines(True), patched.splitlines(True), fromfile='a/internal/server/server.go', tofile='b/internal/server/server.go')))
(evidence / 'ablation.json').write_text(json.dumps({'variable': 'Start position only; same PR locks, byte capacity and reserve/release code', 'file': 'internal/server/server.go', 'compile_path': str(path.parent.parent.parent), 'original_sha256': hashlib.sha256(original.encode()).hexdigest(), 'early_sha256': hashlib.sha256(patched.encode()).hexdigest(), 'reverse_patch_exact': True, 'requires_memory_limit_disabled': True}, indent=2) + '\n')
PY_PATCH
  [[ -z "$(gofmt -l "${compile}/internal/server/server.go")" ]]
  cp "${compile}/internal/server/server.go" "${task_root}/source/early/internal/server/server.go"
  source_manifest "${compile}" "${evidence}/source-early.json"
  python3 - "${evidence}" <<'PY_DIFF'
import json, pathlib, sys
root = pathlib.Path(sys.argv[1])
control = json.loads((root / 'source-pr.json').read_text())
early = json.loads((root / 'source-early.json').read_text())
assert set(control) == set(early)
changed = [name for name in control if control[name] != early[name]]
assert changed == ['internal/server/server.go'], changed
(root / 'changed-source-files.json').write_text(json.dumps(changed) + '\n')
PY_DIFF
  echo 'Building early-Start sandboxd in the exact same compile path'
  (cd "${compile}"; make e2e-runtime-binaries) >"${evidence}/build-early.log" 2>&1
  source_manifest "${compile}" "${evidence}/source-early-after-build.json"
  cmp "${evidence}/source-early.json" "${evidence}/source-early-after-build.json"
  cp -a "${compile}/output" "${task_root}/source/early/output"
  for variant in early pr; do
    sha256sum "${task_root}/source/${variant}/output/sandboxd" >>"${evidence}/sandboxd-binaries.sha256"
    go version -m "${task_root}/source/${variant}/output/sandboxd" >"${evidence}/go-build-${variant}.txt"
  done
  cd "${task_root}/source/pr"
  source third_party/runtime-versions.env
  build_args=(--build-arg E2E_RUNTIME=firecracker --build-arg E2E_VIRTIOFS=0)
  for name in VIRTIOFSD_REVISION GVISOR_RELEASE GVISOR_AMD64_SHA512 GVISOR_AMD64_URL \
    RUNC_VERSION RUNC_AMD64_SHA256 RUNC_RELEASE_BASE_URL KATA_RELEASE \
    KATA_AMD64_SHA256 KATA_RELEASE_BASE_URL FIRECRACKER_RELEASE \
    FIRECRACKER_AMD64_SHA256 FIRECRACKER_AMD64_URL; do
    build_args+=(--build-arg "${name}=${!name}")
  done
  echo 'Building one common runtime image from unchanged PR helpers'
  tag="pr73-start-ablation:${GITHUB_RUN_ID:?}"
  docker build "${build_args[@]}" -f test/e2e/runtime-suite.Dockerfile -t "${tag}" . >"${evidence}/image-build.log" 2>&1
  image="$(docker image inspect --format '{{.Id}}' "${tag}")"
  printf '%s\n' "${image}" >"${evidence}/common-image.txt"
  docker run --rm --entrypoint /bin/sh "${image}" -c \
    'sha256sum /usr/local/bin/sbox /usr/local/bin/firecracker-agent /usr/local/bin/checkpoint-restore /usr/local/bin/firecracker /opt/firecracker/vmlinux /opt/firecracker/initrd.img /usr/local/bin/sandboxd-e2e-run; cat /opt/firecracker/manifest.json' >"${evidence}/common-runtime-manifest.txt"
  echo 'Both builds ready: only internal/server/server.go differs; all rounds share one image.'
}

run_round() {
  local round="${1:?round}" variant="${2:?early|pr}" remaining round_timeout status image free_kib
  [[ "${round}" =~ ^[0-9]+$ && ( "${variant}" == early || "${variant}" == pr ) ]]
  if [[ -f "${evidence}/STOP" ]]; then echo 'Batch already stopped; skipping remaining round.'; return 0; fi
  remaining=$(( deadline - $(date +%s) - 120 ))
  if (( remaining < 120 )); then echo 'INCOMPLETE: deadline reached' | tee "${evidence}/STOP"; return 1; fi
  round_timeout=900; (( remaining >= round_timeout )) || round_timeout=${remaining}
  image="$(cat "${evidence}/common-image.txt")"
  export FC_DIAG_REAL_DOCKER="$(command -v docker)" FC_DIAG_SCRIPTS="${scripts}"
  export DOCKER="${scripts}/docker-observer.sh"
  export FC_DIAG_CONTAINER="pr73-diag-${GITHUB_RUN_ID}-${round}-${variant}"
  export FC_DIAG_EVIDENCE="${evidence}/round-${round}-${variant}"
  export FC_DIAG_BINARY="${task_root}/source/${variant}/output/sandboxd"
  mkdir -p "${FC_DIAG_EVIDENCE}"
  free_kib="$(df -Pk "${task_root}" | awk 'NR==2 {print $4}')"
  if (( free_kib < 8*1024*1024 )); then echo 'Low disk space' | tee "${evidence}/STOP"; return 1; fi
  date -u '+%Y-%m-%dT%H:%M:%S.%NZ' >"${FC_DIAG_EVIDENCE}/started.txt"
  echo "ROUND ${round}: ${variant}; same PR source, only Start position differs"
  cd "${task_root}/source/pr"
  set +e
  E2E_RUNTIME=firecracker E2E_RUNSC_PLATFORM=systrap E2E_RUN_CGROUP_DISABLED=0 \
    E2E_FIRECRACKER_CHECKPOINT_MODE='' E2E_FIRECRACKER_VIRTIOFS=0 \
    E2E_NETWORK_SOAK=1 E2E_SKIP_BUILD=1 RUN_UNIT_TESTS=0 \
    SANDBOXD_E2E_IMAGE="${image}" SANDBOXD_E2E_CONTAINER="${FC_DIAG_CONTAINER}" \
    timeout --signal=TERM --kill-after=75s "${round_timeout}s" bash test/e2e/run.sh >"${FC_DIAG_EVIDENCE}/stdout.log" 2>&1
  status=$?
  set -e
  printf '%s\t%s\t%s\n' "${round}" "${variant}" "${status}" | tee -a "${evidence}/results.tsv"
  grep -E 'node-resource module ready|restored sandbox sbox-e2e-firecracker-cr-target|restored process stopped|e2e passed' \
    "${FC_DIAG_EVIDENCE}/sandboxd.log" "${FC_DIAG_EVIDENCE}/stdout.log" || true
  if [[ ! -f "${FC_DIAG_EVIDENCE}/config.toml" ]]; then
    echo 'Invalid experiment: effective configuration missing' | tee "${evidence}/STOP"; return 1
  fi
  if grep -Fq '[plugin.resource.memory_limit]' "${FC_DIAG_EVIDENCE}/config.toml"; then
    echo 'Invalid experiment: memory_limit section present' | tee "${evidence}/STOP"; return 1
  fi
  docker ps -a --format '{{.Names}}' >"${FC_DIAG_EVIDENCE}/remaining-containers.txt"
  df -h >"${FC_DIAG_EVIDENCE}/disk-after.txt"
  sudo losetup --list --output NAME,BACK-FILE,AUTOCLEAR,RO >"${FC_DIAG_EVIDENCE}/loops-after.txt"
  findmnt --list >"${FC_DIAG_EVIDENCE}/mounts-after.txt"
  sudo find /sys/fs/cgroup/sandboxd-e2e -maxdepth 4 -type f -name cgroup.procs -print -exec cat {} \; \
    >"${FC_DIAG_EVIDENCE}/cgroups-after.txt" 2>&1 || true
  if grep -Fq "${FC_DIAG_CONTAINER}" "${FC_DIAG_EVIDENCE}/remaining-containers.txt" || \
     grep -Fq '/sandboxd-e2e-home.' "${FC_DIAG_EVIDENCE}/loops-after.txt" || \
     grep -Fq '/sandboxd-e2e-home.' "${FC_DIAG_EVIDENCE}/mounts-after.txt" || \
     grep -Eq '^[0-9]+$' "${FC_DIAG_EVIDENCE}/cgroups-after.txt"; then
    echo 'Cleanup incomplete; stopping' | tee "${evidence}/STOP"; return 1
  fi
  if grep -Eq 'restored process stopped: [0-9]+ -> [0-9]+' "${FC_DIAG_EVIDENCE}/stdout.log"; then
    echo 'Original numeric stall reproduced; preserve evidence and stop.' | tee "${evidence}/STOP"
  fi
  return "${status}"
}

case "${mode}" in
  build) build_variants ;;
  round) run_round "${2:?round}" "${3:?early|pr}" ;;
  finish)
    capture_host >"${evidence}/host-after.txt" 2>&1
    python3 - "${evidence}" <<'PY_SUMMARY'
import collections, json, pathlib, sys
root = pathlib.Path(sys.argv[1])
rows = [line.split('\t') for line in (root / 'results.tsv').read_text().splitlines()] if (root / 'results.tsv').exists() else []
counts = collections.Counter(row[1] for row in rows)
passed = collections.Counter(row[1] for row in rows if row[2] == '0')
summary = {'completed': dict(counts), 'passed': dict(passed), 'rounds': len(rows), 'stop': (root / 'STOP').read_text() if (root / 'STOP').exists() else None}
(root / 'batch-summary.json').write_text(json.dumps(summary, indent=2) + '\n')
print(json.dumps(summary))
sys.exit(0 if len(rows) == 20 and all(row[2] == '0' for row in rows) and not summary['stop'] else 1)
PY_SUMMARY
    ;;
  *) echo "Unknown mode ${mode}" >&2; exit 2 ;;
esac
