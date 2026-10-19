package extension

import "testing"

// TestCommandConsumesReleaseSet pins that a command binds a release set when
// one of its loaded jobs carries the consumesReleaseSet trait, whatever its
// name, and that the core gives deploy no such trait of its own.
func TestCommandConsumesReleaseSet(t *testing.T) {
	t.Parallel()
	consumer := &JobDefinition{Traits: CommandTraits{ConsumesReleaseSet: true}}
	cloud := &JobDefinition{Traits: CommandTraits{SideEffects: SideEffectsCloud}}
	rollout := &ExtensionDescription{Name: "@test/rollout", Jobs: map[string]*JobDefinition{
		"rollout": consumer,
		"deploy":  cloud,
		"ship":    nil,
	}}
	platform := &ExtensionDescription{Name: "@test/platform", Jobs: map[string]*JobDefinition{
		"deploy": consumer,
	}}
	for _, tc := range []struct {
		name       string
		extensions []*ExtensionDescription
		command    string
		want       bool
	}{
		{name: "a command of any name that declares the trait", extensions: []*ExtensionDescription{rollout}, command: "rollout", want: true},
		{name: "a step of that command", extensions: []*ExtensionDescription{rollout}, command: "rollout~apply", want: true},
		{name: "deploy that declares only a cloud side effect", extensions: []*ExtensionDescription{nil, rollout}, command: "deploy"},
		{name: "deploy one of the extensions that declare it consumes", extensions: []*ExtensionDescription{rollout, platform}, command: "deploy", want: true},
		{name: "a job without a definition", extensions: []*ExtensionDescription{rollout}, command: "ship"},
		{name: "deploy with no extension loaded", command: "deploy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CommandConsumesReleaseSet(tc.extensions, tc.command); got != tc.want {
				t.Fatalf("CommandConsumesReleaseSet(%q) = %v, want %v", tc.command, got, tc.want)
			}
		})
	}
}
