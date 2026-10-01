package extension

import (
	"reflect"
	"testing"
)

// TestConformance_CommandTraitsVocabulary pins the trait fields themselves.
//
// A trait describes the COMMAND. `preflight: config-schema` was the one that
// described the WORKLOAD instead — it made core walk a project's Go imports
// and infra markers to decide whether an artifact core does not own had to
// exist before the job ran — and contract 3 deleted it. A new
// field here is a protocol decision; a field that asks core to inspect a
// workload is the regression this pin exists to catch.
func TestConformance_CommandTraitsVocabulary(t *testing.T) {
	want := []string{"Heavy", "RequiresRunnable", "InjectPublishChannels", "SideEffects"}
	typ := reflect.TypeOf(CommandTraits{})
	got := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CommandTraits fields = %v, want %v", got, want)
	}
}

// TestConformance_WellKnownVerbs pins the canonical verb vocabulary.
// Adding or removing a verb is a protocol change.
func TestConformance_WellKnownVerbs(t *testing.T) {
	want := []string{
		"build", "test", "lint", "format", "serve", "run",
		"package", "publish", "deploy",
		"workspace-fetch", "workspace-install", "deps-upgrade", "config-extract", "config-merge",
	}
	if len(WellKnownVerbs) != len(want) {
		t.Fatalf("WellKnownVerbs has %d entries, want %d", len(WellKnownVerbs), len(want))
	}
	for _, name := range want {
		spec, ok := WellKnownVerbs[name]
		if !ok {
			t.Errorf("well-known verb %q missing", name)
			continue
		}
		if spec.Description == "" {
			t.Errorf("verb %q requires a description", name)
		}
	}
}

// TestConformance_VerbTraits pins the default traits the orchestrator
// relies on. Changing any of these changes scheduling/gating behavior.
func TestConformance_VerbTraits(t *testing.T) {
	cases := []struct {
		verb string
		want CommandTraits
	}{
		// test heaviness is step-granular (declared on the runner step),
		// not a command-level default.
		{"test", CommandTraits{}},
		{"serve", CommandTraits{RequiresRunnable: true}},
		{"run", CommandTraits{RequiresRunnable: true}},
		{"package", CommandTraits{InjectPublishChannels: true}},
		// publish/deploy carry no orchestrator-side workload inspection.
		// Contract 3 deleted the `preflight: config-schema`
		// trait: whether a task needs another task's artifact is stated by
		// the consuming task's typed input port, not derived by core from
		// the workload's imports and infra markers.
		{"publish", CommandTraits{
			InjectPublishChannels: true,
			SideEffects:           SideEffectsRegistry,
		}},
		{"deploy", CommandTraits{
			SideEffects: SideEffectsCloud,
		}},
		{"build", CommandTraits{}},
	}
	for _, c := range cases {
		if got := EffectiveTraits(c.verb, nil); got != c.want {
			t.Errorf("EffectiveTraits(%q) = %+v, want %+v", c.verb, got, c.want)
		}
	}
	if !WellKnownVerbs["serve"].LongRunning {
		t.Error("serve must be long-running")
	}
	if WellKnownVerbs["run"].LongRunning {
		t.Error("run must not be long-running")
	}
}

func TestEffectiveTraits(t *testing.T) {
	// Pipeline step names resolve through the base command.
	if got := EffectiveTraits("serve~dev", nil); !got.RequiresRunnable {
		t.Error("serve~dev should inherit the serve verb's requiresRunnable trait")
	}
	// Manifest traits replace defaults wholesale.
	declared := &CommandTraits{RequiresRunnable: true}
	got := EffectiveTraits("publish", declared)
	if got.InjectPublishChannels || got.SideEffects != "" || !got.RequiresRunnable {
		t.Errorf("declared traits must replace defaults wholesale, got %+v", got)
	}
	// Unknown verbs default to zero traits.
	if got := EffectiveTraits("frobnicate", nil); got != (CommandTraits{}) {
		t.Errorf("unknown verb traits = %+v, want zero", got)
	}
}

func TestBaseCommandName(t *testing.T) {
	cases := map[string]string{
		"build":              "build",
		"build~transpile":    "build",
		"lint~golangci-lint": "lint",
	}
	for in, want := range cases {
		if got := BaseCommandName(in); got != want {
			t.Errorf("BaseCommandName(%q) = %q, want %q", in, got, want)
		}
	}
}
