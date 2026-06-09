// Package agent runs meshmedic as a per-node operator. It reads each local pod's
// netns sockets directly from /host/proc — no ephemeral container injected, no
// exec — detects ambient orphans on a loop, and re-enrolls the stuck ones with a
// flap-aware re-confirm (the socket state flaps, so a single missing reading must
// not trigger a restart). This is the zero-littering home for the netns detector.
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
	Node       string        // this node's name ($NODE_NAME)
	HostProc   string        // host /proc mount (default /host/proc)
	Interval   time.Duration // time between sweeps
	Confirm    time.Duration // re-confirm delay before acting on an orphan (flap guard)
	AutoRepair bool          // restart confirmed orphans
}

// Orphan is a detected local orphan.
type Orphan struct {
	Namespace string
	Name      string
	UID       string
	Present   []int
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

// ScanNode detects ambient orphans among the local node's pods by reading their
// netns sockets from hostProc. Read-only.
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
	var orphans []Orphan
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Annotations[redirectionAnnotation] != redirectionEnabled {
			continue
		}
		if p.Status.Phase != corev1.PodRunning || !k8s.PodReady(p) {
			continue
		}
		pid, ok := pids[string(p.UID)]
		if !ok {
			continue // couldn't map the PID this sweep — skip, don't false-flag
		}
		if present := readListeners(hostProc, pid); !scan.IsCaptured(present) {
			orphans = append(orphans, Orphan{Namespace: p.Namespace, Name: p.Name, UID: string(p.UID), Present: present})
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
	logf("meshmedic agent on node %q — interval %s, confirm %s, auto-repair %v",
		opts.Node, opts.Interval, opts.Confirm, opts.AutoRepair)

	for {
		orphans, err := ScanNode(ctx, opts.Node, opts.HostProc)
		switch {
		case err != nil:
			logf("sweep error: %v", err)
		case len(orphans) == 0:
			logf("sweep: 0 orphans")
		default:
			logf("sweep: %d orphan(s)", len(orphans))
			for _, o := range orphans {
				logf("  orphan %s/%s present=%v", o.Namespace, o.Name, o.Present)
				if !opts.AutoRepair {
					continue
				}
				if err := sleepCtx(ctx, opts.Confirm); err != nil {
					return err
				}
				if !stillOrphan(o.UID, opts.HostProc) {
					logf("  %s/%s recovered before repair (flap) — skipping", o.Namespace, o.Name)
					continue
				}
				if err := restartPod(ctx, o.Namespace, o.Name); err != nil {
					logf("  repair %s/%s failed: %v", o.Namespace, o.Name, err)
				} else {
					logf("  repaired %s/%s — restarted for a fresh enrollment", o.Namespace, o.Name)
				}
			}
		}
		if err := sleepCtx(ctx, opts.Interval); err != nil {
			return err
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
