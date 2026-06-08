# istio-meshmedic

**MeshMedic** — a Go CLI and (planned) Kubernetes operator that detects and heals
**Istio ambient-mesh enrollment orphans**: workloads that *should* be captured by
their node's ztunnel but aren't.

## The problem it solves

In Istio **ambient** mode, every pod's traffic is redirected to its node's
`ztunnel` for mTLS. A pod can be correctly enrolled at startup and then **silently
lose its per-pod redirection** (e.g. after a node reboot / CNI churn, with no CNI
`DEL`). When that happens:

- the pod keeps the `ambient.istio.io/redirection: enabled` annotation and *looks*
  healthy,
- but its traffic now leaves as **plaintext**, and peer ztunnels reject it with
  `connection closed due to policy rejection: allow policies exist, but none
  allowed` and an **empty `src.identity`**.

The only reliable fix today is a **pod restart** (re-triggers istio-cni, reinstalls
capture). MeshMedic finds these orphans and, under strict safety gates, restarts
them.

## Status

| Capability | State |
| --- | --- |
| `scan` — detect orphans (control-plane: annotation vs. ztunnel `workloadState`) | ✅ implemented |
| Behavioral detector (dataplane: ztunnel access-log `policy rejection` + empty `src.identity`) | ⏳ planned (the more robust signal) |
| `remediate` — restart orphans with safety gates | ⏳ planned |
| Operator/CronJob mode (CRD-driven, scheduled scan + gated remediation) | ⏳ planned |

## `scan`

```
meshmedic scan                 # human table; exits non-zero if orphans found
meshmedic scan -o json         # machine-readable
```

It lists pods annotated `ambient.istio.io/redirection: enabled` (the *expected*
enrolled set), reads each node's ztunnel `:15000/config_dump` (via port-forward —
the admin port is localhost-bound), and reports any expected workload **absent
from its node's ztunnel workload state**.

> **Schema note:** ztunnel's `config_dump` shape has shifted across releases. The
> parser (`src/scan/configdump.go`) is defensive and matches primarily on
> `namespace/name`; verify field names against your ztunnel version
> (`istioctl ztunnel-config workload -o json`). Pinned expectation: Istio 1.29.x.

### Detection caveat (known open question)

The `scan` trusts ztunnel's `workloadState` as ground truth for "captured on this
node." This cleanly catches *never-enrolled* pods. It is **unverified** whether it
also catches the *enrolled-then-lost-capture* mode (a pod may still appear in
`workloadState` even after its netns redirect rules were clobbered). That is why
the **behavioral detector** (grounded in actually-rejected traffic) is planned as
the *primary* signal, with `scan` as a complementary periodic check.

## Build

No local Go toolchain — build in a container:

```
docker run --rm -v "$PWD":/src -w /src golang:1.26.4 \
  sh -c 'go mod tidy && go build -o bin/meshmedic ./cmd/meshmedic'
```

## Run against a cluster (read-only)

```
docker run --rm --network host -e KUBECONFIG=/kube/config \
  -v "$HOME/.kube:/kube:ro" -v "$PWD/bin:/bin-mm:ro" \
  golang:1.26.4 /bin-mm/meshmedic scan
```

## Design

The detection/remediation model (signals, safety gates: controller-kind allowlist,
hysteresis, namespace exclusions, rate limit, dry-run) is carried over from the
original `istio-spiffe-reconciler` scan playbook + NOTES. MeshMedic reimplements it
as a Go operator in the shape of [HASteward](https://github.com/PrPlanIT/HASteward).
