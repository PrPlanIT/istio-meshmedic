// Package agent runs meshmedic as a per-node operator. It reads each local pod's
// netns sockets directly from /host/proc — no ephemeral container injected, no
// exec — and assesses MESH enrollment independently of Kubernetes readiness.
//
// That independence is the whole point: a pod orphaned badly enough to be knocked
// not-Ready (init never completes, or a dependency is unreachable) is exactly the
// pod a readiness gate would hide — and twice in production it was the orphan
// doing the most damage. So the scanner never gates on PodReady. It records
// readiness + how long the pod has been not-Ready as metadata; POLICY, not
// detection, decides what to repair. This is the zero-littering home for the
// netns detector.
package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"

	"github.com/PrPlanIT/istio-meshmedic/src/k8s"
	"github.com/PrPlanIT/istio-meshmedic/src/scan"
)

const (
	redirectionAnnotation = "ambient.istio.io/redirection"
	redirectionEnabled    = "enabled"
)

// Options configures the agent loop.
type Options struct {
	Node        string        // this node's name ($NODE_NAME)
	HostProc    string        // host /proc mount (default /host/proc)
	Interval    time.Duration // time between sweeps
	Confirm     time.Duration // re-confirm delay before acting on an orphan (flap guard)
	GracePeriod time.Duration // a not-Ready orphan is actionable only after it has remained continuously not-Ready longer than this
	AutoRepair  bool          // restart Ready orphans (capture lost on an otherwise-healthy pod)
	RepairStuck bool          // restart not-Ready orphans that remain orphaned past the grace period
	MetricsAddr string        // listen address for /metrics (empty disables)
}

// Orphan is an ambient-annotated pod whose netns is missing ztunnel's capture
// listeners. Readiness is recorded as metadata, never used to filter detection:
// classification is the scanner's job, what to do about each class is policy's.
type Orphan struct {
	Namespace        string
	Name             string
	UID              string
	Present          []int
	Ready            bool          // the pod's Ready condition
	NotReadyDuration time.Duration // how long continuously not-Ready (0 when Ready)
}

// Stuck reports whether this is a not-Ready orphan that has remained continuously
// not-Ready longer than grace — i.e. "this pod appears unhealthy because it is
// orphaned," the stronger claim the stuck-repair path acts on.
func (o Orphan) Stuck(grace time.Duration) bool {
	return !o.Ready && o.NotReadyDuration > grace
}

// podCgroupRe extracts the pod UID from a cgroup path (pod<uid>, with either dash
// or systemd-underscore separators).
var podCgroupRe = regexp.MustCompile(`pod([0-9a-fA-F]{8}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{12})`)

func podUIDFromCgroup(cgroup string) string {
	m := podCgroupRe.FindStringSubmatch(cgroup)
	if len(m) < 2 {
		return ""
	}
	return strings.ReplaceAll(m[1], "_", "-")
}

// mapPodPIDs scans hostProc and returns podUID → a PID in that pod. Any container
// in a pod shares the netns, so the first PID found suffices.
func mapPodPIDs(hostProc string) (map[string]int, error) {
	entries, err := os.ReadDir(hostProc)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", hostProc, err)
	}
	out := make(map[string]int)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(hostProc, e.Name(), "cgroup"))
		if err != nil {
			continue
		}
		if uid := podUIDFromCgroup(string(data)); uid != "" {
			if _, ok := out[uid]; !ok {
				out[uid] = pid
			}
		}
	}
	return out, nil
}

// readListeners reads /host/proc/<pid>/net/tcp{,6} (the pod's netns) and returns
// the ztunnel in-pod listener ports present. Cap-free + injection-free.
func readListeners(hostProc string, pid int) []int {
	var sb strings.Builder
	for _, f := range []string{"net/tcp", "net/tcp6"} {
		if data, err := os.ReadFile(filepath.Join(hostProc, strconv.Itoa(pid), f)); err == nil {
			sb.Write(data)
		}
	}
	return scan.ListenersFromProcNet(sb.String())
}

// notReadyDuration reports how long the pod has been continuously not-Ready.
// Zero when the pod is Ready. For a pod with no Ready condition yet (e.g. wedged
// in init) it measures from the pod's start, then creation, time.
func notReadyDuration(p *corev1.Pod, now time.Time) time.Duration {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			if c.Status == corev1.ConditionTrue {
				return 0
			}
			if !c.LastTransitionTime.IsZero() {
				return now.Sub(c.LastTransitionTime.Time)
			}
			break
		}
	}
	if p.Status.StartTime != nil {
		return now.Sub(p.Status.StartTime.Time)
	}
	return now.Sub(p.CreationTimestamp.Time)
}

