package extension

import (
	"reflect"
	"testing"

	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

func vocabularyExtension(name string, verbs ...VerbDefinition) *ExtensionDescription {
	return &ExtensionDescription{Name: name, Verbs: verbs, Commands: map[string]string{}, Jobs: map[string]*JobDefinition{}}
}

func TestMergeVocabulary_MergesInExtensionNameOrderAndTheFirstDeclarationWins(t *testing.T) {
	t.Parallel()
	zeta := vocabularyExtension("@acme/zeta",
		VerbDefinition{Name: "build", Description: "Zeta build.", Aliases: []string{"z"}},
		VerbDefinition{Name: "check", Description: "Check.", Aliases: []string{"c"}},
	)
	alpha := vocabularyExtension("@acme/alpha",
		VerbDefinition{Name: "test", Description: "Test.", Aliases: []string{"t", "c"}},
		VerbDefinition{Name: "build", Description: "Alpha build.", Aliases: []string{"b"}},
	)
	got := MergeVocabulary([]*ExtensionDescription{zeta, nil, alpha}, nil)

	var names []string
	for _, verb := range got.Verbs {
		names = append(names, verb.Name)
	}
	if want := []string{"test", "build", "check"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("verbs = %v, want %v", names, want)
	}
	if build, _ := got.Verb("build"); build.Description != "Alpha build." {
		t.Fatalf("build = %q, want the first declaration, by extension name", build.Description)
	}
	// The losing declaration of build contributes nothing, and the first claim
	// of an alias wins.
	want := []VerbAlias{
		{Name: "t", Verb: "test", Extension: "@acme/alpha"},
		{Name: "c", Verb: "test", Extension: "@acme/alpha"},
		{Name: "b", Verb: "build", Extension: "@acme/alpha"},
	}
	if !reflect.DeepEqual(got.Aliases, want) {
		t.Fatalf("aliases = %v, want %v", got.Aliases, want)
	}
	if aliases := got.AliasMap(); aliases["c"] != "test" || len(aliases) != 3 {
		t.Fatalf("alias map = %v", aliases)
	}
}

func TestMergeVocabulary_WarnsWhenTwoVocabulariesDeclareOneVerb(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "extension-verb-vocabulary", "two-vocabularies-declaring-one-verb-warn")
	registry := proto.CommandTraits{InjectPublishChannels: true, SideEffects: proto.SideEffectsRegistry}
	zeta := vocabularyExtension("@acme/zeta", VerbDefinition{Name: "publish", Description: "Zeta publish.", Traits: &registry})
	alpha := vocabularyExtension("@acme/alpha",
		VerbDefinition{Name: "publish", Description: "Alpha publish."},
		VerbDefinition{Name: "publish", Description: "Repeated in one vocabulary."},
	)
	got := MergeVocabulary([]*ExtensionDescription{zeta, alpha}, nil)
	want := []string{`the vocabularies of @acme/alpha and @acme/zeta both declare the verb "publish"; the declaration of @acme/alpha applies`}
	if !reflect.DeepEqual(got.Warnings, want) {
		t.Fatalf("warnings = %q, want %q", got.Warnings, want)
	}
	if publish, _ := got.Verb("publish"); publish.Description != "Alpha publish." {
		t.Fatalf("publish = %q, want the first declaration, by extension name", publish.Description)
	}
}

func TestMergeVocabulary_AVocabularyNeverChangesTheSideEffectsOfAWellKnownVerb(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "vocabulary-default-traits", "a-vocabulary-never-changes-a-well-known-verbs-side-effects")
	manifest := &Manifest{Name: "@acme/go", Commands: map[string]CommandDefinition{"deploy": {}, "publish": {}, "package": {}}}
	jobs := Resolve(manifest, "/ext")
	harmless := proto.CommandTraits{InjectPublishChannels: true}
	cloud := proto.CommandTraits{SideEffects: proto.SideEffectsCloud}
	dev := vocabularyExtension("@acme/dev",
		VerbDefinition{Name: "publish", Description: "Publish.", Traits: &harmless},
		VerbDefinition{Name: "deploy", Description: "Deploy.", Traits: &cloud},
		VerbDefinition{Name: "package", Description: "Package.", Traits: &cloud},
	)
	warnings := ApplyVocabularyTraits([]*ExtensionDescription{jobs, dev})

	cases := map[string]CommandTraits{
		"publish": proto.WellKnownVerbs["publish"].Traits, // the lowered traits are ignored
		"deploy":  cloud,                                  // the same side effects apply
		"package": cloud,                                  // package has no side effects to keep
	}
	for name, want := range cases {
		if got := jobs.Jobs[name].Traits; got != want {
			t.Errorf("%s traits = %+v, want %+v", name, got, want)
		}
	}
	want := []string{`the vocabulary of @acme/dev gives the verb "publish" the side effects "" instead of "registry"; its default traits are ignored`}
	if !reflect.DeepEqual(warnings, want) {
		t.Fatalf("warnings = %q, want %q", warnings, want)
	}
}

