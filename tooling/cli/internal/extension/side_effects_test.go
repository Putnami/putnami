package extension

import (
	"slices"
	"testing"
)

// TestCommandSideEffects pins that a command's side effects are the traits its
// loaded jobs declare plus the well-known verb's default: a verb of an
// extension's own is classified by its declaration, and declared traits add
// side effects to a well-known verb but never remove its default.
func TestCommandSideEffects(t *testing.T) {
	t.Parallel()
	job := func(effects string) *JobDefinition {
		return &JobDefinition{Traits: CommandTraits{SideEffects: effects}}
	}
	release := &ExtensionDescription{Name: "@test/release", Jobs: map[string]*JobDefinition{
		"release":  job(SideEffectsRegistry),
		"converge": job(SideEffectsCloud),
		"publish":  {Traits: CommandTraits{InjectPublishChannels: true}},
		"ship":     job(""),
	}}
	mirror := &ExtensionDescription{Name: "@test/mirror", Jobs: map[string]*JobDefinition{
		"ship":    job(SideEffectsRegistry),
		"release": job(SideEffectsCloud),
		"deploy":  job(SideEffectsRegistry),
	}}
	again := &ExtensionDescription{Name: "@test/again", Jobs: map[string]*JobDefinition{
		"publish": job(SideEffectsRegistry),
		"ship":    job(SideEffectsRegistry),
	}}
	loaded := []*ExtensionDescription{nil, release, mirror}
	for _, tc := range []struct {
		name       string
		extensions []*ExtensionDescription
		command    string
		want       []string
	}{
		{name: "a verb of its own declaring registry", extensions: []*ExtensionDescription{release}, command: "release", want: []string{SideEffectsRegistry}},
		{name: "a verb of its own declaring cloud", extensions: loaded, command: "converge", want: []string{SideEffectsCloud}},
		{name: "a step of that verb", extensions: []*ExtensionDescription{release}, command: "release~upload", want: []string{SideEffectsRegistry}},
		{name: "publish declared with no side effect keeps registry", extensions: loaded, command: "publish", want: []string{SideEffectsRegistry}},
		{name: "deploy declared with registry adds it to cloud", extensions: loaded, command: "deploy", want: []string{SideEffectsCloud, SideEffectsRegistry}},
		{name: "every extension that declares the command counts", extensions: loaded, command: "release", want: []string{SideEffectsCloud, SideEffectsRegistry}},
		{name: "one side effect among declarations of none", extensions: loaded, command: "ship", want: []string{SideEffectsRegistry}},
		{name: "publish declaring its own default counts it once", extensions: []*ExtensionDescription{again}, command: "publish", want: []string{SideEffectsRegistry}},
		{name: "two extensions declaring one side effect count it once", extensions: []*ExtensionDescription{mirror, again}, command: "ship", want: []string{SideEffectsRegistry}},
		{name: "publish no extension declares", extensions: []*ExtensionDescription{mirror}, command: "publish", want: []string{SideEffectsRegistry}},
		{name: "deploy with no extension loaded", command: "deploy", want: []string{SideEffectsCloud}},
		{name: "build with no extension loaded", command: "build"},
		{name: "an unknown verb with no extension loaded", command: "release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CommandSideEffects(tc.extensions, tc.command); !slices.Equal(got, tc.want) {
				t.Fatalf("CommandSideEffects(%q) = %q, want %q", tc.command, got, tc.want)
			}
		})
	}
}