// ScanNode detects ambient orphans among the local node's pods by reading their
// netns sockets from hostProc. Mesh enrollment is assessed independently of
// application readiness — every ambient pod with a live netns on this node is
// checked; only terminal pods (Succeeded/Failed, no netns to assess) are skipped.
// Read-only.
func ScanNode(ctx context.Context, node, hostProc string) ([]Orphan, error) {
	c := k8s.GetClients()
	if c == nil {
		return nil, fmt.Errorf("kubernetes clients not initialized")
	}
	pids, err := mapPodPIDs(hostProc)
	if err != nil {
		return nil, err
	}
	pods, err := c.Clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", node).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	now := time.Now()
	var orphans []Orphan
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Annotations[redirectionAnnotation] != redirectionEnabled {
			continue
		}
		// Mesh enrollment is independent of application readiness — do NOT gate on
		// PodReady. Skip only terminal pods, which have no live netns to assess.
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		pid, ok := pids[string(p.UID)]
		if !ok {
			continue // no live sandbox/netns on this node yet — nothing to assess
		}
		if present := readListeners(hostProc, pid); !scan.IsCaptured(present) {
			orphans = append(orphans, Orphan{
				Namespace:        p.Namespace,
				Name:             p.Name,
				UID:              string(p.UID),
				Present:          present,
				Ready:            k8s.PodReady(p),
				NotReadyDuration: notReadyDuration(p, now),
			})
		}
	}
	sort.Slice(orphans, func(i, j int) bool {
		if orphans[i].Namespace != orphans[j].Namespace {
			return orphans[i].Namespace < orphans[j].Namespace
		}
		return orphans[i].Name < orphans[j].Name
	})
	return orphans, nil
}

// classify counts orphans by class for logging + metrics.
func classify(orphans []Orphan, grace time.Duration) (ready, notReady, stuck int) {
	for _, o := range orphans {
		if o.Ready {
			ready++
		} else {
			notReady++
			if o.Stuck(grace) {
				stuck++
			}
		}
	}
	return ready, notReady, stuck
}

// stillOrphan re-reads a pod's listeners after the confirm delay — the flap guard.
func stillOrphan(uid, hostProc string) bool {
	pids, err := mapPodPIDs(hostProc)
	if err != nil {
		return false
	}
	pid, ok := pids[uid]
	if !ok {
		return false
	}
	return !scan.IsCaptured(readListeners(hostProc, pid))
}

// Run is the continuous agent loop.
func Run(ctx context.Context, opts Options, logf func(string, ...any)) error {
	if opts.HostProc == "" {
		opts.HostProc = "/host/proc"
	}
	if opts.Interval <= 0 {
		opts.Interval = 60 * time.Second
	}
	if opts.Confirm <= 0 {
		opts.Confirm = 10 * time.Second
	}
	if opts.GracePeriod <= 0 {
		opts.GracePeriod = 5 * time.Minute
	}
	if opts.MetricsAddr != "" {
		serveMetrics(opts.MetricsAddr, logf)
		logf("meshmedic metrics on %s/metrics", opts.MetricsAddr)
	}
	logf("meshmedic agent on node %q — interval %s, confirm %s, grace %s, auto-repair %v, repair-stuck %v",
		opts.Node, opts.Interval, opts.Confirm, opts.GracePeriod, opts.AutoRepair, opts.RepairStuck)

	for {
		orphans, err := ScanNode(ctx, opts.Node, opts.HostProc)
		if err != nil {
			sweepErrorsTotal.Inc()
			logf("sweep error: %v", err)
			if e := sleepCtx(ctx, opts.Interval); e != nil {
				return e
			}
			continue
		}
		sweepsTotal.Inc()
		recordSweepMetrics(orphans, opts.GracePeriod)

		ready, notReady, stuck := classify(orphans, opts.GracePeriod)
		logf("sweep: %d orphan(s) — %d ready, %d not-ready (%d stuck >%s)",
			len(orphans), ready, notReady, stuck, opts.GracePeriod)

		for _, o := range orphans {
			if o.Ready {
				logf("  orphan %s/%s [ready] present=%v", o.Namespace, o.Name, o.Present)
			} else {
				logf("  orphan %s/%s [not-ready %s, stuck=%v] present=%v",
					o.Namespace, o.Name, o.NotReadyDuration.Round(time.Second), o.Stuck(opts.GracePeriod), o.Present)
			}

			// Policy gates the restart, never the detection above. A Ready orphan
			// (capture lost on a healthy pod) heals under --auto-repair. A not-Ready
			// orphan heals only under --repair-stuck AND past the grace period — the
			// stronger "unhealthy because orphaned" claim.
			act := (o.Ready && opts.AutoRepair) || (!o.Ready && opts.RepairStuck && o.Stuck(opts.GracePeriod))
			if !act {
				continue
			}

			// Flap guard: re-read after the confirm delay; act only if still orphaned.
			if e := sleepCtx(ctx, opts.Confirm); e != nil {
				return e
			}
			if !stillOrphan(o.UID, opts.HostProc) {
				logf("  %s/%s recovered before repair (flap) — skipping", o.Namespace, o.Name)
				continue
			}
			class := "ready"
			if !o.Ready {
				class = "stuck"
			}
			if err := restartPod(ctx, o.Namespace, o.Name); err != nil {
				logf("  repair %s/%s failed: %v", o.Namespace, o.Name, err)
			} else {
				orphansRepairedTotal.WithLabelValues(class).Inc()
				logf("  repaired %s/%s [%s] — restarted for a fresh enrollment", o.Namespace, o.Name, class)
			}
		}
		if e := sleepCtx(ctx, opts.Interval); e != nil {
			return e
		}
	}
}

func restartPod(ctx context.Context, namespace, name string) error {
	c := k8s.GetClients()
	if c == nil {
		return fmt.Errorf("kubernetes clients not initialized")
	}
	return c.Clientset.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
