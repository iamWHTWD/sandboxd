#!/usr/bin/env bash
# External, read-only observer. Never signals or ptraces a business process.
# Run inside the original CI container, with /evidence bound to runner storage.
set -uo pipefail

readonly TARGET=sbox-e2e-firecracker-cr-target
SOCKET=${SANDBOXD_SOCKET:-/run/sandboxd/sandboxd.sock}
EVIDENCE=${EVIDENCE:-/evidence}
SBOX=${OBS_SBOX:-/usr/local/bin/sbox}
PROC=${OBS_PROC_ROOT:-/proc}
CGROUP=${OBS_CGROUP_ROOT:-/sys/fs/cgroup}
STATE_ROOT=${OBS_STATE_ROOT:-/home/akernel/sandboxd/root/containers}
CONFIG=${OBS_CONFIG:-/home/akernel/sandboxd/config.toml}
CONSOLE_ROOT=${OBS_CONSOLE_ROOT:-/var/log/sandboxd}
FIRST=${OBS_STALL_FIRST:-10}
SECOND=${OBS_STALL_SECOND:-30}
POLL=${OBS_POLL_SECONDS:-2}
DEADLINE=${OBS_DEADLINE_SECONDS:-3600}
SELF=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/$(basename -- "${BASH_SOURCE[0]}")

clock_tag() {
  if command -v python3 >/dev/null 2>&1; then
    python3 -c 'import time; print("wall_ns=%d monotonic_ns=%d" % (time.time_ns(), time.monotonic_ns()))'
  else
    printf 'wall=%s monotonic_source=/proc/uptime uptime=' "$(date -u '+%Y-%m-%dT%H:%M:%S.%NZ')"
    cat /proc/uptime 2>&1
  fi
}

mono_seconds() {
  if command -v python3 >/dev/null 2>&1; then
    python3 -c 'import time; print(int(time.monotonic()))'
  else
    awk '{ printf "%.0f\n", int($1) }' /proc/uptime
  fi
}

read_file() {
  local path=$1
  printf '\n[file %s]\n' "$path"
  # Errors, including permission denied, remain in the evidence.
  head -c 1048576 -- "$path" 2>&1 || true
  printf '\n'
}

