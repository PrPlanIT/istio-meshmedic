package scan

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/PrPlanIT/istio-meshmedic/src/k8s"
)

// ztunnelSelector matches ztunnel pods in any namespace (istio-system here, or a
// custom mesh namespace — auto-discovered rather than hardcoded).
const ztunnelSelector = "app=ztunnel"

var (
	reDstWorkload  = regexp.MustCompile(`dst\.workload="([^"]*)"`)
	reDstNamespace = regexp.MustCompile(`dst\.namespace="([^"]*)"`)
	reSrcWorkload  = regexp.MustCompile(`src\.workload="([^"]*)"`)
	reSrcNamespace = regexp.MustCompile(`src\.namespace="([^"]*)"`)
	reSrcAddrIP    = regexp.MustCompile(`src\.addr=([0-9.]+):`)
)

// BehavioralCandidates scrapes every ztunnel's recent access logs for the two
// orphan signatures and returns the set of candidate pods ("namespace/name"),
// without injecting anything. It is the cheap radar that narrows "all ambient
// pods" to "the ones whose breakage is actually disrupting traffic", so the
// netns probe only has to confirm a handful:
//
//   - dst HBONE refused — "Connection refused (os error 111)" to dst.addr=…:15008
//     means the dest workload's in-pod ztunnel sockets are missing (#57285). The
//     dest is named precisely (dst.workload/dst.namespace).
//   - inbound policy rejection — "allow policies exist, but none allowed" with no
//     source identity means the source lost outbound capture (#55968). The source
//     usually has no resolved workload, so it is mapped from src.addr via the
//     pod-IP index, best-effort (often a node IP that doesn't resolve).
//
// Only Running+Ready pods are returned: an orphan looks healthy, and a refused
// connection to a pod that is simply down is "down", not "orphaned".
//
// Blind spot (why this is a pre-filter, not a detector): an idle orphan that is
// neither sending nor receiving mesh traffic in the log window leaves no trail.
func BehavioralCandidates(ctx context.Context, sinceSeconds int64) (map[string]bool, error) {
	c := k8s.GetClients()
	if c == nil {
		return nil, fmt.Errorf("kubernetes clients not initialized")
	}
	cs := c.Clientset

	pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	byIP := make(map[string]string)  // podIP -> ns/name
	ready := make(map[string]bool)   // ns/name -> healthy-looking (Running+Ready)
	for i := range pods.Items {
		p := &pods.Items[i]
		key := p.Namespace + "/" + p.Name
		if p.Status.PodIP != "" {
			byIP[p.Status.PodIP] = key
		}
		ready[key] = k8s.PodReady(p) && p.Status.Phase == corev1.PodRunning
	}

	zpods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{LabelSelector: ztunnelSelector})
	if err != nil {
		return nil, fmt.Errorf("list ztunnel pods: %w", err)
	}
	if len(zpods.Items) == 0 {
		return nil, fmt.Errorf("no ztunnel pods found (selector %q) — is this an ambient mesh?", ztunnelSelector)
	}

	candidates := make(map[string]bool)
	for i := range zpods.Items {
		z := &zpods.Items[i]
		logs, err := podLogs(ctx, z.Namespace, z.Name, sinceSeconds)
		if err != nil {
			continue // best-effort per ztunnel; one unreadable log is not fatal
		}
		scanZtunnelLog(logs, byIP, ready, candidates)
	}
	return candidates, nil
}

// scanZtunnelLog parses one ztunnel's log text and adds orphan candidates.
func scanZtunnelLog(logs string, byIP map[string]string, ready, out map[string]bool) {
	for _, line := range strings.Split(logs, "\n") {
		switch {
		// Signal B: inbound HBONE refused → the dest's in-pod sockets are missing.
		case strings.Contains(line, "Connection refused (os error 111)") && strings.Contains(line, ":15008"):
			ns, wl := match(reDstNamespace, line), match(reDstWorkload, line)
			if ns != "" && wl != "" {
				if key := ns + "/" + wl; ready[key] {
					out[key] = true
				}
			}
		// Signal A: inbound policy rejection with no source identity → the source
		// lost outbound capture. Prefer a resolved src.workload; else map src.addr.
		case strings.Contains(line, "policy rejection: allow policies exist, but none allowed"):
			if ns, wl := match(reSrcNamespace, line), match(reSrcWorkload, line); ns != "" && wl != "" {
				if key := ns + "/" + wl; ready[key] {
					out[key] = true
				}
				continue
			}
			if ip := match(reSrcAddrIP, line); ip != "" {
				if key, ok := byIP[ip]; ok && ready[key] {
					out[key] = true
				}
			}
		}
	}
}

func match(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); len(m) > 1 {
		return m[1]
	}
	return ""
}

// podLogs streams a pod's recent logs as text.
func podLogs(ctx context.Context, namespace, name string, sinceSeconds int64) (string, error) {
	c := k8s.GetClients()
	if c == nil {
		return "", fmt.Errorf("kubernetes clients not initialized")
	}
	req := c.Clientset.CoreV1().Pods(namespace).GetLogs(name, &corev1.PodLogOptions{SinceSeconds: &sinceSeconds})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	var sb strings.Builder
	if _, err := io.Copy(&sb, stream); err != nil {
		return sb.String(), err
	}
	return sb.String(), nil
}
