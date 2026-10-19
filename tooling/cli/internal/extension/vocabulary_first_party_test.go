package extension

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

// firstPartyVocabularyProjects are the first-party extensions whose jobs a
// development vocabulary gives meaning to, by workspace path.
var firstPartyVocabularyProjects = []string{
	filepath.Join("go", "extension"),
	filepath.Join("typescript", "extension"),
	filepath.Join("python", "extension"),
	filepath.Join("cloud", "extension"),
	filepath.Join("tooling", "scaffold"),
}

// firstPartyKindProjects are the first-party extensions whose commands take a
// kind, a workload or an effect from the development vocabulary or declare
// their own: the language and Cloud extensions, and the two that declare a
// validate command.
var firstPartyKindProjects = append(slices.Clone(firstPartyVocabularyProjects),
	filepath.Join("tooling", "sdd-extension"),
	filepath.Join("tooling", "clientgen-extension"),
)

// discoverFirstPartyJobTraits copies the real first-party manifests, and
// vocabulary when it is not empty, into a workspace, and returns the traits
// discovery gives each job, keyed by extension and job name. The workspace
// declares vocabulary by path, which satisfies the dependency the language
// extensions name on @putnami/dev.
func discoverFirstPartyJobTraits(t *testing.T, repoRoot, vocabulary string) map[string]CommandTraits {
	t.Helper()
	return discoverJobTraits(t, repoRoot, firstPartyVocabularyProjects, vocabulary)
}

