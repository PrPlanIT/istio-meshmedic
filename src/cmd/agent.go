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
	agentNode       string
	agentHostProc   string
	agentInterval   time.Duration
	agentConfirm    time.Duration
	agentAutoRepair bool
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Run as a per-node operator (DaemonSet) — reads /host/proc, no probes",
	Long: `Runs meshmedic as a node-local operator. It reads each local pod's netns
sockets directly from /host/proc — no ephemeral container injected — and detects
ambient orphans on a loop. With --auto-repair it restarts confirmed orphans, but
only after re-confirming the pod is still missing capture --confirm later (the
flap guard: the socket state flaps, so a single missing reading must not trigger
a restart).

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
			Node:       agentNode,
			HostProc:   agentHostProc,
			Interval:   agentInterval,
			Confirm:    agentConfirm,
			AutoRepair: agentAutoRepair,
		}, logf)
	},
}

func init() {
	agentCmd.Flags().StringVar(&agentNode, "node", "", "node name (default: $NODE_NAME)")
	agentCmd.Flags().StringVar(&agentHostProc, "host-proc", "/host/proc", "host /proc mount path")
	agentCmd.Flags().DurationVar(&agentInterval, "interval", 60*time.Second, "sweep interval")
	agentCmd.Flags().DurationVar(&agentConfirm, "confirm", 10*time.Second, "re-confirm delay before repair (flap guard)")
	agentCmd.Flags().BoolVar(&agentAutoRepair, "auto-repair", false, "restart confirmed orphans (default: detect-only)")
	RootCmd.AddCommand(agentCmd)
}
