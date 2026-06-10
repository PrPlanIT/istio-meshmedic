# istio-spiffe-reconciler — Outstanding Work

Status as of 2026-05-30: scaffolding only. Not running in any cluster.

## What exists

- `serviceaccount.yaml`, `clusterrole.yaml`, `clusterrolebinding.yaml` — RBAC is intentionally read-only (`get/list/watch` pods/namespaces/nodes, `create pods/exec`). No mutation verbs.
- `configmap-playbook.yaml` — contains `scan.yml`, a read-only Ansible playbook that:
  1. Lists pods with annotation `ambient.istio.io/redirection: enabled` → expected set
  2. Execs into each ztunnel pod and reads `localhost:15000/config_dump` → `workloadState` keys → enrolled set
  3. Diffs per-node: `expected − enrolled = orphans`
  4. Emits JSON, human table, and Prometheus text-format

## What's missing to make it functional

1. **No `kustomization.yaml`** in this directory. Flux does not deploy any of these manifests today.
2. **No `CronJob`/`Job`** that actually runs the playbook. The SA + RBAC exist but nothing invokes them.
3. **No overlay reference.** `grep -rn istio-spiffe-reconciler /srv/dungeon/fluxcd` returns hits only inside this directory — no production overlay includes it.
4. **No `remediate.yml`** playbook. Detection only; nothing closes the loop.
5. **No mutation RBAC** even if remediation existed (would need `pods/delete` or workload-controller patch rights).

## Open detection question (test before relying on this)

The scan trusts ztunnel's `workloadState` as the source of truth for "enrolled on this node." This catches *pod was never enrolled* cases cleanly.

It is **unverified** whether the scan catches the failure mode hit on 2026-05-30:

- Pod was enrolled correctly at startup.
- Later, per-pod netns capture rules were silently lost (no CNI DEL).
- Outbound traffic exited as plain TCP; destination ztunnel rejected with
  `error="connection closed due to policy rejection: allow policies exist, but none allowed"`
  and `src.identity` was empty in the access log.
- Fix was a pod restart (re-triggers istio-cni and re-installs capture).

**Test before treating the scan as the safety net:** in a non-prod ambient namespace, force-clobber a pod's redirect rules (e.g., flush its nftables ruleset in its netns) without going through CNI DEL, and check whether the pod's UID disappears from `workloadState` on the local ztunnel.

- If it disappears → scan catches this class. Ship the scan + remediation.
- If it persists → scan has a blind spot; add a behavioral detector (see below).

## Recommended additions

### A. Behavioral detector (dataplane signal, not control-plane assumption)

Independent of the scan, alert on ztunnel access logs matching:

- `error` containing `policy rejection: allow policies exist, but none allowed`
- `src.identity` field empty/absent
- `direction=inbound`

This is grounded in actual rejected traffic — it cannot be fooled by a stale `workloadState`. Cheap to add via Loki/LogQL. Group by `src.workload` + `src.namespace` to identify the offending pod. Should be the **primary** alert; the scan is a complementary periodic check.

### B. Remediation playbook (`remediate.yml`)

Inputs: orphan list from `scan.yml` JSON output (or behavioral detector).

Policy gates before restarting:

- **Allowlist of controller kinds.** Deployment/ReplicaSet: yes. StatefulSet: case-by-case — never auto-restart a CNPG primary or single-replica StatefulSet without escalation.
- **Hysteresis.** Require the orphan condition to persist for N minutes (suggest 5) before action. Single scan miss → no action.
- **Per-namespace exclusion list.** At minimum: `kube-system`, `king-of-red-lions`, `flux-system`, anything labeled `policy.prplanit.com/no-auto-restart=true`.
- **Rate limit.** No more than M restarts per scan cycle cluster-wide (suggest 3) to avoid cascading restarts if detection misfires.
- **Dry-run mode** controlled by ConfigMap flag for the first weeks of deployment.

Mutation RBAC needed: `pods` `delete`, or alternatively patch the controller's pod template hash to trigger a rolling restart.

### C. Wire into flux

- Add `kustomization.yaml` listing the four existing manifests + new `CronJob`.
- Add this directory to the production overlay's resources list.
- Schedule: every 10–15 min for scan, with separate manual-trigger Job for remediation until trusted.

### D. Observability

- Already emits Prometheus block — add a ServiceMonitor / PodMonitor so Prom scrapes the metrics from the Job's last-run output (via a sidecar exposing the cached metrics, or via push to Pushgateway).
- Add alert rules:
  - `ambient_orphan_total > 0` for >15 min → page
  - `ambient_ztunnel_workload_count` per node sudden drop → page (catches node-level capture loss before individual pods are flagged)

## Reference: 2026-05-30 incident

Symptoms cluster-wide: every CNPG cluster showed `Instance Status Extraction Error: HTTP communication issue`. Root cause: the `cloudnative-pg` operator pod (90 days old) had lost its ambient capture rules at some point; outbound traffic exited un-tunneled; destination ztunnels rejected with the policy-rejection error above. Manual fix: `kubectl -n gorons-bracelet rollout restart deploy cloudnative-pg`. Within ~60s, identity returned to ztunnel logs and 7+ CNPG clusters recovered to healthy without further intervention.
