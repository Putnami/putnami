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

// paramsVariants are the BreakEvenParams values the deprecated helpers must
// ignore: the default, the zero value, and one whose floor and bandwidth would
// exclude every bytes-carrying entry if anything read them.
var paramsVariants = map[string]BreakEvenParams{
	"default": DefaultBreakEven,
	"zero":    {},
	"extreme": {BandwidthBytesPerSec: 1, MinDurationMs: 1 << 62},
}

func TestWorthRemoteCaching_AdmitsEveryEntry(t *testing.T) {
	entries := []struct {
		name       string
		durationMs int64
		sizeBytes  int64
	}{
		{"files-less, cheap", 100, 0},
		{"files-less, unknown duration", 0, 0},
		{"bytes, unknown duration", 0, 10},
		{"bytes, cheap build", 150, 1000},
		{"bytes, fast build with huge output", 300, 10_000_000},
		{"bytes, slow build", 5000, 1_000_000},
	}
	for paramsName, p := range paramsVariants {
		for _, e := range entries {
			if !WorthRemoteCaching(e.durationMs, e.sizeBytes, p) {
				t.Errorf("WorthRemoteCaching(%s, params %s) = false, want true", e.name, paramsName)
			}
		}
	}
}

func TestEligibleForRemote_SideEffectsOnly(t *testing.T) {
	tests := []struct {
		name string
		key  KeyRequest
		want bool
	}{
		// A side-effecting task is never eligible, even with an expensive build.
		{"publish, slow build", KeyRequest{Task: "publish~npm", DurationMs: 60_000, SizeBytes: 1000}, false},
		{"publish, files-less", KeyRequest{Task: "publish"}, false},
		// Every other task is eligible, whatever its duration or size.
		{"build, cheap with output bytes", KeyRequest{Task: "build~transpile", DurationMs: 50, SizeBytes: 4096}, true},
		{"build, unknown duration with output bytes", KeyRequest{Task: "build~generate", SizeBytes: 4096}, true},
		{"build, fast with huge output", KeyRequest{Task: "build~bundle", DurationMs: 100, SizeBytes: 1 << 40}, true},
		{"build, slow with modest output", KeyRequest{Task: "build~transpile", DurationMs: 60_000, SizeBytes: 5_000_000}, true},
		{"validate, cheap files-less", KeyRequest{Task: "validate~specs", DurationMs: 50}, true},
	}
	for paramsName, p := range paramsVariants {
		for _, tt := range tests {
			if got := EligibleForRemote(tt.key, p); got != tt.want {
				t.Errorf("EligibleForRemote(%s, params %s) = %v, want %v", tt.name, paramsName, got, tt.want)
			}
		}
	}
}
