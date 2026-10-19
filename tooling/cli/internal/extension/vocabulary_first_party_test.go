package extension

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// discoverFirstPartyJobTraits copies the real first-party manifests, and
// vocabulary when it is not empty, into a workspace, and returns the traits
// discovery gives each job, keyed by extension and job name. The workspace
// declares vocabulary by path, which satisfies the dependency the language
// extensions name on @putnami/dev.
func discoverFirstPartyJobTraits(t *testing.T, repoRoot, vocabulary string) map[string]CommandTraits {
	t.Helper()
	dir := t.TempDir()
	paths := slices.Clone(firstPartyVocabularyProjects)
	for _, rel := range firstPartyVocabularyProjects {
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

// A development vocabulary that declares the verbs of the first-party jobs,
// without traits or with the well-known ones, changes the traits of none of
// them: every job keeps the traits Resolve gives it without a vocabulary. The
// vocabulary the repository ships in tooling/dev changes none either.
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
	for name, vocabulary := range map[string]string{
		"without traits":             devVocabulary(t, baseline, false),
		"with the well-known traits": devVocabulary(t, baseline, true),
		"the shipped vocabulary":     string(shipped),
	} {
		got := discoverFirstPartyJobTraits(t, repoRoot, vocabulary)
		for job, want := range baseline {
			if got[job] != want {
				t.Errorf("%s: %s traits = %+v, want %+v", name, job, got[job], want)
			}
		}
		if len(got) != len(baseline) {
			t.Errorf("%s: %d jobs, want %d", name, len(got), len(baseline))
		}
	}
}