// discoverJobTraits is discoverFirstPartyJobTraits over the extensions at
// projects.
func discoverJobTraits(t *testing.T, repoRoot string, projects []string, vocabulary string) map[string]CommandTraits {
	t.Helper()
	dir := t.TempDir()
	paths := slices.Clone(projects)
	for _, rel := range projects {
		for _, name := range []string{ManifestFilename, "putnami.json"} {
			data, err := os.ReadFile(filepath.Join(repoRoot, rel, name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, rel), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, rel, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	cfg := &wsproto.Config{}
	if vocabulary != "" {
		writeExtensionManifest(t, filepath.Join(dir, "dev"), vocabulary)
		paths = append(paths, "dev")
		cfg.Extensions = wsproto.ExtensionsConfig{List: map[string]string{"/dev": ""}}
	}
	result, err := DiscoverExtensionsDetailed(dir, cfg, paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Extensions) != len(paths) {
		t.Fatalf("loaded %d extensions, want %d (skipped %v)", len(result.Extensions), len(paths), result.Skipped)
	}
	traits := make(map[string]CommandTraits)
	for _, ext := range result.Extensions {
		for name, job := range ext.Jobs {
			traits[ext.Name+" "+name] = job.Traits
		}
	}
	return traits
}

// devVocabulary is a vocabulary that declares every verb the first-party jobs
// use, in the shape the development-tooling extension will ship: without
// traits, or with the well-known verb's traits spelled out.
func devVocabulary(t *testing.T, jobs map[string]CommandTraits, withTraits bool) string {
	t.Helper()
	verbFormat := regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	names := make([]string, 0, len(jobs)+len(proto.WellKnownVerbs))
	for key := range jobs {
		_, job, _ := strings.Cut(key, " ")
		name := proto.BaseCommandName(job)
		if verbFormat.MatchString(name) && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	for name := range proto.WellKnownVerbs {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	verbs := make([]proto.VerbDefinition, 0, len(names))
	for _, name := range names {
		verb := proto.VerbDefinition{Name: name, Description: "The " + name + " verb."}
		if withTraits {
			traits := proto.WellKnownVerbs[name].Traits
			verb.Traits = &traits
		}
		verbs = append(verbs, verb)
	}
	type vocabularyManifest struct {
		Name        string                 `json:"name"`
		CLIContract int                    `json:"cliContract"`
		Verbs       []proto.VerbDefinition `json:"verbs"`
	}
	data, err := json.Marshal(vocabularyManifest{Name: "@putnami/dev", CLIContract: 9, Verbs: verbs})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// coreTraits keeps the traits the core reads today: heavy, requiresRunnable,
// injectPublishChannels and sideEffects. A kind, a workload or an effect that
// contract 11 adds is left out.
func coreTraits(traits CommandTraits) CommandTraits {
	return CommandTraits{
		Heavy:                 traits.Heavy,
		RequiresRunnable:      traits.RequiresRunnable,
		InjectPublishChannels: traits.InjectPublishChannels,
		SideEffects:           traits.SideEffects,
	}
}

// kindTraits keeps the traits contract 11 adds: the kind, the workload,
// rewritesSources and consumesReleaseSet.
func kindTraits(traits CommandTraits) CommandTraits {
	return CommandTraits{
		Kind:               traits.Kind,
		Workload:           traits.Workload,
		RewritesSources:    traits.RewritesSources,
		ConsumesReleaseSet: traits.ConsumesReleaseSet,
	}
}

// A development vocabulary that declares the verbs of the first-party jobs,
// without traits or with the well-known ones, changes the traits of none of
// them: every job keeps the traits Resolve gives it without a vocabulary. The
// vocabulary the repository ships in tooling/dev changes none of the traits
// the core reads either; the kinds it adds are pinned by
// TestTheDevVocabularyDeclaresEveryFirstPartyKind.
func TestFirstPartyJobsKeepTheirTraitsUnderADevelopmentVocabulary(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "vocabulary-default-traits", "first-party-jobs-keep-their-traits")
	repoRoot := findRepoRoot(t)
	baseline := discoverFirstPartyJobTraits(t, repoRoot, "")

	// Control: the baseline is not vacuous, and it carries the traits a
	// vocabulary could change.
	withEffects := 0
	for _, traits := range baseline {
		if traits != (CommandTraits{}) {
			withEffects++
		}
	}
	if len(baseline) < 20 || withEffects == 0 {
		t.Fatalf("baseline has %d jobs, %d with traits; the first-party manifests did not load", len(baseline), withEffects)
	}

	shipped, err := os.ReadFile(filepath.Join(repoRoot, "tooling", "dev", ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		vocabulary string
		compare    func(CommandTraits) CommandTraits
	}{
		"without traits":             {devVocabulary(t, baseline, false), func(traits CommandTraits) CommandTraits { return traits }},
		"with the well-known traits": {devVocabulary(t, baseline, true), func(traits CommandTraits) CommandTraits { return traits }},
		"the shipped vocabulary":     {string(shipped), coreTraits},
	} {
		got := discoverFirstPartyJobTraits(t, repoRoot, test.vocabulary)
		for job, want := range baseline {
			if test.compare(got[job]) != test.compare(want) {
				t.Errorf("%s: %s traits = %+v, want %+v", name, job, got[job], want)
			}
		}
		if len(got) != len(baseline) {
			t.Errorf("%s: %d jobs, want %d", name, len(got), len(baseline))
		}
	}
}

// selfDeclaredKinds are the kinds, workloads and effects first-party commands
// declare themselves, so a workspace without @putnami/dev keeps them: serve and
// run declare their workload in every language extension, and Cloud and sdd
// declare the kind of the commands they own the meaning of.
var selfDeclaredKinds = map[string]CommandTraits{
	"@putnami/cloud test":             {Kind: proto.CommandKindVerification},
	"@putnami/cloud validate":         {Kind: proto.CommandKindCriteria},
	"@putnami/sdd validate":           {Kind: proto.CommandKindCriteria},
	"@putnami/sdd validate-workspace": {Kind: proto.CommandKindCriteria},
	"@putnami/go serve":               {Workload: proto.WorkloadServe},
	"@putnami/go run":                 {Workload: proto.WorkloadOnce},
	"@putnami/typescript serve":       {Workload: proto.WorkloadServe},
	"@putnami/typescript run":         {Workload: proto.WorkloadOnce},
	"@putnami/python serve":           {Workload: proto.WorkloadServe},
	"@putnami/python run":             {Workload: proto.WorkloadOnce},
}

// verbKinds are the kinds, workloads and effects the shipped development
// vocabulary gives each verb.
var verbKinds = map[string]CommandTraits{
	"build":    {Kind: proto.CommandKindFiles},
	"test":     {Kind: proto.CommandKindVerification},
	"lint":     {Kind: proto.CommandKindDiagnostics},
	"validate": {Kind: proto.CommandKindCriteria},
	"format":   {RewritesSources: true},
	"serve":    {Workload: proto.WorkloadServe},
	"run":      {Workload: proto.WorkloadOnce},
	"deploy":   {ConsumesReleaseSet: true},
}

// The shipped development vocabulary declares the kind, workload or effect of
// every verb that has one, and every first-party job ends with the one its
// verb names: from the vocabulary when its command declares no traits, from
// its own declaration otherwise. Without the vocabulary, a job keeps only what
// its command declares.
func TestTheDevVocabularyDeclaresEveryFirstPartyKind(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "command-kinds-and-traits", "the-dev-vocabulary-declares-every-first-party-kind")
	repoRoot := findRepoRoot(t)
	shipped, err := os.ReadFile(filepath.Join(repoRoot, "tooling", "dev", ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}

	dev, diags := proto.ParseManifest(shipped)
	if dev == nil || len(diags) != 0 {
		t.Fatalf("the shipped vocabulary does not parse: %v", diags)
	}
	declared := make(map[string]CommandTraits)
	for _, verb := range dev.Verbs {
		if verb.Traits != nil && kindTraits(*verb.Traits) != (CommandTraits{}) {
			declared[verb.Name] = kindTraits(*verb.Traits)
		}
	}
	if !reflect.DeepEqual(declared, verbKinds) {
		t.Fatalf("the shipped vocabulary declares %+v, want %+v", declared, verbKinds)
	}

	withVocabulary := discoverJobTraits(t, repoRoot, firstPartyKindProjects, string(shipped))
	seen := make(map[string]bool)
	for job, traits := range withVocabulary {
		_, name, _ := strings.Cut(job, " ")
		want, own := selfDeclaredKinds[job]
		if !own {
			want = verbKinds[proto.BaseCommandName(name)]
		}
		if got := kindTraits(traits); got != want {
			t.Errorf("with @putnami/dev, %s declares %+v, want %+v", job, got, want)
		}
		if want != (CommandTraits{}) {
			seen[proto.BaseCommandName(name)] = true
		}
	}
	// Control: the first-party jobs exercise every kind and workload, so the
	// comparison above is not vacuous.
	for _, verb := range []string{"build", "test", "lint", "validate", "serve", "run"} {
		if !seen[verb] {
			t.Errorf("no first-party job of the verb %q was discovered", verb)
		}
	}

	without := discoverJobTraits(t, repoRoot, firstPartyKindProjects, "")
	for job, traits := range without {
		if got, want := kindTraits(traits), selfDeclaredKinds[job]; got != want {
			t.Errorf("without @putnami/dev, %s declares %+v, want %+v", job, got, want)
		}
	}
	for job := range selfDeclaredKinds {
		if _, ok := without[job]; !ok {
			t.Errorf("the first-party job %s was not discovered", job)
		}
	}
}
