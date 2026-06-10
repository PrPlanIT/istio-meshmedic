package agent

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNotReadyDuration(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	withReady := func(status corev1.ConditionStatus, ltt time.Time) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: status, LastTransitionTime: metav1.NewTime(ltt)},
		}}}
	}

	// Ready → zero, regardless of when it transitioned.
	if d := notReadyDuration(withReady(corev1.ConditionTrue, now.Add(-time.Hour)), now); d != 0 {
		t.Errorf("ready pod: got %v, want 0", d)
	}
	// Not-Ready → measured from the condition's last transition.
	if d := notReadyDuration(withReady(corev1.ConditionFalse, now.Add(-30*time.Minute)), now); d != 30*time.Minute {
		t.Errorf("not-ready pod: got %v, want 30m", d)
	}
	// No Ready condition yet (wedged in init) → measured from start time.
	initStuck := &corev1.Pod{Status: corev1.PodStatus{StartTime: &metav1.Time{Time: now.Add(-2 * time.Hour)}}}
	if d := notReadyDuration(initStuck, now); d != 2*time.Hour {
		t.Errorf("init-stuck pod: got %v, want 2h", d)
	}
}

func TestStuckClassification(t *testing.T) {
	grace := 5 * time.Minute
	cases := []struct {
		name  string
		ready bool
		dur   time.Duration
		stuck bool
	}{
		{"ready orphan is never stuck", true, 0, false},
		{"not-ready within grace", false, 2 * time.Minute, false},
		{"not-ready past grace", false, 10 * time.Minute, true},
		{"not-ready exactly at grace", false, 5 * time.Minute, false}, // strictly greater
	}
	for _, c := range cases {
		o := Orphan{Ready: c.ready, NotReadyDuration: c.dur}
		if got := o.Stuck(grace); got != c.stuck {
			t.Errorf("%s: Stuck=%v, want %v", c.name, got, c.stuck)
		}
	}
}

func TestClassifyCounts(t *testing.T) {
	grace := 5 * time.Minute
	orphans := []Orphan{
		{Ready: true},
		{Ready: false, NotReadyDuration: 1 * time.Minute},  // not-ready, not stuck
		{Ready: false, NotReadyDuration: 10 * time.Minute}, // stuck
		{Ready: false, NotReadyDuration: 20 * time.Minute}, // stuck
	}
	ready, notReady, stuck := classify(orphans, grace)
	if ready != 1 || notReady != 3 || stuck != 2 {
		t.Errorf("classify: ready=%d notReady=%d stuck=%d, want 1/3/2", ready, notReady, stuck)
	}
}
