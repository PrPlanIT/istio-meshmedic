// Package repair re-enrolls ambient orphans by toggling the per-pod
// istio.io/dataplane-mode label, forcing istio-cni to recreate the missing in-pod
// ztunnel listeners without restarting the pod.
package repair

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/PrPlanIT/istio-meshmedic/src/k8s"
	"github.com/PrPlanIT/istio-meshmedic/src/scan"
)

// dataplaneModeLabel opts a single pod in/out of the ambient data plane,
// overriding the namespace default. Setting it to "none" tears down the pod's
// redirection; removing it lets the namespace default ("ambient") re-apply.
const dataplaneModeLabel = "istio.io/dataplane-mode"

// Result is the per-orphan outcome of a repair pass.
type Result struct {
	Pod    string `json:"pod"`
	Node   string `json:"node"`
	Action string `json:"action"` // would-repair | repaired | failed
	Detail string `json:"detail"`
}

// Repair finds orphans (via the same detector as scan) and, when apply is true,
// re-enrolls each by toggling its istio.io/dataplane-mode label off ("none") and
// back (removed → namespace default re-applies). That makes istio-cni tear down
// and re-establish the pod's redirection — recreating the missing ztunnel
// listeners — WITHOUT restarting the pod. It then re-probes to confirm.
//
// With apply=false it is a dry run: it reports what it WOULD repair, changing
// nothing. candidates (behavioral pre-filter) and namespace scope are honored.
func Repair(ctx context.Context, probeImage, namespace string, candidates map[string]bool, apply bool) ([]Result, error) {
	report, err := scan.Scan(ctx, probeImage, namespace, candidates)
	if err != nil {
		return nil, err
	}

	var results []Result
	for _, o := range report.Orphans {
		key := o.Namespace + "/" + o.Name
		if !apply {
			results = append(results, Result{
				Pod: key, Node: o.Node, Action: "would-repair",
				Detail: "toggle dataplane-mode none→ambient (no restart)",
			})
			continue
		}

		if err := toggleDataplaneMode(ctx, o.Namespace, o.Name); err != nil {
			results = append(results, Result{Pod: key, Node: o.Node, Action: "failed", Detail: err.Error()})
			continue
		}
		if present, ok := confirmHealed(ctx, o.Namespace, o.Name, probeImage); ok {
			results = append(results, Result{
				Pod: key, Node: o.Node, Action: "repaired",
				Detail: fmt.Sprintf("listeners returned %v", present),
			})
		} else {
			results = append(results, Result{
				Pod: key, Node: o.Node, Action: "failed",
				Detail: "listeners did not return after toggle — a pod restart may be required",
			})
		}
	}
	return results, nil
}

// toggleDataplaneMode opts the pod out of ambient (label "none" → istio-cni tears
// down redirection), waits, then removes the label so the namespace default
// re-applies and istio-cni re-enrolls the pod, recreating the in-pod listeners.
func toggleDataplaneMode(ctx context.Context, namespace, name string) error {
	c := k8s.GetClients()
	if c == nil {
		return fmt.Errorf("kubernetes clients not initialized")
	}
	pods := c.Clientset.CoreV1().Pods(namespace)

	off := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:"none"}}}`, dataplaneModeLabel))
	if _, err := pods.Patch(ctx, name, types.StrategicMergePatchType, off, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("opt out (label none): %w", err)
	}

	if err := sleep(ctx, 4*time.Second); err != nil {
		return err
	}

	on := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:null}}}`, dataplaneModeLabel))
	if _, err := pods.Patch(ctx, name, types.StrategicMergePatchType, on, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("re-enroll (remove label): %w", err)
	}
	return nil
}

// confirmHealed re-probes the pod a few times with backoff and reports whether
// the capture listeners returned.
func confirmHealed(ctx context.Context, namespace, name, probeImage string) ([]int, bool) {
	for attempt := 0; attempt < 4; attempt++ {
		if err := sleep(ctx, time.Duration(3*(attempt+1))*time.Second); err != nil {
			return nil, false
		}
		present, err := scan.ProbePod(ctx, namespace, name, probeImage)
		if err != nil {
			continue
		}
		if scan.IsCaptured(present) {
			return present, true
		}
	}
	return nil, false
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
