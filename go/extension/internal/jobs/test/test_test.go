package test

import (
	"reflect"
	"testing"
)

func TestAppendParallelArgsKeepsPackageAndTestWidthsDistinct(t *testing.T) {
	tests := []struct {
		name            string
		packageParallel string
		parallel        string
		want            []string
	}{
		{
			name:            "both explicit",
			packageParallel: "4",
			parallel:        "7",
			want:            []string{"test", "-p", "4", "-parallel", "7"},
		},
		{
			name:            "package width only",
			packageParallel: "6",
			want:            []string{"test", "-p", "6"},
		},
		{
			name:     "test width only",
			parallel: "8",
			want:     []string{"test", "-parallel", "8"},
		},
		{
			name: "both absent preserves Go defaults",
			want: []string{"test"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := appendParallelArgs([]string{"test"}, tt.packageParallel, tt.parallel)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parallel args = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- isNumeric ---

func TestIsNumeric_ValidNumbers(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"0", true},
		{"1", true},
		{"42", true},
		{"1000", true},
		{"9999999", true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := isNumeric(tt.input); got != tt.want {
				t.Errorf("isNumeric(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsNumeric_NonNumeric(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"30s", false},
		{"1m", false},
		{"abc", false},
		{"1.5", false},
		{"-1", false},
		{"1 2", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := isNumeric(tt.input); got != tt.want {
				t.Errorf("isNumeric(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsNumeric_EmptyString(t *testing.T) {
	if isNumeric("") {
		t.Error("isNumeric(\"\") should return false")
	}
}

// --- resolveCount ---

func TestResolveCount(t *testing.T) {
	tests := []struct {
		name      string
		count     string
		threshold float64
		want      string
	}{
		// Gate active and no explicit count → force a fresh run so the merged
		// coverage profile is not polluted by stale cached fragments.
		{"gate active, no count", "", 70, "1"},
		// No gate → preserve Go's test caching (empty count, no -count flag).
		{"no gate, no count", "", 0, ""},
		// An explicit count is always honored, even with the gate active.
		{"gate active, explicit count", "3", 70, "3"},
		{"no gate, explicit count", "3", 0, "3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveCount(tt.count, tt.threshold); got != tt.want {
				t.Errorf("resolveCount(%q, %v) = %q, want %q", tt.count, tt.threshold, got, tt.want)
			}
		})
	}
}
