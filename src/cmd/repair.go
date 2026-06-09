package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/PrPlanIT/istio-meshmedic/src/k8s"
	"github.com/PrPlanIT/istio-meshmedic/src/repair"
	"github.com/PrPlanIT/istio-meshmedic/src/scan"
)

var (
	repairNamespace  string
	repairBehavioral bool
	repairSince      string
	repairYes        bool
	repairProbeImage string
	repairOutput     string
)

var repairCmd = &cobra.Command{
	Use:   "repair",
	Short: "Re-enroll ambient orphans (gated; dry-run unless --yes)",
	Long: `Finds ambient orphans (same detector as scan) and re-enrolls each by toggling
the per-pod istio.io/dataplane-mode label off and back — forcing istio-cni to
recreate the missing in-pod ztunnel listeners WITHOUT restarting the pod — then
re-probes to confirm the capture returned.

Dry-run by default: reports what it WOULD repair and changes nothing. Pass --yes
to apply. Requires --namespace or --behavioral — it will not scan+repair the whole
cluster blindly.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if repairNamespace == "" && !repairBehavioral {
			return fmt.Errorf("repair requires --namespace or --behavioral (refusing to repair cluster-wide)")
		}
		if _, err := k8s.Init(Kubeconfig); err != nil {
			return err
		}
		ctx := context.Background()

		var candidates map[string]bool
		if repairBehavioral {
			since, err := time.ParseDuration(repairSince)
			if err != nil {
				return fmt.Errorf("invalid --since %q: %w", repairSince, err)
			}
			candidates, err = scan.BehavioralCandidates(ctx, int64(since.Seconds()))
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "behavioral pre-filter: %d candidate(s)\n", len(candidates))
			if len(candidates) == 0 {
				fmt.Println("No orphan candidates in ztunnel logs.")
				return nil
			}
		}

		results, err := repair.Repair(ctx, repairProbeImage, repairNamespace, candidates, repairYes)
		if err != nil {
			return err
		}

		if repairOutput == "json" {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(results)
		}
		printRepair(results, repairYes)

		for _, r := range results {
			if r.Action == "failed" {
				os.Exit(1)
			}
		}
		return nil
	},
}

func printRepair(results []repair.Result, applied bool) {
	if len(results) == 0 {
		fmt.Println("No orphans found.")
		return
	}
	mode := "DRY RUN — use --yes to apply"
	if applied {
		mode = "APPLIED"
	}
	fmt.Printf("Repair — %d orphan(s) [%s]\n\n", len(results), mode)
	for _, r := range results {
		icon := "~"
		switch r.Action {
		case "repaired":
			icon = "✓"
		case "failed":
			icon = "✗"
		}
		fmt.Printf("  %s %-50s %-13s %s\n", icon, r.Pod, r.Action, r.Detail)
	}
}

func init() {
	repairCmd.Flags().StringVarP(&repairNamespace, "namespace", "n", "", "limit repair to one namespace")
	repairCmd.Flags().BoolVar(&repairBehavioral, "behavioral", false, "find orphans via ztunnel rejection logs first")
	repairCmd.Flags().StringVar(&repairSince, "since", "15m", "ztunnel log window for --behavioral")
	repairCmd.Flags().BoolVar(&repairYes, "yes", false, "apply the repair (default: dry-run)")
	repairCmd.Flags().StringVarP(&repairOutput, "output", "o", "table", "output format: table|json")
	repairCmd.Flags().StringVar(&repairProbeImage, "probe-image", scan.DefaultProbeImage, "ephemeral netns probe image")
	RootCmd.AddCommand(repairCmd)
}
