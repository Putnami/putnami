package cli

import (
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

// wsOptions is the workspace `options` block. Naming it once keeps this file's
// tables off the module's untyped-payload ceiling (structural_baseline_test.go)
// while the literals below stay readable.
type wsOptions = map[string]map[string]any

func TestResolveCPUPolicyPrecedenceAndDefaults(t *testing.T) {
	tests := []struct {
		name     string
		flag     string
		env      string
		options  wsOptions
		commands []string
		want     string
	}{
		{name: "unset stays empty so the scheduler default applies", commands: []string{"build"}, want: ""},
		{
			name:     "global config",
			options:  wsOptions{"*": {cpuPolicyOption: "measured"}},
			commands: []string{"build"}, want: cpuPolicyMeasured,
		},
		{
			name:     "per-command config beats global",
			options:  wsOptions{"*": {cpuPolicyOption: "measured"}, "build": {cpuPolicyOption: "critical-path"}},
			commands: []string{"build"}, want: cpuPolicyCriticalPath,
		},
		{
			name:     "environment beats config",
			env:      "critical-path",
			options:  wsOptions{"build": {cpuPolicyOption: "measured"}},
			commands: []string{"build"}, want: cpuPolicyCriticalPath,
		},
		{
			name: "flag beats environment",
			flag: "measured", env: "critical-path",
			commands: []string{"build"}, want: cpuPolicyMeasured,
		},
		{
			name: "value is case-insensitive", flag: "MEASURED",
			commands: []string{"build"}, want: cpuPolicyMeasured,
		},
		{
			name: "agreeing commands resolve",
			options: wsOptions{
				"lint":  {cpuPolicyOption: "measured"},
				"build": {cpuPolicyOption: "measured"},
			},
			commands: []string{"lint", "build"}, want: cpuPolicyMeasured,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PUTNAMI_CPU_POLICY", tt.env)
			g := &GlobalFlags{CPUBudgetPolicy: tt.flag}
			if err := resolveCPUPolicy(g, &wsproto.Config{Options: tt.options}, tt.commands); err != nil {
				t.Fatalf("resolveCPUPolicy: %v", err)
			}
			if g.CPUBudgetPolicy != tt.want {
				t.Fatalf("CPUBudgetPolicy = %q, want %q", g.CPUBudgetPolicy, tt.want)
			}
		})
	}
}

// This policy is the variable a measurement campaign controls, so a value the
// operator did not ask for must never be substituted silently.
func TestResolveCPUPolicyRejectsUnknownValues(t *testing.T) {
	tests := []struct {
		name       string
		flag       string
		env        string
		options    wsOptions
		wantSource string
	}{
		{name: "flag", flag: "fastest", wantSource: "--cpu-policy"},
		{name: "environment", env: "makespan", wantSource: "PUTNAMI_CPU_POLICY"},
		{
			name:       "config",
			options:    wsOptions{"build": {cpuPolicyOption: "occupancy"}},
			wantSource: "options.build.cpu-policy",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PUTNAMI_CPU_POLICY", tt.env)
			g := &GlobalFlags{CPUBudgetPolicy: tt.flag}
			err := resolveCPUPolicy(g, &wsproto.Config{Options: tt.options}, []string{"build"})
			if err == nil {
				t.Fatal("expected an error naming the offending source")
			}
			if !strings.Contains(err.Error(), tt.wantSource) {
				t.Fatalf("error %q does not name the source %q", err, tt.wantSource)
			}
		})
	}
}

func TestResolveCPUPolicyRejectsNonStringAndConflictingConfig(t *testing.T) {
	t.Setenv("PUTNAMI_CPU_POLICY", "")

	err := resolveCPUPolicy(&GlobalFlags{}, &wsproto.Config{
		Options: wsOptions{"build": {cpuPolicyOption: 4}},
	}, []string{"build"})
	if err == nil || !strings.Contains(err.Error(), "must be a string") {
		t.Fatalf("non-string option error = %v", err)
	}

	// The allocator is run-scoped, so two commands cannot each get their own
	// ceiling policy; a conflict is a configuration error, not a silent winner.
	err = resolveCPUPolicy(&GlobalFlags{}, &wsproto.Config{
		Options: wsOptions{
			"lint":  {cpuPolicyOption: "measured"},
			"build": {cpuPolicyOption: "critical-path"},
		},
	}, []string{"lint", "build"})
	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("conflicting option error = %v", err)
	}
}
