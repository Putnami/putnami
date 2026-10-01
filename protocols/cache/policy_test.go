package cache

import "testing"

func TestSideEffectingTask(t *testing.T) {
	cases := map[string]bool{
		"publish":         true,
		"publish~npm":     true,
		"publish~docker":  true,
		"build":           false,
		"build~transpile": false,
		"test~unit":       false,
		"":                false,
	}
	for task, want := range cases {
		if got := SideEffectingTask(task); got != want {
			t.Errorf("SideEffectingTask(%q) = %v, want %v", task, got, want)
		}
	}
}

func TestWorthRemoteCaching(t *testing.T) {
	p := BreakEvenParams{BandwidthBytesPerSec: 1_000_000, MinDurationMs: 200}

	tests := []struct {
		name       string
		durationMs int64
		sizeBytes  int64
		want       bool
	}{
		{"files-less below duration floor", 100, 0, true}, // nothing to transfer: always worth sharing
		{"files-less unknown duration", 0, 0, true},
		{"unknown duration with bytes", 0, 10, false},
		{"cheap build, tiny output", 150, 1000, false}, // under floor
		{"slow build, no size info", 5000, 0, true},
		{"slow build, small output", 5000, 1_000_000, true}, // 1s transfer <= 5s build
		{"fast build, huge output", 300, 10_000_000, false}, // 10s transfer > 0.3s build
		{"transfer equals build", 1000, 1_000_000, true},    // 1000ms == 1000ms
		{"transfer over build", 1000, 1_500_000, false},     // 1500ms > 1000ms
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WorthRemoteCaching(tt.durationMs, tt.sizeBytes, p); got != tt.want {
				t.Errorf("WorthRemoteCaching(%d, %d) = %v, want %v", tt.durationMs, tt.sizeBytes, got, tt.want)
			}
		})
	}
}

func TestWorthRemoteCaching_NoBandwidthDisablesSizeCheck(t *testing.T) {
	p := BreakEvenParams{BandwidthBytesPerSec: 0, MinDurationMs: 200}
	if !WorthRemoteCaching(500, 1<<40, p) {
		t.Error("with bandwidth disabled, any artifact above the floor should be worth caching")
	}
	if WorthRemoteCaching(100, 1, p) {
		t.Error("below the floor should still be skipped")
	}
}

func TestEligibleForRemote(t *testing.T) {
	p := DefaultBreakEven

	// Side-effecting task is never eligible, even with an expensive build.
	if EligibleForRemote(KeyRequest{Task: "publish~npm", DurationMs: 60_000, SizeBytes: 1000}, p) {
		t.Error("publish task must never be remote-eligible")
	}

	// A slow, reasonably sized build is eligible.
	if !EligibleForRemote(KeyRequest{Task: "build~transpile", DurationMs: 60_000, SizeBytes: 5_000_000}, p) {
		t.Error("a slow build with modest output should be remote-eligible")
	}

	// A trivially cheap build with bytes to move is not worth it.
	if EligibleForRemote(KeyRequest{Task: "build~transpile", DurationMs: 50, SizeBytes: 4096}, p) {
		t.Error("a sub-floor build with output bytes should not be remote-eligible")
	}

	// A files-less result is always eligible, however cheap: sharing the
	// status costs nothing and excluding it is a permanent remote miss.
	if !EligibleForRemote(KeyRequest{Task: "build~config-merge", DurationMs: 50}, p) {
		t.Error("a files-less result must be remote-eligible regardless of duration")
	}
}
