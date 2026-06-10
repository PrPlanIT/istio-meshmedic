package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/PrPlanIT/istio-meshmedic/src/agent"
	"github.com/PrPlanIT/istio-meshmedic/src/k8s"
)

var (
	agentNode        string
	agentHostProc    string
	agentInterval    time.Duration
	agentConfirm     time.Duration
	agentGracePeriod time.Duration
	agentAutoRepair  bool
	agentRepairStuck bool
	agentMetricsAddr string
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Run as a per-node operator (DaemonSet) — reads /host/proc, no probes",
	Long: `Runs meshmedic as a node-local operator. It reads each local pod's netns
sockets directly from /host/proc — no ephemeral container injected — and assesses
ambient enrollment INDEPENDENTLY of Kubernetes readiness. A pod orphaned badly
enough to be knocked not-Ready (init never completes, a dependency is unreachable)
is exactly the orphan a readiness gate would hide, so the scanner does not gate on
it. Every orphan is classified:

  ready      + orphaned  → capture lost on an otherwise-healthy pod
  not-ready  + orphaned  → "this pod appears unhealthy because it is orphaned"

Repair is policy, gated separately from detection:
  --auto-repair   restarts Ready orphans.
  --repair-stuck  restarts not-Ready orphans, but ONLY once a pod has remained
                  continuously not-Ready longer than --grace-period (the actionable
                  invariant) and still fails the post-confirm re-read.

With neither flag the agent is detect-and-surface: it logs every orphan and
exports per-node metrics (/metrics), so a stuck orphan is visible within one grace
period instead of sitting silently dead.

Intended to run as a DaemonSet with hostPID: true and the host /proc mounted at
/host/proc. See deploy/meshmedic-daemonset.yaml.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if agentNode == "" {
			agentNode = os.Getenv("NODE_NAME")
		}
		if agentNode == "" {
			return fmt.Errorf("node name required (set --node or the NODE_NAME env var)")
		}
		if _, err := k8s.Init(Kubeconfig); err != nil {
			return err
		}
		logf := func(f string, a ...any) { log.Printf(f, a...) }
		return agent.Run(context.Background(), agent.Options{
			Node:        agentNode,
			HostProc:    agentHostProc,
			Interval:    agentInterval,
			Confirm:     agentConfirm,
			GracePeriod: agentGracePeriod,
			AutoRepair:  agentAutoRepair,
			RepairStuck: agentRepairStuck,
			MetricsAddr: agentMetricsAddr,
		}, logf)
	},
}

func init() {
	agentCmd.Flags().StringVar(&agentNode, "node", "", "node name (default: $NODE_NAME)")
	agentCmd.Flags().StringVar(&agentHostProc, "host-proc", "/host/proc", "host /proc mount path")
	agentCmd.Flags().DurationVar(&agentInterval, "interval", 60*time.Second, "sweep interval")
	agentCmd.Flags().DurationVar(&agentConfirm, "confirm", 10*time.Second, "re-confirm delay before repair (flap guard)")
	agentCmd.Flags().DurationVar(&agentGracePeriod, "grace-period", 5*time.Minute, "a not-ready orphan is actionable only after it has remained continuously not-ready longer than this")
	agentCmd.Flags().BoolVar(&agentAutoRepair, "auto-repair", false, "restart Ready orphans (capture lost on a healthy pod)")
	agentCmd.Flags().BoolVar(&agentRepairStuck, "repair-stuck", false, "restart not-Ready orphans that stay orphaned past --grace-period")
	agentCmd.Flags().StringVar(&agentMetricsAddr, "metrics-addr", ":9100", "listen address for Prometheus /metrics (empty to disable)")
	RootCmd.AddCommand(agentCmd)
}
