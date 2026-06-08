package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/PrPlanIT/istio-meshmedic/src/version"
)

// Kubeconfig is the resolved kubeconfig path (empty = in-cluster → KUBECONFIG →
// ~/.kube/config). Bound to the persistent --kubeconfig flag.
var Kubeconfig string

// RootCmd is the meshmedic CLI root.
var RootCmd = &cobra.Command{
	Use:   "meshmedic",
	Short: "Detect and heal Istio ambient-mesh enrollment orphans",
	Long: `MeshMedic finds workloads that should be enrolled in the Istio ambient mesh
(captured by their node's ztunnel) but aren't — the silent capture-loss failure
mode where a pod was enrolled at startup, later lost its per-pod redirection,
and now sends plaintext that peer ztunnels reject ("policy rejection: allow
policies exist, but none allowed", empty src.identity). Left alone these pods
look healthy but can't reach mTLS peers until restarted.

meshmedic detects orphans (read-only) and, under strict safety gates, can
remediate them by restarting the affected pods.`,
	SilenceUsage:  true,
	SilenceErrors: false,
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("meshmedic %s (%s, %s)\n", version.Version, version.Commit, version.BuildDate)
	},
}

func init() {
	RootCmd.PersistentFlags().StringVar(&Kubeconfig, "kubeconfig", "",
		"path to kubeconfig (default: in-cluster, then KUBECONFIG, then ~/.kube/config)")
	RootCmd.AddCommand(versionCmd)
}
