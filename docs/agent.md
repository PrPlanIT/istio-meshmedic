# MeshMedic node-agent — operations, scan model & performance

The agent (`meshmedic agent`) is the production form: a DaemonSet, one pod per
node, that keeps the ambient mesh enrolled without ever injecting a container.

## Scan model

Each sweep (every `--interval`, default **60s**) the agent:

1. **Lists** the ambient-annotated pods assigned to its node (one Kubernetes API
   call, field-selected by `spec.nodeName`).
2. **Maps** each pod to a PID by reading `/host/proc/<pid>/cgroup` and matching the
   `pod<uid>` segment (handles both cgroupfs-dash and systemd-underscore forms).
   `hostPID: true` makes every host process visible; any container in a pod shares
   the netns, so the first PID found suffices.
3. **Reads** `/host/proc/<pid>/net/tcp{,6}` and checks for ztunnel's in-pod capture
   listeners (`15001` out, `15006`/`15008` in, `15053` DNS). No exec, no ephemeral
   container, no Linux capabilities — reading `/proc/net/tcp` is cap-free; reaching
   another pod's netns needs only `hostPID` + root.

Mesh enrollment is assessed **independently of Kubernetes readiness** — see
[Classification](#classification). The sweep is read-only unless a repair flag is
set.

## Classification

Every ambient pod with a live netns on this node (terminal `Succeeded`/`Failed`
pods are skipped) lands in one of four boxes:

| | Captured | Orphaned |
| --- | --- | --- |
| **Ready** | healthy | capture lost on a healthy pod → `--auto-repair` |
| **Not-Ready** | someone else's problem (app/dep) — left alone | "unhealthy **because** orphaned" → `--repair-stuck` |

The scanner records `Ready` + `NotReadyDuration` as metadata; it never gates
detection on them. A pod orphaned badly enough to be knocked not-Ready (init never
completes, a dependency is unreachable) is exactly the orphan a readiness gate
would hide — in production this had hidden whole stacks for **weeks**.

## Repair policy (opt-in, two tiers)

- `--auto-repair` — restart **Ready** orphans. A healthy pod that merely lost
  capture is almost always fixed by a fresh enrollment.
- `--repair-stuck` — restart **not-Ready** orphans, but only once a pod has remained
  **continuously not-Ready longer than `--grace-period`** (default 5m) — the
  actionable invariant, not an enrollment-timing guess. This is the stronger claim
  and a notch riskier, hence a separate flag.

Both paths re-read the netns after `--confirm` (flap guard) and only act if the pod
is *still* orphaned. Repair is a **pod restart** (delete; the controller recreates
with a fresh enrollment) — durable, unlike the label toggle which flaps.

**Why it can't churn:** because the not-Ready path requires `orphaned`, a pod that
re-enrolls after a restart becomes *captured* and drops out of scope — even if it's
still not-Ready for an app reason. The agent restarts a stuck orphan at most a few
times until enrollment sticks, then hands off.

## Performance (measured, ~10-node cluster)

| metric | value |
| --- | --- |
| Memory (RSS) | **10–16 MiB** per agent |
| CPU, idle | **1–2 m** |
| CPU, mid-sweep | brief spikes to **~18–28 m** (the `/host/proc` walk) |
| Goroutines | ~10 |
| Sweep cadence | `--interval` (60s default) |

No CPU limit is set, so the per-sweep spike runs unthrottled and finishes in well
under a second; the memory limit is `128Mi` against a `32Mi` request. **Cost scales
with the number of processes on the node** (every `/proc/<pid>/cgroup` is read under
`hostPID`) and the number of ambient pods — so the heaviest nodes see the largest
(still sub-second) spike. For very dense nodes, raise the CPU request toward the
observed spike if scheduling fairness matters; memory is flat.

## Metrics

Prometheus `/metrics` on `--metrics-addr` (default `:9100`); the DaemonSet carries
`prometheus.io/scrape` annotations.

| metric | type | meaning |
| --- | --- | --- |
| `meshmedic_orphans_total` | gauge | orphans this sweep (this node) |
| `meshmedic_orphans_ready` | gauge | Ready + orphaned |
| `meshmedic_orphans_not_ready` | gauge | not-Ready + orphaned |
| `meshmedic_orphans_stuck` | gauge | not-Ready orphans past the grace period |
| `meshmedic_orphans_repaired_total{class}` | counter | restarts, `class=ready\|stuck` |
| `meshmedic_sweeps_total` / `meshmedic_sweep_errors_total` | counter | sweep health |

`meshmedic_orphans_stuck > 0` is the alert-worthy signal: an orphan a readiness
gate used to hide is sitting there. Alert on it being non-zero for longer than a
grace period or two.

## Tuning

- `--interval` — scan cadence vs. detection latency (60s is cheap; lower only if you
  need faster reaction).
- `--grace-period` — how long a pod must be continuously not-Ready before the stuck
  path will touch it. Raise it for workloads with legitimately long startups.
- `--auto-repair` / `--repair-stuck` — off by default (detect-and-surface). Enable
  per the risk tolerance above.

See [`deploy/meshmedic-daemonset.yaml`](../deploy/meshmedic-daemonset.yaml) for the
reference manifest (RBAC, `hostPID`, `/host/proc` mount, scrape annotations).
