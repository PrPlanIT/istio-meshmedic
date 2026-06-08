package k8s

import (
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Clients holds the initialized Kubernetes client set.
type Clients struct {
	Clientset  kubernetes.Interface
	RestConfig *rest.Config
}

var (
	clients     *Clients
	clientsOnce sync.Once
	clientsErr  error
)

// Init initializes the Kubernetes clients. Safe to call multiple times; only the
// first call performs initialization. Pass kubeconfig="" for standard resolution
// (in-cluster → KUBECONFIG → ~/.kube/config).
func Init(kubeconfig string) (*Clients, error) {
	clientsOnce.Do(func() {
		var cfg *rest.Config
		if kubeconfig != "" {
			cfg, clientsErr = clientcmd.BuildConfigFromFlags("", kubeconfig)
		} else {
			cfg, clientsErr = rest.InClusterConfig()
			if clientsErr != nil {
				rules := clientcmd.NewDefaultClientConfigLoadingRules()
				cfg, clientsErr = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
					rules, &clientcmd.ConfigOverrides{}).ClientConfig()
			}
		}
		if clientsErr != nil {
			clientsErr = fmt.Errorf("build kubeconfig: %w", clientsErr)
			return
		}
		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			clientsErr = fmt.Errorf("create clientset: %w", err)
			return
		}
		clients = &Clients{Clientset: cs, RestConfig: cfg}
	})
	return clients, clientsErr
}

// GetClients returns the cached clients. Init must be called first.
func GetClients() *Clients { return clients }

// PodReady reports whether a pod's Ready condition is true.
func PodReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
