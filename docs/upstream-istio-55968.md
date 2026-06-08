# Upstream: istio-cni `reconcileExistingPod` abandons running pods after node reboot

Draft for reopening [istio/istio#55968](https://github.com/istio/istio/issues/55968)
(dup: [#57285](https://github.com/istio/istio/issues/57285)) and a fix PR. This is
the bug meshmedic exists to work around.

---

## Title

`istio-cni`: `reconcileExistingPod` silently abandons running ambient pods whose
netns isn't in the snapshot after node reboot → permanent mesh bypass

## Summary

After a node reboot, some already-running ambient pods permanently lose their
in-pod redirection (iptables + ztunnel listeners) and are never recovered. They
**keep** the `ambient.istio.io/redirection: enabled` annotation and **remain
present in ztunnel's `workloadState`**, so they look healthy to every
control-plane check — but their traffic bypasses the mesh until the pod or
istio-cni is manually restarted. Root cause: `getNetns` returns `ErrPodNotFound`
for an existing pod whose netns the agent failed to relocate, and
`reconcileExistingPod` treats that as "ok, newly created" and skips it.

## Affected versions

Reported on 1.25.1 (#55968) and 1.26.3 (#57285); confirmed present in the code on
`master`/1.29.x/1.30.x. The relevant logic is unchanged through 1.30 (1.30.0
change notes contain no fix).

## Symptom

A pod that was correctly enrolled, then survived a node reboot:

- `ambient.istio.io/redirection: enabled` annotation is **present**.
- The workload **is** in its node ztunnel's `config_dump` / `workloadState`.
- But the pod's **netns has none of ztunnel's in-pod listeners** (15001 outbound,
  15006 inbound, 15008 HBONE, 15053 DNS), and the in-pod iptables redirect chains
  are gone.
- Peer ztunnels reject its traffic: `connection closed due to policy rejection:
  allow policies exist, but none allowed`, with **empty `src.identity`**.
- Restarting the pod **or** `istio-cni` restores it immediately.

## Reproduction

Ambient cluster (here: 1.29.1, Cilium-chained, `reconcileIptablesOnStartup: true`).
Reboot a worker node. After recovery, some running pods come back orphaned.

Confirm by reading the pod's own netns (no privileges needed — a baseline
ephemeral probe, since real orphans are often distroless):

```bash
kubectl debug <pod> -n <ns> --image=docker.io/istio/install-cni:<ver> \
  --profile=baseline -c probe -- sleep 60
kubectl exec <pod> -n <ns> -c probe -- \
  sh -c 'cat /proc/net/tcp /proc/net/tcp6 | awk "\$4==\"0A\"{print \$2}"'
# hex ports: 3A99=15001 3A9E=15006 3AA0=15008 3ACD=15053
```

Observed, same node:

| netns LISTEN | orphan pod | healthy pod |
| --- | --- | --- |
| app ports | present | present |
| 15001 / 15006 / 15008 / 15053 | **none** | all four |

The orphan is in ztunnel `workloadState` the entire time — so a control-plane
("annotation vs workloadState") scan reports it healthy. Only the netns (or the
rejected-traffic logs) reveals it.

## Root cause

`cni/pkg/nodeagent/net_linux.go`:

```go
func (s *NetServer) reconcileExistingPod(pod *corev1.Pod) error {
	openNetns, err := s.getNetns(pod)
	if err != nil {
		return err            // ← abandons reconciliation
	}
	...
}
```

```go
// inside getNetns
openNetns = s.currentPodSnapshot.Get(string(pod.UID))
if openNetns == nil {
	return nil, fmt.Errorf(
		"can't find netns for pod, this is ok if this is a newly created pod (%w)",
		ErrPodNotFound)
}
```

`currentPodSnapshot` is built from procfs at agent startup. After a node reboot
the agent restarts and, for some already-running pods, fails to relocate their
netns into the snapshot. `getNetns` then returns `ErrPodNotFound`, and the
caller's comment/assumption ("ok if this is a newly created pod") is **wrong**:
the pod is a live, Running pod with a valid netns — the agent simply lost track
of it. Reconciliation is skipped, so the in-pod iptables and the ztunnel
listeners are never re-established, and nothing ever retries.

## Impact

- **Silent, persistent mesh bypass** (security + connectivity). Affected pods
  pass readiness and appear enrolled everywhere except their actual netns.
- Triggered by routine node maintenance/reboots; recurs every time.
- No metric distinguishes "skipped — genuinely new pod" from "FAILED to relocate
  an existing pod," so it's invisible without deep inspection.

## Proposed fix

Stop treating "netns not in snapshot" as benign for pods that are clearly **not**
new, and recover the netns instead of skipping:

1. In `reconcileExistingPod` (or `getNetns`), when the snapshot misses a pod that
   is **Running with started containers and a non-recent start time**, do not
   return `ErrPodNotFound`. Instead **actively re-discover** the netns:
   - re-scan procfs for the pod's sandbox PID → `/proc/<pid>/ns/net`, or
   - query the CRI for the sandbox PID, or
   - re-run the same enrollment path used on CNI `ADD`.
2. Only treat "netns not found" as a benign skip for genuinely new pods (no
   started containers / very recent creation).
3. Add a counter (e.g. `istio_cni_reconcile_netns_not_found{reason="existing"}`)
   and a non-debug log so this failure is observable, not silent.

### Sketch (illustrative — needs validation against the full file)

```go
openNetns := s.currentPodSnapshot.Get(string(pod.UID))
if openNetns == nil {
	if podIsLikelyNew(pod) {
		// genuinely new: CNI ADD will (or did) handle it.
		return nil, fmt.Errorf("netns not found for new pod (%w)", ErrPodNotFound)
	}
	// Existing running pod we lost track of (e.g. after node reboot):
	// re-discover its netns rather than abandoning reconciliation.
	openNetns, err = s.relocateNetns(pod) // procfs/CRI sandbox PID lookup
	if err != nil {
		reconcileNetnsNotFound.With("reason", "existing").Increment()
		return nil, fmt.Errorf("failed to relocate netns for existing pod %s/%s: %w",
			pod.Namespace, pod.Name, err)
	}
	s.currentPodSnapshot.Upsert(string(pod.UID), openNetns)
}

func podIsLikelyNew(p *corev1.Pod) bool {
	for _, c := range p.Status.ContainerStatuses {
		if c.State.Running != nil { // has a running container → not new
			return false
		}
	}
	return true
}
```

## Workaround (until fixed)

Restart the affected pod, or toggle `istio.io/dataplane-mode: none` → back (forces
re-enrollment without killing the pod), or `kubectl rollout restart daemonset
istio-cni-node` (re-reconciles on startup, but inherits the same bug for any pod
it still can't relocate). Detection requires the **netns** check above —
`workloadState` will not reveal these orphans.
