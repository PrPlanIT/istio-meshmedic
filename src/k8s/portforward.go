package k8s

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// PortForwardGet establishes a one-shot port-forward to podPort on the named pod
// and performs an HTTP GET against path, returning the response body. This is how
// meshmedic reads ztunnel's admin endpoint (e.g. config_dump on :15000): that
// port binds localhost only and the ztunnel container ships no HTTP client to
// exec, so port-forward through the kubelet is the reliable path.
func PortForwardGet(ctx context.Context, pod, namespace string, podPort int, path string) ([]byte, error) {
	c := GetClients()
	if c == nil {
		return nil, fmt.Errorf("kubernetes clients not initialized")
	}

	req := c.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(pod).SubResource("portforward")

	transport, upgrader, err := spdy.RoundTripperFor(c.RestConfig)
	if err != nil {
		return nil, fmt.Errorf("spdy roundtripper: %w", err)
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", req.URL())

	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	defer close(stopCh)

	// "0:<podPort>" lets the OS pick a free local port.
	pf, err := portforward.New(dialer, []string{fmt.Sprintf("0:%d", podPort)},
		stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		return nil, fmt.Errorf("port-forward: %w", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- pf.ForwardPorts() }()

	select {
	case <-readyCh:
	case err := <-errCh:
		return nil, fmt.Errorf("port-forward not ready: %w", err)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("port-forward ready timeout")
	}

	fwd, err := pf.GetPorts()
	if err != nil || len(fwd) == 0 {
		return nil, fmt.Errorf("resolve local port: %w", err)
	}

	endpoint := fmt.Sprintf("http://127.0.0.1:%d%s", fwd[0].Local, path)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return body, fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	return body, nil
}
