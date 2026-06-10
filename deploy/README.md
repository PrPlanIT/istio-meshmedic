# Deploying istio-meshmedic

Two supported shapes. Both run the agent as a DaemonSet (one pod per node) that
reads `/host/proc` directly — no ephemeral containers. The agent needs `hostPID`
and root to read other pods' netns, so **it must run in a namespace that allows
privileged pods.**

## 1. Quick start — single file

```sh
kubectl apply -f deploy/meshmedic-daemonset.yaml
```

Self-contained: creates a `meshmedic` namespace, RBAC, and the DaemonSet in
**detect-only** mode (`--auto-repair=false --repair-stuck=false`). It logs orphans
and serves `/metrics` but changes nothing. Good for a first look. Flip the
`--auto-repair` / `--repair-stuck` args once you trust it (see below).

> **Image tags:** the standalone manifest pins `:v0.0.1` (the first release) with
> `imagePullPolicy: IfNotPresent`. The `deploy/fluxcd/base` overlay tracks the
> rolling `:latest-dev` dev tag, and `deploy/fluxcd/production` pins `:v0.0.1` —
> the base/overlay split showing the dev-vs-pinned pattern. Never float production
> on `:latest-dev`.

## 2. Flux / Kustomize — `deploy/fluxcd/`

```
deploy/fluxcd/
  base/        # SA, ClusterRole(+binding), DaemonSet — SAFE defaults (detect-only)
  production/  # overlay: --auto-repair=true + --repair-stuck=true
```

Point a Flux `Kustomization` (or `kubectl apply -k`) at `deploy/fluxcd/production`:

```sh
kubectl apply -k deploy/fluxcd/production
```

### Where to put it

The templates default to **`istio-system`** deliberately: deploy meshmedic **into
your istio data-plane namespace, alongside ztunnel / istio-cni.**

- That namespace is **already privileged** (ztunnel/cni require it), so the agent's
  `hostPID`+root adds no new PSS exposure.
- It is **not shared with application workloads**, so you avoid the real risk —
  dropping a privileged DaemonSet into a shared app namespace and being forced to
  relax that namespace's Pod Security for everything else in it.
- meshmedic literally watches the in-pod listeners ztunnel installs, so it belongs
  next to the thing it monitors.

Change `namespace:` in the base files to match your install (e.g. a themed istio
namespace). A dedicated privileged namespace also works; a shared app namespace
does **not** — see the risk above.

> **Reference deployment:** this project runs as a Flux-managed operator named
> `istio-meshmedic`, in the istio data-plane namespace, sorted beside the other
> `istio*` operators, with the production overlay (repair enabled). The
> `istio-meshmedic` resource naming keeps it adjacent to istio in `kubectl get ds`,
> `clusterrole`, and the GitOps tree — without ever being a namespace.

## Repair policy (opt-in, two tiers)

| flag | restarts | when |
| --- | --- | --- |
| `--auto-repair` | **Ready** orphans (healthy pod that lost capture) | almost always restart-fixable |
| `--repair-stuck` | **not-Ready** orphans | only after `--grace-period` of continuous not-Ready, still orphaned after the `--confirm` re-read |

Both are off by default. Detect-and-surface first; enable repair once
`meshmedic_orphans_stuck` shows it's catching real, restart-fixable orphans. See
[`../docs/agent.md`](../docs/agent.md) for the scan model, metrics, and the measured
footprint.
