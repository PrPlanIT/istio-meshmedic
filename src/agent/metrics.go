package agent

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Per-node orphan metrics. Gauges reflect the most recent sweep; counters are
// cumulative. The point is to answer "did the change actually catch anything?"
// without grepping logs — in particular meshmedic_orphans_stuck > 0 is the signal
// that a not-Ready orphan is sitting there that a readiness gate used to hide.
var (
	orphansTotal = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "meshmedic_orphans_total",
		Help: "Ambient orphans detected on this node in the last sweep.",
	})
	orphansReady = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "meshmedic_orphans_ready",
		Help: "Orphans that are pod-Ready (capture lost on an otherwise-healthy pod).",
	})
	orphansNotReady = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "meshmedic_orphans_not_ready",
		Help: "Orphans that are not pod-Ready.",
	})
	orphansStuck = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "meshmedic_orphans_stuck",
		Help: "Not-Ready orphans past the grace period (actionable as stuck orphans).",
	})
	orphansRepairedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshmedic_orphans_repaired_total",
		Help: "Cumulative orphan repairs (restarts), by class.",
	}, []string{"class"}) // class = ready | stuck
	sweepsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "meshmedic_sweeps_total",
		Help: "Cumulative completed sweeps.",
	})
	sweepErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "meshmedic_sweep_errors_total",
		Help: "Cumulative sweep errors.",
	})
)

// recordSweepMetrics updates the per-sweep gauges from the orphan set.
func recordSweepMetrics(orphans []Orphan, grace time.Duration) {
	ready, notReady, stuck := classify(orphans, grace)
	orphansTotal.Set(float64(len(orphans)))
	orphansReady.Set(float64(ready))
	orphansNotReady.Set(float64(notReady))
	orphansStuck.Set(float64(stuck))
}

// serveMetrics starts a background HTTP server exposing /metrics and /healthz.
func serveMetrics(addr string, logf func(string, ...any)) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logf("metrics server: %v", err)
		}
	}()
}
