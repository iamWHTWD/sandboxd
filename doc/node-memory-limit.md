# Optional aggregate sandbox memory limit

This opt-in cgroup v2 policy caps the configured sandbox parent (for example `/sandboxd` or `/akernel`) at startup and clips node-resource reporting to the same budget. It is disabled by default. Deployments retain ownership of budgets and of limits on components outside the parent.

```toml
[plugin.resource.memory_limit]
enabled = true
system_reserve = "4GiB"
control_reserve = "auto"
other_reserve = "8GiB"
safety_buffer = "8GiB"
runtime_headroom = "2GiB"
```

These settings extend an existing sandboxd configuration. The public sample uses `/sandboxd`; AKernel deployments can select `/akernel` through `cgroup_root_name`. Reporting additionally requires `[plugin.node_resource]` with `sock_path` and a `cgroup` or `kubernetes` provider. Without that module, the parent hard limit still applies. These are examples, not production defaults. Sizes accept integer bytes or `B`, `KiB`, `MiB`, `GiB`, `TiB`. System, control and safety reserves must be positive. Other reserve and runtime headroom default to zero. Control reserve can be an explicit size or `auto`, which requires a positive finite `memory.max` on the visible `/system.slice`. Distill-fs's positive finite `[plugin.image].cgroup_memory_limit` supplies its budget; it is rounded up to a page. No current-use samples or historical peak database are required.

The calculation is:

```text
T = min(/proc/meminfo MemTotal, finite memory.max values on parent ancestors)
R = system_reserve + control_reserve + distill-fs budget + other_reserve
candidate = round_down_to_page(T - R - safety_buffer)
actual_max = min(candidate, existing finite parent memory.max)
reported_capacity = min(provider_capacity, actual_max - runtime_headroom)
/resource Mem = reported_capacity - active transient reservations
```

The parent's own old limit is excluded when calculating T, so restarts do not subtract budgets repeatedly. At a real hierarchy root there may be no `memory.max`; a Docker cgroup namespace root can expose a finite container limit, which is included. Only visible ancestors can be inspected: container memory limits must represent the delegated node budget if higher host ancestors are hidden. The control daemon's current group is not an ancestor of a sibling `/akernel`.

For example, T=64GiB, system=4GiB, control=6GiB, distill-fs=8GiB, other=4GiB and safety=4GiB give candidate=38GiB. With runtime headroom=2GiB, the reporting ceiling is 36GiB. If an existing parent cap is 32GiB, it is preserved and the reporting ceiling is 30GiB. A Kubernetes provider reporting 28GiB yields 28GiB: its already-reserved capacity is clipped, not reduced by R a second time. Sandbox live usage is not subtracted from this capacity; the scheduler already tracks sandbox allocations and subtracts them from capacity.

Cgroup-provider memory capacity comes from the managed parent and its ancestors rather than sandboxd's own service group. CPU discovery is unchanged. Cgroup and Kubernetes providers preserve byte precision instead of rounding down to whole GiB. This precision change also applies with the policy disabled. YuanRong converts resource units internally; its view may have a small conversion tolerance.

Enable or change the policy on a drained node. Adding or lowering a parent limit requires the entire subtree to have no processes and residual charged memory to fit the new limit. An already equally strict or stricter cap can be reused with live sandboxes. Startup refuses invalid configuration, exhausted budgets, missing controllers, failed writes and mismatched readback. Headroom must fit under the actual cap. The implementation changes only `memory.max`; `memory.high`, swap policy and group OOM policy retain existing settings. Policy changes take effect on a controlled restart.

New nodes with the same image and configuration calculate T at startup; different node sizes therefore receive different caps without a per-node hardcoded limit. Reserves remain deployment policy and must be updated when component budgets change. `other_reserve` includes checkpoint and other components outside the sandbox parent; headroom covers costs inside it. Avoid counting a component in both reserves. Fixed reserves alone do not constrain outside components, so this feature cannot guarantee avoidance of all host OOMs. Continue enforcing their deployment limits and monitoring node memory pressure.
