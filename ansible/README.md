# istio-spiffe-reconciler — the original Ansible concept (superseded, frozen)

> **Status: archived. Not running anywhere, not planned for any further work.**
> This is the initial proof-of-concept that **MeshMedic (the Go node-agent in this
> repo) superseded.** It is kept for design lineage only.

## What it was

A read-only **Ansible** scanner (`base/configmap-playbook.yaml` → `scan.yml`) that
tried to find Istio ambient-mesh enrollment orphans from the **control plane**:

1. list pods annotated `ambient.istio.io/redirection: enabled` (the expected set),
2. exec into each ztunnel and read `:15000/config_dump` → `workloadState` keys (the
   enrolled set),
3. diff per node: `expected − enrolled = orphans`,
4. emit JSON + a human table + a Prometheus block.

It never got a `CronJob` or an overlay reference — it was scaffolding (RBAC + the
playbook ConfigMap) and nothing ran it. See [`base/NOTES.md`](base/NOTES.md) for the
original outstanding-work list and the 2026-05-30 incident that motivated it.

**Layout:** `base/` holds the original manifests + `NOTES.md`; `prod/` is the
production overlay it never grew (the CronJob + remediation RBAC from `NOTES.md`
§§ B–C). The `kustomization.yaml` files in both were added only to preserve the
base/prod shape — nothing here deploys.

## Why it was superseded

`NOTES.md` raised the right questions; MeshMedic is the answers:

| Open question / recommendation in the concept | How MeshMedic resolved it |
| --- | --- |
| *Does ztunnel `workloadState` actually catch a pod that lost capture after enrollment?* | **No — it doesn't.** An orphan stays in `workloadState`, so the control-plane scan is blind to the exact failure mode that mattered. MeshMedic instead reads the **pod's netns** (`/host/proc/<pid>/net/tcp`) for ztunnel's in-pod listeners — the authoritative signal. |
| "Add a behavioral detector on ztunnel access logs (policy-rejection / empty `src.identity`) as the primary signal." | Implemented as `meshmedic scan --behavioral` — a cheap fleet-wide pre-filter. |
| "Remediation with policy gates: controller allowlist, hysteresis, per-namespace exclusions, rate limit, dry-run." | Realized in the agent: pod-restart repair gated by a **grace period** (hysteresis), a **flap-guard re-read** (no single-miss action), opt-in `--auto-repair` / `--repair-stuck`, and an **orphan filter** that bounds churn. |
| "No mutation; detection only." | The Go agent closes the loop — detect **and** durably re-enroll via restart. |

The conceptual leap was **mesh health ≠ control-plane bookkeeping**: trust the data
plane (the pod's own sockets / the rejected traffic), not `workloadState`.

## Do not revive this

It is here as a record of how MeshMedic's design was reached. The Ansible path is not
maintained and should not be deployed. For the working tool, see the repository
[`README.md`](../README.md) and [`docs/agent.md`](../docs/agent.md).