target_signature() {
  local proc comm='' arg next=0 matched=0 stat='' rest='' start=''
  local -a fields=()
  for proc in "$PROC"/[0-9]*; do
    [[ -r "$proc/comm" ]] || continue
    IFS= read -r comm <"$proc/comm" || continue
    [[ "$comm" == firecracker* ]] || continue
    next=0; matched=0
    while IFS= read -r -d '' arg; do
      if (( next )); then [[ "$arg" == "$TARGET" ]] && matched=1; next=0; fi
      [[ "$arg" == --id ]] && next=1
      [[ "$arg" == "--id=$TARGET" ]] && matched=1
    done <"$proc/cmdline"
    (( matched )) || continue
    IFS= read -r stat <"$proc/stat" || continue
    rest=${stat##*) }
    read -r -a fields <<<"$rest"
    start=${fields[19]:-unknown}
    printf '%s-%s\n' "${proc##*/}" "$start"
  done | sort | paste -sd, -
}

snapshot_cgroup() {
  local dir=$1 file
  [[ -d "$dir" ]] || return 0
  for file in cgroup.procs cgroup.events cgroup.freeze memory.current memory.max memory.high \
    memory.events memory.events.local memory.pressure cpu.max cpu.stat cpu.pressure \
    io.stat io.max io.pressure pids.current pids.max; do
    [[ ! -e "$dir/$file" ]] || read_file "$dir/$file"
  done
}

snapshot_host() {
  local proc comm file task limit rel line path count=0
  clock_tag
  printf '\n[container process/thread tree]\n'
  ps -eLo pid,ppid,tid,stat,wchan:40,comm,args 2>&1 || true
  printf '\n[target persisted state: includes pid, vcpus, memory_mib]\n'
  read_file "$STATE_ROOT/$TARGET/firecracker/state.json"
  printf '\n[effective E2E configuration]\n'
  read_file "$CONFIG"
  for file in /proc/uptime /proc/stat /proc/diskstats /proc/pressure/cpu \
    /proc/pressure/io /proc/pressure/memory; do read_file "$file"; done
  # Only the target VMM and sandboxd are inspected. No host-wide cgroup walk.
  for proc in "$PROC"/[0-9]*; do
    [[ -r "$proc/comm" ]] || continue
    IFS= read -r comm <"$proc/comm" || continue
    if [[ "$comm" == firecracker* ]]; then
      tr '\0' '\n' <"$proc/cmdline" | grep -Fx -- "$TARGET" >/dev/null ||
        tr '\0' '\n' <"$proc/cmdline" | grep -Fx -- "--id=$TARGET" >/dev/null || continue
      limit=64
    elif [[ "$comm" == sandboxd ]]; then limit=128
    else continue; fi
    printf '\n[process %s %s]\n' "$proc" "$comm"
    printf '[cmdline] '; tr '\0' ' ' <"$proc/cmdline" 2>&1; printf '\n'
    for file in stat status io sched cgroup limits; do read_file "$proc/$file"; done
    count=0
    for task in "$proc"/task/[0-9]*; do
      [[ -d "$task" ]] || continue
      (( count += 1 )); (( count <= limit )) || break
      for file in comm stat wchan syscall stack sched; do read_file "$task/$file"; done
    done
    # Resolve only cgroups named by an inspected process. Never ascend/walk /.
    while IFS=: read -r _ _ rel; do
      [[ "$rel" == /* && "$rel" != *'/../'* && "$rel" != */.. ]] || continue
      path="$CGROUP$rel"
      snapshot_cgroup "$path"
    done <"$proc/cgroup"
  done
  # This is the known E2E subtree, rather than the host cgroup root.
  count=0
  while IFS= read -r path; do
    (( count += 1 )); (( count <= 64 )) || break
    snapshot_cgroup "$path"
  done < <(find "$CGROUP/sandboxd-e2e" -maxdepth 4 -type d 2>&1 | sort)
  for file in checkpoint-workload.stdout checkpoint-runtime.stderr; do
    printf '\n[checkpoint console %s]\n' "$file"
    tail -400 "$CONSOLE_ROOT/$file" 2>&1 || true
  done
  clock_tag
}

snapshot_guest() {
  clock_tag
  "$SBOX" --address "$SOCKET" --timeout 5s exec "$TARGET" /bin/sh -c '
    echo "[guest sample begins]"; date -u
    for file in /proc/uptime /proc/stat /proc/loadavg /proc/diskstats /proc/interrupts /proc/softirqs \
      /proc/timer_list /proc/pressure/cpu /proc/pressure/io /proc/pressure/memory \
      /sys/devices/system/clocksource/clocksource0/current_clocksource \
      /sys/devices/system/clocksource/clocksource0/available_clocksource \
      /sys/devices/system/cpu/online /var/checkpoint-counter /var/checkpoint-generation; do
      echo "[file $file]"; head -c 262144 "$file" 2>&1 || true
    done
    count=0
    for proc in /proc/[0-9]*; do
      test -d "$proc" || continue
      count=$((count+1)); test "$count" -le 96 || break
      echo "[process $proc]"; tr "\000" " " <"$proc/cmdline" 2>&1; echo
      for file in status stat io timers; do echo "[file $proc/$file]"; cat "$proc/$file" 2>&1 || true; done
      threads=0
      for task in "$proc"/task/[0-9]*; do
        test -d "$task" || continue
        threads=$((threads+1)); test "$threads" -le 32 || break
        for file in comm children wchan syscall stack sched; do
          echo "[file $task/$file]"; cat "$task/$file" 2>&1 || true
        done
      done
    done
    echo "[guest dmesg tail]"; dmesg 2>&1 | tail -160
    echo "[guest sample ends]"; cat /proc/uptime; date -u
  '
  local status=$?
  clock_tag
  return "$status"
}

instance_preflight() {
  local dir=$1 signature=$2 item pid proc comm task file rc count current
  mkdir -p "$dir"
  printf 'signature=%s\nThis is a read-permission preflight, not a stall diagnosis.\n' "$signature" >"$dir/preflight.txt"
  clock_tag >>"$dir/preflight.txt"
  current=$(target_signature)
  printf 'current_signature=%s\n' "$current" >>"$dir/preflight.txt"
  [[ "$current" == "$signature" ]] || printf 'target changed before preflight; target reads skipped\n' >>"$dir/preflight.txt"
  # Hash the actual executing inode through /proc/PID/exe, not a pathname
  # that might have been replaced since this process started.
  for proc in "$PROC"/[0-9]*; do
    [[ -r "$proc/comm" ]] || continue
    IFS= read -r comm <"$proc/comm" || continue
    pid=${proc##*/}
    if [[ "$comm" == sandboxd || ( "$current" == "$signature" && ",$signature," == *",$pid-"* ) ]]; then
      {
        printf '\n[actual executable pid=%s comm=%s]\n' "$pid" "$comm"
        readlink "$proc/exe" 2>&1 || true
        if command -v sha256sum >/dev/null 2>&1; then sha256sum "$proc/exe" 2>&1
        else shasum -a 256 "$proc/exe" 2>&1; fi
        printf 'hash_read_rc=%s\n' "$?"
      } >>"$dir/preflight.txt"
    fi
    [[ "$current" == "$signature" && ",$signature," == *",$pid-"* ]] || continue
    count=0
    for task in "$proc" "$proc"/task/[0-9]*; do
      [[ -d "$task" ]] || continue
      (( count += 1 )); (( count <= 64 )) || break
      for file in stack syscall; do
        item="$dir/pid-$pid-task-${task##*/}-$file.txt"
        rc=0; cat "$task/$file" >"$item" 2>&1 || rc=$?
        printf 'read_path=%s read_rc=%s evidence=%s\n' "$task/$file" "$rc" "${item##*/}" >>"$dir/preflight.txt"
      done
    done
  done
  clock_tag >>"$dir/preflight.txt"
}

snapshot_pair() {
  local dir=$1 signature=$2 reason=$3 sample pid host_rc guest_rc file
  mkdir -p "$dir"
  printf 'signature=%s\nreason=%s\n' "$signature" "$reason" >"$dir/trigger.txt"
  clock_tag >>"$dir/trigger.txt"
  # Preserve the real checkpoint console, never the unrelated basic VM logs.
  # Bound copies to 8 MiB and record the limit explicitly.
  for file in checkpoint-workload.stdout checkpoint-runtime.stderr; do
    head -c 8388608 "$CONSOLE_ROOT/$file" >"$dir/$file" 2>"$dir/$file.copy-stderr" || true
  done
  printf 'checkpoint console copies limited to first 8388608 bytes per file\n' >"$dir/console-copy-limit.txt"
  for sample in 1 2; do
    timeout --signal=TERM --kill-after=1s 6s bash "$SELF" host \
      >"$dir/host-$sample.txt" 2>&1 &
    pid=$!
    guest_rc=0
    timeout --signal=TERM --kill-after=1s 6s bash "$SELF" guest \
      >"$dir/guest-$sample.txt" 2>&1 || guest_rc=$?
    host_rc=0
    wait "$pid" || host_rc=$?
    printf 'host_rc=%s guest_rc=%s\n' "$host_rc" "$guest_rc" >"$dir/status-$sample.txt"
    [[ "$sample" != 1 ]] || sleep 2
  done
  clock_tag >"$dir/complete.txt"
}

observe() {
  local signature='' previous_signature='' seen=0 previous='' stable_since=0 failed_since=-1
  local numeric_stage=0 failed_stage=0 dumps=0 active='' now begin value rc=0 reason='' dir=''
  local stopping=0 instance=0 count=0 elapsed=0 threshold
  local -a preflights=()
  mkdir -p "$EVIDENCE"
  begin=$(mono_seconds)
  trap 'stopping=1' TERM INT
  while (( ! stopping )); do
    now=$(mono_seconds); (( now - begin < DEADLINE )) || break
    [[ -z "${OBS_STOP_FILE:-}" || ! -e "$OBS_STOP_FILE" ]] || break
    signature=$(target_signature)
    if [[ -n "$signature" && "$signature" != "$previous_signature" ]]; then
      (( instance += 1 )); seen=1; previous_signature=$signature
      previous=''; stable_since=$now; failed_since=-1; numeric_stage=0; failed_stage=0; dumps=0
      printf 'instance=%s signature=%s ' "$instance" "$signature" >>"$EVIDENCE/observer.log"
      clock_tag >>"$EVIDENCE/observer.log"
      timeout --signal=TERM --kill-after=1s 6s bash "$SELF" preflight \
        "$EVIDENCE/instance-$instance-preflight" "$signature" \
        >"$EVIDENCE/instance-$instance-preflight-launch.log" 2>&1 &
      preflights+=("$!")
    fi
    rc=0
    value=$(timeout --signal=TERM --kill-after=1s 4s "$SBOX" --address "$SOCKET" \
      --timeout 2s exec "$TARGET" /bin/cat /var/checkpoint-counter \
      2>"$EVIDENCE/counter-read-last.stderr") || rc=$?
    now=$(mono_seconds); reason=''
    printf 'instance=%s signature=%s rc=%s counter=%q ' "$instance" "$previous_signature" "$rc" "$value" \
      >>"$EVIDENCE/observer.log"
    clock_tag >>"$EVIDENCE/observer.log"
    if [[ -n "$active" ]] && ! kill -0 "$active" 2>/dev/null; then wait "$active" || true; active=''; fi
    if (( seen )); then
      if (( rc == 0 )) && [[ "$value" =~ ^[0-9]+$ ]]; then
        failed_since=-1; failed_stage=0
        if [[ "$value" != "$previous" ]]; then
          previous=$value; stable_since=$now; numeric_stage=0
        else
          threshold=$FIRST; (( numeric_stage == 0 )) || threshold=$SECOND
          if (( numeric_stage < 2 && now - stable_since >= threshold )); then reason=numeric-stall; fi
        fi
      else
        (( failed_since >= 0 )) || failed_since=$now
        threshold=$FIRST; (( failed_stage == 0 )) || threshold=$SECOND
        if (( failed_stage < 2 && now - failed_since >= threshold )); then reason=counter-read-failure; fi
      fi
      if [[ -n "$reason" && -z "$active" ]] && (( dumps < 4 )); then
        (( dumps += 1 )); (( count += 1 )); elapsed=$(( now - begin ))
        dir="$EVIDENCE/instance-$instance-dump-$count-$reason-t$elapsed"
        printf 'trigger=%s directory=%s signature=%s ' "$reason" "$dir" "$previous_signature" \
          >>"$EVIDENCE/observer.log"
        clock_tag >>"$EVIDENCE/observer.log"
        timeout --signal=TERM --kill-after=1s 19s bash "$SELF" pair "$dir" "$previous_signature" "$reason" \
          >"$dir-launch.log" 2>&1 &
        active=$!
        if [[ "$reason" == numeric-stall ]]; then (( numeric_stage += 1 )); else (( failed_stage += 1 )); fi
      fi
    fi
    sleep "$POLL"
  done
  # Only wait for our own bounded diagnostic child. Never kill workload PIDs.
  [[ -z "$active" ]] || wait "$active" || true
  for active in "${preflights[@]}"; do wait "$active" || true; done
  printf 'observer-exit instances=%s dumps=%s ' "$instance" "$count" >>"$EVIDENCE/observer.log"
  clock_tag >>"$EVIDENCE/observer.log"
}

case ${1:-run} in
  run) observe ;;
  host) snapshot_host ;;
  guest) snapshot_guest ;;
  pair) snapshot_pair "${2:?directory}" "${3:?signature}" "${4:?reason}" ;;
  preflight) instance_preflight "${2:?directory}" "${3:?signature}" ;;
  signature) target_signature ;;
  *) printf 'Usage: %s [run|host|guest|pair DIR SIGNATURE REASON|signature]\n' "$0" >&2; exit 2 ;;
esac
