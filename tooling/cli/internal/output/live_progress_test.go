package output

import (
	"strings"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/jobs"
)

// wallJob is a test job carrying a learned expected wall-clock duration, the
// weight that drives the global progress bar.
func wallJob(project, cmd string, expectedMs int64) *jobs.ScheduledJob {
	j := makeLiveTestJob(project, cmd)
	j.ExpectedWallMs = expectedMs
	return j
}

func TestSessionProgressWeightsByExpectedWall(t *testing.T) {
	// build is expected to take 9× as long as lint. Completing build alone
	// should fill the bar to ~90% — the EMA weighting, not a 1-of-2 count.
	build := wallJob("app", "build", 9000)
	lint := wallJob("app", "lint", 1000)
	r, _ := newTestRenderer(t, 120, build, lint)

	r.JobStart(build)
	r.JobComplete(build, &jobs.JobResult{Status: "success"})

	frac, _, _ := r.sessionProgress(time.Now())
	if frac < 0.88 || frac > 0.92 {
		t.Fatalf("weighted fraction after the long task = %.3f, want ~0.90", frac)
	}
}

func TestSessionProgressPartialCreditForRunningTask(t *testing.T) {
	build := wallJob("app", "build", 10000) // 10s expected
	r, _ := newTestRenderer(t, 120, build)

	base := time.Unix(2000, 0)
	r.start = base
	r.taskProg[build.Key()].startTime = base

	// Halfway through a 10s task → ~50% earned even though nothing finished.
	if frac, _, _ := r.sessionProgress(base.Add(5 * time.Second)); frac < 0.45 || frac > 0.55 {
		t.Fatalf("partial credit at half a task = %.3f, want ~0.50", frac)
	}

	// An overrunning task must not claim 100% until it actually completes.
	if frac, _, _ := r.sessionProgress(base.Add(60 * time.Second)); frac >= 1.0 {
		t.Fatalf("overrunning task should cap below 100%%, got %.3f", frac)
	}
}

func TestSessionProgressColdStoreIsCountFraction(t *testing.T) {
	// No history anywhere → equal weights → the bar tracks task count.
	a := makeLiveTestJob("a", "build")
	b := makeLiveTestJob("b", "build")
	c := makeLiveTestJob("c", "build")
	d := makeLiveTestJob("d", "build")
	r, _ := newTestRenderer(t, 120, a, b, c, d)

	for _, j := range []*jobs.ScheduledJob{a, b} {
		r.JobStart(j)
		r.JobComplete(j, &jobs.JobResult{Status: "success"})
	}

	if frac, _, _ := r.sessionProgress(time.Now()); frac < 0.49 || frac > 0.51 {
		t.Fatalf("cold-store fraction after 2/4 done = %.3f, want 0.50", frac)
	}
}

func TestETAFlooredByLongestRemainingTask(t *testing.T) {
	// Two quick tasks finish fast while one 60s task has barely started. A naive
	// throughput estimate would extrapolate the early parallel rate and report a
	// few seconds; the ETA must not be more optimistic than the long task's own
	// remaining time.
	long := wallJob("slow", "test", 60000)
	q1 := wallJob("a", "lint", 1000)
	q2 := wallJob("b", "lint", 1000)
	r, _ := newTestRenderer(t, 120, long, q1, q2)

	base := time.Unix(1000, 0)
	r.start = base
	for _, j := range []*jobs.ScheduledJob{q1, q2} {
		r.JobStart(j)
		r.JobComplete(j, &jobs.JobResult{Status: "success"})
	}
	r.taskProg[long.Key()].startTime = base

	_, eta, hasETA := r.sessionProgress(base.Add(2 * time.Second))
	if !hasETA {
		t.Fatal("expected an ETA")
	}
	if eta < 50*time.Second {
		t.Errorf("ETA %v is too optimistic; the long task needs ~58s more", eta)
	}
}

func TestETANeverIncreases(t *testing.T) {
	// A long task runs while one quick task already finished. As wall time
	// passes the measured throughput rate falls (fewer completions per second),
	// which would push a naive estimate upward — the displayed ETA must only
	// ever decrease.
	long := wallJob("slow", "build", 30000)
	quick := wallJob("q", "lint", 2000)
	r, _ := newTestRenderer(t, 120, long, quick)

	base := time.Unix(1000, 0)
	r.start = base
	r.JobStart(quick)
	r.JobComplete(quick, &jobs.JobResult{Status: "success"})
	r.taskProg[long.Key()].startTime = base

	prev := time.Duration(1) << 62
	for s := 1; s <= 25; s++ {
		_, eta, ok := r.sessionProgress(base.Add(time.Duration(s) * time.Second))
		if !ok {
			continue
		}
		if eta > prev {
			t.Fatalf("ETA increased at t=%ds: %v > %v", s, eta, prev)
		}
		prev = eta
	}
}

func TestFormatETA(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{400 * time.Millisecond, "<1s"},
		{3 * time.Second, "5s"},
		{7 * time.Second, "10s"},
		{42 * time.Second, "45s"},
		{75 * time.Second, "1m15s"},
		{125 * time.Second, "2m05s"},
		{3725 * time.Second, "1h02m"},
	}
	for _, tt := range tests {
		if got := formatETA(tt.d); got != tt.want {
			t.Errorf("formatETA(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestMiniProgressShowsBarAndETANoPercent(t *testing.T) {
	build := wallJob("app", "build", 4000)
	lint := wallJob("app", "lint", 4000)
	r, _ := newTestRenderer(t, 120, build, lint)

	base := time.Unix(5000, 0)
	r.start = base
	r.JobStart(build)
	r.JobComplete(build, &jobs.JobResult{Status: "success"})

	mini := r.renderMiniProgress(base.Add(2*time.Second), 40)
	if !strings.Contains(mini, "█") {
		t.Errorf("expected a filled bar segment: %q", mini)
	}
	if !strings.Contains(mini, "~") {
		t.Errorf("expected an ETA in the mini bar: %q", mini)
	}
	if strings.Contains(mini, "%") {
		t.Errorf("mini bar must not show a percentage: %q", mini)
	}
}

func TestMiniProgressEmptyWhenBudgetTooSmall(t *testing.T) {
	build := wallJob("app", "build", 4000)
	r, _ := newTestRenderer(t, 120, build)
	if got := r.renderMiniProgress(time.Now(), miniBarMin-1); got != "" {
		t.Errorf("tiny budget should yield no bar, got %q", got)
	}
}

func TestHeaderEmbedsMiniBar(t *testing.T) {
	build := wallJob("app", "build", 4000)
	lint := wallJob("app", "lint", 4000)
	r, _ := newTestRenderer(t, 120, build, lint)

	base := time.Unix(5000, 0)
	r.start = base
	r.JobStart(build)
	r.JobComplete(build, &jobs.JobResult{Status: "success"})

	header := r.renderHeaderAt(base.Add(2 * time.Second))
	if !strings.Contains(header, "impacted: 1 projects") && !strings.Contains(header, "impacted: 2 projects") {
		t.Errorf("header should retain the counts: %q", header)
	}
	if !strings.Contains(header, "█") {
		t.Errorf("header should embed the mini progress bar: %q", header)
	}
}
