package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/PrPlanIT/istio-meshmedic/src/k8s"
	"github.com/PrPlanIT/istio-meshmedic/src/scan"
)

var (
	scanOutput     string
	scanProbeImage string
	scanNamespace  string
	scanBehavioral bool
	scanSince      string
)

var scanCmd = &cobra.Command{
	Use:   "scan",
	Short: "Detect ambient-mesh enrollment orphans (read-only)",
	Long: `Finds ambient-annotated, healthy-looking pods whose network namespace is
missing ztunnel's in-pod capture listeners (15001/15006/15008/15053) — the silent
capture-loss failure mode (istio.io issue 55968 / 57285).

It checks the pod's netns directly (via a baseline-safe ephemeral probe reading
/proc/net/tcp), NOT ztunnel's workloadState — orphans remain present in
workloadState, so a control-plane scan misses them. Exits non-zero when orphans
are found.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := k8s.Init(Kubeconfig); err != nil {
			return err
		}
		ctx := context.Background()

		// Behavioral pre-filter: narrow to pods whose breakage showed up in
		// ztunnel's rejection logs, so we probe a handful instead of every ambient
		// pod. Nil candidates = probe all (the exhaustive netns sweep).
		var candidates map[string]bool
		if scanBehavioral {
			since, err := time.ParseDuration(scanSince)
			if err != nil {
				return fmt.Errorf("invalid --since %q: %w", scanSince, err)
			}
			candidates, err = scan.BehavioralCandidates(ctx, int64(since.Seconds()))
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "behavioral pre-filter: %d candidate(s) from ztunnel logs (last %s)\n",
				len(candidates), scanSince)
			if len(candidates) == 0 {
				fmt.Println("No orphan candidates in ztunnel logs — no active rejections in the window.")
				return nil
			}
		}

		report, err := scan.Scan(ctx, scanProbeImage, scanNamespace, candidates)
		if err != nil {
			return err
		}

		switch scanOutput {
		case "json":
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(report); err != nil {
				return err
			}
		default:
			printScanTable(report)
		}

		if len(report.Orphans) > 0 {
			os.Exit(1)
		}
		return nil
	},
}

func printScanTable(r *scan.Report) {
	fmt.Printf("Ambient capture scan — %d checked, %d healthy, %d orphan(s)\n",
		r.Checked, r.Healthy, len(r.Orphans))

	if len(r.Unverifiable) > 0 {
		fmt.Printf("\nUnverifiable (%d — could not read netns):\n", len(r.Unverifiable))
		for k, why := range r.Unverifiable {
			fmt.Printf("  ? %-50s %s\n", k, why)
		}
	}

	if len(r.Orphans) == 0 {
		fmt.Println("\nNo orphans.")
		return
	}
	fmt.Printf("\nOrphans (%d) — annotated ambient-enrolled but netns missing ztunnel listeners:\n", len(r.Orphans))
	for _, o := range r.Orphans {
		fmt.Printf("  ✗ %-50s node=%-20s present=%v\n", o.Namespace+"/"+o.Name, o.Node, o.PresentPorts)
	}
}

func init() {
	scanCmd.Flags().StringVarP(&scanOutput, "output", "o", "table", "output format: table|json")
	scanCmd.Flags().StringVarP(&scanNamespace, "namespace", "n", "",
		"limit the scan to one namespace (default: all — probes every ambient pod)")
	scanCmd.Flags().BoolVar(&scanBehavioral, "behavioral", false,
		"pre-filter candidates from ztunnel rejection logs (cheap; probes only flagged pods)")
	scanCmd.Flags().StringVar(&scanSince, "since", "15m", "ztunnel log window for --behavioral")
	scanCmd.Flags().StringVar(&scanProbeImage, "probe-image", scan.DefaultProbeImage,
		"image for the ephemeral netns probe (must be present on the node)")
	RootCmd.AddCommand(scanCmd)
}
