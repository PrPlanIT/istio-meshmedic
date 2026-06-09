// Package repair re-enrolls ambient orphans. The durable strategy is a pod
// restart (fresh enrollment is unaffected by the istio-cni reconcile bug); the
// gentle per-pod dataplane-mode toggle is offered as an opt-in, but on an
// unstable mesh it can restore the in-pod sockets only transiently (they flap
// back), so it is NOT the default.
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

// dataplaneModeLabel opts a single pod in/out of the ambient data plane.
const dataplaneModeLabel = "istio.io/dataplane-mode"

// Strategy selects how an orphan is re-enrolled.
type Strategy string

const (
	// StrategyRestart deletes the pod so its controller recreates it with a fresh
	// ambient enrollment. Durable — fresh enrollment is unaffected by the reconcile
	// bug. The default.
	StrategyRestart Strategy = "restart"
	// StrategyToggle flips the per-pod dataplane-mode label off and back, asking
	// istio-cni to re-enroll without a restart. Gentle, but the re-established
	// sockets can flap on an unstable mesh — opt-in only.
	StrategyToggle Strategy = "toggle"
)

// Result is the per-orphan outcome of a repair pass.
type Result struct {
	Pod    string `json:"pod"`
	Node   string `json:"node"`
	Action string `json:"action"` // would-repair | restarted | repaired | failed
	Detail string `json:"detail"`
}

// Repair finds orphans (the same detector as scan) and, when apply is true,
// re-enrolls each via the chosen strategy. With apply=false it is a dry run that
// changes nothing. candidates (behavioral pre-filter) and namespace scope honored.
func Repair(ctx context.Context, probeImage, namespace string, candidates map[string]bool, apply bool, strategy Strategy) ([]Result, error) {
	report, err := scan.Scan(ctx, probeImage, namespace, candidates)
	if err != nil {
		return nil, err
	}

	var results []Result
	for _, o := range report.Orphans {
		key := o.Namespace + "/" + o.Name
		if !apply {
			results = append(results, Result{Pod: key, Node: o.Node, Action: "would-repair", Detail: planDetail(strategy)})
			continue
		}
		if strategy == StrategyToggle {
			results = append(results, toggleRepair(ctx, o, probeImage))
		} else {
			results = append(results, restartRepair(ctx, o))
		}
	}
	return results, nil
}

func planDetail(s Strategy) string {
	if s == StrategyToggle {
		return "toggle dataplane-mode (gentle, no restart — may only hold transiently)"
	}
	return "restart pod (fresh enrollment — durable)"
}

// restartRepair deletes the orphan pod; its controller recreates it with a fresh,
// durable ambient enrollment. This is the reliable heal — the gentle toggle can
// restore sockets only transiently on a flapping data plane.
func restartRepair(ctx context.Context, o scan.Orphan) Result {
	key := o.Namespace + "/" + o.Name
	c := k8s.GetClients()
	if c == nil {
		return Result{Pod: key, Node: o.Node, Action: "failed", Detail: "kubernetes clients not initialized"}
	}
	if err := c.Clientset.CoreV1().Pods(o.Namespace).Delete(ctx, o.Name, metav1.DeleteOptions{}); err != nil {
		return Result{Pod: key, Node: o.Node, Action: "failed", Detail: "delete: " + err.Error()}
	}
	return Result{
		Pod: key, Node: o.Node, Action: "restarted",
		Detail: "deleted — controller re-creates with a fresh enrollment (re-scan to confirm)",
	}
}

// toggleRepair flips the dataplane-mode label off and back, then re-probes. Gentle
// (no restart) but unreliable on an unstable mesh — the sockets may flap back.
func toggleRepair(ctx context.Context, o scan.Orphan, probeImage string) Result {
	key := o.Namespace + "/" + o.Name
	if err := toggleDataplaneMode(ctx, o.Namespace, o.Name); err != nil {
		return Result{Pod: key, Node: o.Node, Action: "failed", Detail: err.Error()}
	}
	if present, ok := confirmHealed(ctx, o.Namespace, o.Name, probeImage); ok {
		return Result{
			Pod: key, Node: o.Node, Action: "repaired",
			Detail: fmt.Sprintf("listeners returned %v — verify it holds; use --strategy restart if it recurs", present),
		}
	}
	return Result{
		Pod: key, Node: o.Node, Action: "failed",
		Detail: "listeners did not return — use the default restart strategy",
	}
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
