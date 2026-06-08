package k8s

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProbeContainerName is the name of the ephemeral container meshmedic injects to
// read a pod's network namespace (e.g. /proc/net/tcp). It shares the pod's netns
// — as all ephemeral containers do — so its view of listening sockets IS the
// pod's. It adds no Linux capabilities, so it is permitted under baseline
// PodSecurity (reading /proc/net/tcp needs none).
const ProbeContainerName = "mm-probe"

// EnsureNetnsProbe injects (idempotently) a sleeping ephemeral container into the
// pod and waits for it to be Running, returning its name to exec into. Used to
// inspect the pod's netns even when the app container is distroless. The image
// must be present on the node (rate-limited clusters can't pull on demand) — the
// istio-cni image is a safe default since it runs on every ambient node.
func EnsureNetnsProbe(ctx context.Context, namespace, pod, image string) (string, error) {
	c := GetClients()
	if c == nil {
		return "", fmt.Errorf("kubernetes clients not initialized")
	}
	cs := c.Clientset

	p, err := cs.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	// Already running from a prior probe?
	for _, ec := range p.Status.EphemeralContainerStatuses {
		if ec.Name == ProbeContainerName && ec.State.Running != nil {
			return ProbeContainerName, nil
		}
	}

	// Add to spec if not already declared.
	declared := false
	for _, ec := range p.Spec.EphemeralContainers {
		if ec.Name == ProbeContainerName {
			declared = true
			break
		}
	}
	if !declared {
		p.Spec.EphemeralContainers = append(p.Spec.EphemeralContainers, corev1.EphemeralContainer{
			EphemeralContainerCommon: corev1.EphemeralContainerCommon{
				Name:                     ProbeContainerName,
				Image:                    image,
				ImagePullPolicy:          corev1.PullIfNotPresent,
				Command:                  []string{"sleep", "120"},
				TerminationMessagePolicy: corev1.TerminationMessageReadFile,
			},
		})
		if _, err := cs.CoreV1().Pods(namespace).UpdateEphemeralContainers(ctx, pod, p, metav1.UpdateOptions{}); err != nil {
			return "", fmt.Errorf("inject probe: %w", err)
		}
	}

	// Wait for Running.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		cur, err := cs.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
		if err == nil {
			for _, ec := range cur.Status.EphemeralContainerStatuses {
				if ec.Name != ProbeContainerName {
					continue
				}
				switch {
				case ec.State.Running != nil:
					return ProbeContainerName, nil
				case ec.State.Terminated != nil:
					return "", fmt.Errorf("probe terminated early: %s", ec.State.Terminated.Reason)
				case ec.State.Waiting != nil && ec.State.Waiting.Reason != "" &&
					ec.State.Waiting.Reason != "ContainerCreating":
					return "", fmt.Errorf("probe stuck: %s %s", ec.State.Waiting.Reason, ec.State.Waiting.Message)
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return "", fmt.Errorf("probe did not start within timeout (image %q present on node?)", image)
}