// TestResolve_DeclaredTraitsKeepAWellKnownVerbsSideEffects pins the one
// resolution every gate reads: a command whose declared traits name no side
// effects keeps its well-known verb's, and a vocabulary does not reach a job
// whose manifest declared traits.
func TestResolve_DeclaredTraitsKeepAWellKnownVerbsSideEffects(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "vocabulary-default-traits", "declared-traits-keep-a-well-known-verbs-side-effects")
	channels := proto.CommandTraits{InjectPublishChannels: true}
	heavy := proto.CommandTraits{Heavy: true}
	jobs := Resolve(&Manifest{Name: "@acme/release", Commands: map[string]CommandDefinition{
		"publish": {Traits: &channels},
		"deploy":  {Traits: &heavy},
		"release": {Traits: &channels},
	}}, "/ext")
	dev := vocabularyExtension("@acme/dev", VerbDefinition{Name: "release", Description: "Release.",
		Traits: &proto.CommandTraits{SideEffects: proto.SideEffectsRegistry}})
	ApplyVocabularyTraits([]*ExtensionDescription{jobs, dev})

	cases := map[string]CommandTraits{
		"publish": {InjectPublishChannels: true, SideEffects: proto.SideEffectsRegistry},
		"deploy":  {Heavy: true, SideEffects: proto.SideEffectsCloud},
		"release": channels, // no well-known side effects, and the declaration wins over the vocabulary
	}
	for name, want := range cases {
		if got := jobs.Jobs[name].Traits; got != want {
			t.Errorf("%s traits = %+v, want %+v", name, got, want)
		}
	}
}

func TestMergeVocabulary_AnAliasNeverShadowsAVerbCommandGroupOrReservedName(t *testing.T) {
	t.Parallel()
	vocabulary := vocabularyExtension("@acme/dev", VerbDefinition{
		Name: "build", Description: "Build.",
		Aliases: []string{"test", "fmt", "cloud", "help", "b"},
	}, VerbDefinition{Name: "test", Description: "Test."})
	other := &ExtensionDescription{
		Name:          "@acme/other",
		Commands:      map[string]string{"fmt": "Format."},
		CommandGroups: map[string]CommandGroupDefinition{"cloud": {}},
	}
	got := MergeVocabulary([]*ExtensionDescription{vocabulary, other}, func(name string) bool { return name == "help" })
	if want := []VerbAlias{{Name: "b", Verb: "build", Extension: "@acme/dev"}}; !reflect.DeepEqual(got.Aliases, want) {
		t.Fatalf("aliases = %v, want %v", got.Aliases, want)
	}
}

func TestMergeVocabulary_NoVocabulary(t *testing.T) {
	t.Parallel()
	got := MergeVocabulary([]*ExtensionDescription{{Name: "@acme/go", Commands: map[string]string{"build": ""}}}, nil)
	if len(got.Verbs) != 0 || got.AliasMap() != nil {
		t.Fatalf("vocabulary = %+v, want none", got)
	}
	if _, ok := got.Verb("build"); ok {
		t.Fatal("an empty vocabulary declares no verb")
	}
}

func TestApplyVocabularyTraits_DefaultsOnlyUndeclaredTraits(t *testing.T) {
	t.Parallel()
	manifest := &Manifest{
		Name: "@acme/go",
		Commands: map[string]CommandDefinition{
			"bundle":  {},
			"check":   {},
			"publish": {Traits: &CommandTraits{Heavy: true}},
			"serve":   {},
		},
	}
	jobs := Resolve(manifest, "/ext")
	cloud := proto.CommandTraits{SideEffects: proto.SideEffectsCloud}
	dev := vocabularyExtension("@acme/dev",
		VerbDefinition{Name: "bundle", Description: "Bundle.", Traits: &cloud},
		VerbDefinition{Name: "publish", Description: "Publish.", Traits: &cloud},
		VerbDefinition{Name: "serve", Description: "Serve."},
	)
	ApplyVocabularyTraits([]*ExtensionDescription{jobs, dev})

	cases := map[string]CommandTraits{
		"bundle": cloud, // the vocabulary's default
		"check":  {},    // no verb, no default
		// declared traits win, and keep the verb's side effects
		"publish": {Heavy: true, SideEffects: proto.SideEffectsRegistry},
		"serve":   proto.WellKnownVerbs["serve"].Traits, // a verb without traits keeps the fallback
	}
	for name, want := range cases {
		if got := jobs.Jobs[name].Traits; got != want {
			t.Errorf("%s traits = %+v, want %+v", name, got, want)
		}
	}
	if !jobs.Jobs["publish"].TraitsDeclared || jobs.Jobs["bundle"].TraitsDeclared {
		t.Error("TraitsDeclared must record whether the manifest declared traits")
	}
	if got := Resolve(manifest, "/ext").Verbs; got != nil {
		t.Errorf("a manifest without a vocabulary resolves verbs %v", got)
	}
}

func TestApplyVocabularyTraits_WithoutAVocabularyChangesNothing(t *testing.T) {
	t.Parallel()
	jobs := Resolve(&Manifest{Name: "@acme/go", Commands: map[string]CommandDefinition{"publish": {}}}, "/ext")
	ApplyVocabularyTraits([]*ExtensionDescription{jobs, nil})
	if got, want := jobs.Jobs["publish"].Traits, proto.WellKnownVerbs["publish"].Traits; got != want {
		t.Fatalf("publish traits = %+v, want the well-known defaults %+v", got, want)
	}
}

func TestResolve_CopiesTheVocabulary(t *testing.T) {
	t.Parallel()
	verbs := []VerbDefinition{{Name: "build", Description: "Build."}, {Name: "test", Description: "Test."}}
	if got := Resolve(&Manifest{Name: "@acme/dev", Verbs: verbs}, "/ext").Verbs; !reflect.DeepEqual(got, verbs) {
		t.Fatalf("verbs = %v, want %v", got, verbs)
	}
}
