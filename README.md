# istio-meshmedic

**MeshMedic** — a Go CLI **and** node-agent DaemonSet that detects and heals
**Istio ambient-mesh enrollment orphans**: workloads that *should* be captured by
their node's ztunnel but aren't.

<!-- sf:project:start -->
<!-- sf:project:end -->
<!-- sf:badges:start -->
<!-- sf:badges:end -->
<!-- sf:image:start -->
<!-- sf:image:end -->

## The problem it solves

In Istio **ambient** mode, every pod's traffic is redirected to its node's
`ztunnel`. A pod can be correctly enrolled at startup and then **silently lose its
in-pod redirection** — most often after a node reboot, when istio-cni fails to
re-reconcile already-running pods (`reconcileExistingPod`/`getNetns` bails;
upstream [istio/istio#55968](https://github.com/istio/istio/issues/55968), unfixed).
When that happens the pod:

- keeps its `ambient.istio.io/redirection: enabled` annotation and *looks* healthy,
- **remains in ztunnel's `workloadState`** (so the control plane thinks it's fine),
- but its netns is missing ztunnel's in-pod listeners, so its traffic isn't
  captured — peers reject it (`policy rejection: allow policies exist, but none
  allowed`, empty `src.identity`) and HBONE to it is refused.

See [`docs/upstream-istio-55968.md`](docs/upstream-istio-55968.md) for the
root-cause analysis and a proposed upstream fix — the only true *cure*. MeshMedic
is the **mitigation**.

## How it detects (the important part)

A control-plane check is **blind** to this: the orphan stays in ztunnel's
`workloadState`. The authoritative signal is the **pod's own network namespace** —
an orphan is annotated ambient-enrolled but has **none** of ztunnel's in-pod
listeners (`15001` outbound, `15006` inbound, `15008` HBONE, `15053` DNS). MeshMedic
reads `/proc/net/tcp` from the pod's netns and checks for those ports.

Caveat learned in production: netns *sockets* are necessary but the state **flaps**
(istio-cni reconciles in the background), so a single missing reading is re-confirmed
before any action.

## Commands

| command | what |
| --- | --- |
| `scan` | netns-socket detector. Execs the pod's own container to read `/proc/net/tcp` (no ephemeral container); falls back to a baseline-safe ephemeral probe only for distroless images. `-n <ns>` to scope. |
| `scan --behavioral` | cheap fleet-wide pre-filter: scrape every ztunnel's access logs for the orphan signatures (HBONE refused to `:15008`; policy-rejection with no `src.identity`), then netns-probe only the flagged pods. |
| `repair` | re-enroll orphans. **`--strategy restart`** (default) deletes the pod for a fresh, durable enrollment; **`--strategy toggle`** flips the `dataplane-mode` label (gentle, but flaps). Dry-run unless `--yes`; requires `-n` or `--behavioral`. |
| `agent` | per-node DaemonSet: reads `/host/proc` directly (zero injection), sweeps on a loop, and with `--auto-repair` restarts confirmed-stuck orphans (flap-guarded). |

```sh
meshmedic scan -n delivery-bag
meshmedic scan --behavioral --since 15m
meshmedic repair -n hookshot            # dry-run
meshmedic repair -n hookshot --yes      # restart the orphans
```

## Deploy the node-agent (no ephemeral probes, continuous)

```sh
kubectl apply -f deploy/meshmedic-daemonset.yaml   # detect-only by default
```

One pod per node (`hostPID`, host `/proc` at `/host/proc`) maps each local pod to a
PID via its cgroup UID and reads its netns sockets directly — cap-free, nothing
injected. Flip `--auto-repair=true` in the DaemonSet to have it keep the mesh
healed.

## Build / run

Built and shipped through [StageFreight](https://gitlab.prplanit.com/PrPlanIT/StageFreight)
(`docker.io/prplanit/istio-meshmedic`). To run the CLI against a cluster from a
container:

```sh
docker run --rm --network host --user "$(id -u):$(id -g)" -v "$HOME/.kube:/kube:ro" \
  docker.io/prplanit/istio-meshmedic:v0.0.1 \
  scan --kubeconfig /kube/config -n <namespace>
```

(Run as your own uid so the kubeconfig is readable — the image runs as `nonroot`.)

## Mitigation vs. cure

MeshMedic finds and restarts stuck orphans; it does **not** stop them being
created. The cure is the substrate: the upstream `#55968` fix, or rolling
ztunnel/istio-cni. Per-pod repair is whack-a-mole on a flapping data plane — which
is exactly why the agent runs continuously.
