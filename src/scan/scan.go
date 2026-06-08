package scan

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/PrPlanIT/istio-meshmedic/src/k8s"
)

const (
	// RedirectionAnnotation is set by istio-cni on pods it has enrolled into the
	// ambient mesh. Its presence means the pod is *expected* to be captured — but
	// the annotation can persist after capture is silently lost, which is exactly
	// the orphan condition this detector finds.
	RedirectionAnnotation = "ambient.istio.io/redirection"
	redirectionEnabled    = "enabled"
)

// DefaultProbeImage runs on every ambient node (it's the istio-cni image), so it
// is guaranteed present and needs no pull on a rate-limited cluster.
const DefaultProbeImage = "docker.io/istio/install-cni:1.29.1"

// Workload identifies a pod meshmedic cares about.
type Workload struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Node      string `json:"node"`
	PodIP     string `json:"podIP"`
}

// Orphan is an ambient-annotated, healthy-looking pod whose network namespace is
// missing ztunnel's in-pod capture listeners — the silent capture-loss failure
// mode (istio.io/issue 55968 / 57285). Note: such pods are still present in
// ztunnel's workloadState, which is why a control-plane scan misses them and
// this netns-socket check is required.
type Orphan struct {
	Workload
	PresentPorts []int  `json:"presentZtunnelPorts"` // which of 15001/15006/15008/15053 were listening
	Reason       string `json:"reason"`
}

// Report is the result of a scan.
type Report struct {
	Checked      int               `json:"checked"`
	Healthy      int               `json:"healthy"`
	Orphans      []Orphan          `json:"orphans"`
	Unverifiable map[string]string `json:"unverifiable,omitempty"` // ns/name -> why the netns could not be read
}

// Scan finds ambient-annotated, Running+Ready pods whose network namespace lacks
// ztunnel's in-pod capture listeners (15001/15008). For each candidate it injects
// a baseline-safe ephemeral probe and reads /proc/net/tcp{,6} from the pod's own
// netns. Read-only with respect to mesh state (it does inject probe containers).
func Scan(ctx context.Context, probeImage string) (*Report, error) {
	c := k8s.GetClients()
	if c == nil {
		return nil, fmt.Errorf("kubernetes clients not initialized")
	}
	if probeImage == "" {
		probeImage = DefaultProbeImage
	}

	pods, err := c.Clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}

	report := &Report{Unverifiable: map[string]string{}}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Annotations[RedirectionAnnotation] != redirectionEnabled {
			continue
		}
		// Orphans hide among healthy-LOOKING pods: Running, Ready, with a netns.
		// Not-ready pods are excluded — they're broken for other reasons.
		if p.Spec.NodeName == "" || p.Status.Phase != corev1.PodRunning || !k8s.PodReady(p) {
			continue
		}
		key := p.Namespace + "/" + p.Name

		present, err := probeListeners(ctx, p, probeImage)
		if err != nil {
			report.Unverifiable[key] = err.Error()
			continue
		}
		report.Checked++
		if isCaptured(present) {
			report.Healthy++
			continue
		}
		report.Orphans = append(report.Orphans, Orphan{
			Workload:     Workload{Namespace: p.Namespace, Name: p.Name, Node: p.Spec.NodeName, PodIP: p.Status.PodIP},
			PresentPorts: present,
			Reason:       "ambient-annotated but netns missing ztunnel capture listeners (15001/15008)",
		})
	}

	sort.Slice(report.Orphans, func(i, j int) bool {
		if report.Orphans[i].Namespace != report.Orphans[j].Namespace {
			return report.Orphans[i].Namespace < report.Orphans[j].Namespace
		}
		return report.Orphans[i].Name < report.Orphans[j].Name
	})
	return report, nil
}

// probeListeners injects an ephemeral probe into the pod and reads its netns
// /proc/net/tcp{,6}, returning which ztunnel in-pod ports are LISTENing.
func probeListeners(ctx context.Context, p *corev1.Pod, image string) ([]int, error) {
	probe, err := k8s.EnsureNetnsProbe(ctx, p.Namespace, p.Name, image)
	if err != nil {
		return nil, err
	}
	res, err := k8s.ExecCommand(ctx, p.Name, p.Namespace, probe,
		[]string{"cat", "/proc/net/tcp", "/proc/net/tcp6"})
	if err != nil {
		return nil, fmt.Errorf("read /proc/net/tcp: %w", err)
	}
	return parseListenPorts(res.Stdout, ztunnelInPodPorts), nil
}
