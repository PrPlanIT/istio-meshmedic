package k8s

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// ExecResult holds the captured output of a pod exec.
type ExecResult struct {
	Stdout string
	Stderr string
}

// ExecCommand runs a command in a pod container via the Kubernetes exec API and
// returns its captured stdout/stderr. No stdin is attached.
func ExecCommand(ctx context.Context, pod, namespace, container string, command []string) (*ExecResult, error) {
	c := GetClients()
	if c == nil {
		return nil, fmt.Errorf("kubernetes clients not initialized")
	}

	req := c.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(c.RestConfig, "POST", req.URL())
	if err != nil {
		return nil, fmt.Errorf("create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
	res := &ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		return res, fmt.Errorf("exec failed: %w (stderr: %s)", err, stderr.String())
	}
	return res, nil
}
