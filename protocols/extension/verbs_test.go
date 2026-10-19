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
// workload is the regression this pin exists to catch. Workload is not one: it
// names how the command runs (serve or once), and core reads nothing of the
// workload's sources to answer it.
func TestConformance_CommandTraitsVocabulary(t *testing.T) {
	want := []string{
		"Heavy", "RequiresRunnable", "InjectPublishChannels", "SideEffects",
		"Kind", "Workload", "RewritesSources", "ConsumesReleaseSet",
	}
	typ := reflect.TypeOf(CommandTraits{})
	got := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CommandTraits fields = %v, want %v", got, want)
	}
}

// TestConformance_WellKnownVerbs pins the lifecycle verb registry: five rows,
// no development verb. Adding or removing a verb is a protocol change.
func TestConformance_WellKnownVerbs(t *testing.T) {
	want := []string{
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
		if spec.LongRunning || spec.Traits != (CommandTraits{}) {
			t.Errorf("verb %q carries a default: %+v", name, spec)
		}
	}
}

// TestEffectiveTraits pins that the protocol adds no default: a command gets
// the traits it declares, or the zero traits.
func TestEffectiveTraits(t *testing.T) {
	declared := &CommandTraits{RequiresRunnable: true}
	// Pipeline step names resolve through the base command, and a declared
	// object comes back as declared.
	if got, want := EffectiveTraits("serve~dev", declared), *declared; got != want {
		t.Errorf("EffectiveTraits(serve~dev, %+v) = %+v, want %+v", *declared, got, want)
	}
	// A declaration replaces everything, side effects included.
	for _, side := range []string{SideEffectsNone, SideEffectsRegistry, SideEffectsCloud} {
		traits := &CommandTraits{Heavy: true, SideEffects: side}
		if got := EffectiveTraits("publish", traits); got != *traits {
			t.Errorf("EffectiveTraits(publish, %+v) = %+v, want the declaration", *traits, got)
		}
	}
	// A command that declares none gets the zero traits, whatever its name.
	for _, name := range []string{"build", "publish", "deploy~apply", "serve", "frobnicate"} {
		if got := EffectiveTraits(name, nil); got != (CommandTraits{}) {
			t.Errorf("EffectiveTraits(%q, nil) = %+v, want zero", name, got)
		}
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
