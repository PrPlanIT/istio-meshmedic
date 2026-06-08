package main

import (
	"os"

	"github.com/PrPlanIT/istio-meshmedic/src/cmd"
)

func main() {
	if err := cmd.RootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
