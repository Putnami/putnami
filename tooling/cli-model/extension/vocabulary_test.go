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

	if want := []string{"test", "build", "check"}; !reflect.DeepEqual(got.VerbNames(), want) {
		t.Fatalf("verbs = %v, want %v", got.VerbNames(), want)
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
	spectest.Proves(t, "cli/job-model", "extension-verb-vocabulary", "two-vocabularies-declaring-one-verb-warn")
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

// TestApplyVocabularyTraits_AVocabularyGivesItsVerbsSideEffects pins that the
// core keeps no side-effect floor: a job without declared traits takes its
// verb's default traits whole, side effects included, and a vocabulary that
// gives publish none leaves it with none.
func TestApplyVocabularyTraits_AVocabularyGivesItsVerbsSideEffects(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-model", "vocabulary-default-traits", "traits-alone-give-a-job-its-side-effects")
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
		"publish": harmless,
		"deploy":  cloud,
		"package": cloud,
	}
	for name, want := range cases {
		if got := jobs.Jobs[name].Traits; got != want {
			t.Errorf("%s traits = %+v, want %+v", name, got, want)
		}
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %q, want none", warnings)
	}
}

// TestResolve_DeclaredTraitsReplaceTheVerbsSideEffects pins the resolution
// every gate reads: declared traits are the job's traits whole, so a command
// that declares no side effect has none, whatever its name or its verb's
// default, and a vocabulary does not reach a job whose manifest declared
// traits.
func TestResolve_DeclaredTraitsReplaceTheVerbsSideEffects(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-model", "vocabulary-default-traits", "traits-alone-give-a-job-its-side-effects")
	channels := proto.CommandTraits{InjectPublishChannels: true}
	heavy := proto.CommandTraits{Heavy: true}
	jobs := Resolve(&Manifest{Name: "@acme/release", Commands: map[string]CommandDefinition{
		"publish": {Traits: &channels},
		"deploy":  {Traits: &heavy},
		"release": {Traits: &channels},
	}}, "/ext")
	dev := vocabularyExtension("@acme/dev",
		VerbDefinition{Name: "release", Description: "Release.", Traits: &proto.CommandTraits{SideEffects: proto.SideEffectsRegistry}},
		VerbDefinition{Name: "deploy", Description: "Deploy.", Traits: &proto.CommandTraits{SideEffects: proto.SideEffectsCloud}},
	)
	ApplyVocabularyTraits([]*ExtensionDescription{jobs, dev})

	cases := map[string]CommandTraits{
		"publish": channels,
		"deploy":  heavy,
		"release": channels,
	}
	for name, want := range cases {
		if got := jobs.Jobs[name].Traits; got != want {
			t.Errorf("%s traits = %+v, want %+v", name, got, want)
		}
	}
}

// A job whose manifest declares no traits takes its verb's whole default
// traits, kind and workload included.
func TestApplyVocabularyTraits_AJobWithoutDeclaredTraitsTakesItsVerbsKindAndWorkload(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-model", "command-kinds-and-traits", "a-job-without-declared-traits-takes-its-verbs-kind-and-workload")
	jobs := Resolve(&Manifest{Name: "@acme/go", Commands: map[string]CommandDefinition{
		"check": {}, "serve": {}, "release": {}, "publish": {}, "bundle": {},
	}}, "/ext")
	check := proto.CommandTraits{Kind: proto.CommandKindVerification}
	serve := proto.CommandTraits{RequiresRunnable: true, Workload: proto.WorkloadServe}
	release := proto.CommandTraits{SideEffects: proto.SideEffectsCloud, ConsumesReleaseSet: true}
	lowered := proto.CommandTraits{InjectPublishChannels: true, Kind: proto.CommandKindFiles}
	dev := vocabularyExtension("@acme/dev",
		VerbDefinition{Name: "check", Description: "Check.", Traits: &check},
		VerbDefinition{Name: "serve", Description: "Serve.", Traits: &serve},
		VerbDefinition{Name: "release", Description: "Release.", Traits: &release},
		VerbDefinition{Name: "publish", Description: "Publish.", Traits: &lowered},
		VerbDefinition{Name: "bundle", Description: "Bundle.", Traits: &proto.CommandTraits{RewritesSources: true}},
	)
	ApplyVocabularyTraits([]*ExtensionDescription{jobs, dev})

	cases := map[string]CommandTraits{
		"check":   check,
		"serve":   serve,
		"release": release,
		"publish": lowered,
		"bundle":  {RewritesSources: true},
	}
	for name, want := range cases {
		if got := jobs.Jobs[name].Traits; got != want {
			t.Errorf("%s traits = %+v, want %+v", name, got, want)
		}
	}
}

// Declared traits replace the verb's defaults wholesale: a command that
// declares traits without a kind or a workload has none, whatever its verb
// declares, and a command that declares its own keeps it.
func TestResolve_DeclaredTraitsReplaceTheVerbsKindWholesale(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-model", "command-kinds-and-traits", "declared-traits-replace-the-verbs-kind-wholesale")
	runnable := proto.CommandTraits{RequiresRunnable: true}
	files := proto.CommandTraits{Heavy: true, Kind: proto.CommandKindFiles}
	jobs := Resolve(&Manifest{Name: "@acme/go", Commands: map[string]CommandDefinition{
		"check": {Traits: &proto.CommandTraits{Heavy: true}},
		"serve": {Traits: &runnable},
		"build": {Traits: &files},
	}}, "/ext")
	dev := vocabularyExtension("@acme/dev",
		VerbDefinition{Name: "check", Description: "Check.", Traits: &proto.CommandTraits{Kind: proto.CommandKindCriteria}},
		VerbDefinition{Name: "serve", Description: "Serve.", Traits: &proto.CommandTraits{RequiresRunnable: true, Workload: proto.WorkloadServe}},
		VerbDefinition{Name: "build", Description: "Build.", Traits: &proto.CommandTraits{Kind: proto.CommandKindVerification}},
	)
	ApplyVocabularyTraits([]*ExtensionDescription{jobs, dev})

	cases := map[string]CommandTraits{
		"check": {Heavy: true},
		"serve": runnable,
		"build": files,
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
	if len(got.Verbs) != 0 || got.AliasMap() != nil || got.VerbNames() != nil {
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
		"bundle":  cloud,         // the vocabulary's default
		"check":   {},            // no verb, no default
		"publish": {Heavy: true}, // declared traits win whole
		"serve":   {},            // a verb without traits gives none
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
	if got := jobs.Jobs["publish"].Traits; got != (CommandTraits{}) {
		t.Fatalf("publish traits = %+v, want none: the core gives a verb no trait of its own", got)
	}
}

func TestResolve_CopiesTheVocabulary(t *testing.T) {
	t.Parallel()
	verbs := []VerbDefinition{{Name: "build", Description: "Build."}, {Name: "test", Description: "Test."}}
	if got := Resolve(&Manifest{Name: "@acme/dev", Verbs: verbs}, "/ext").Verbs; !reflect.DeepEqual(got, verbs) {
		t.Fatalf("verbs = %v, want %v", got, verbs)
	}
}
