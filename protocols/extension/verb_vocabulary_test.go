package extension

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
)

// vocabularyManifest is a vocabulary-only manifest: verbs, and nothing that
// runs.
const vocabularyManifest = `{
  "name": "@acme/verbs",
  "cliContract": %d,
  "verbs": [
    {"name": "build", "description": "Compile the project.", "aliases": ["b"], "related": ["test", "lint"]},
    {"name": "test", "description": "Run the tests.", "aliases": ["t"]},
    {"name": "publish", "description": "Push artifacts.", "traits": {"injectPublishChannels": true, "sideEffects": "registry"}}
  ]
}`

func TestVerbs_AVocabularyOnlyManifestValidatesAndRequiresContract9(t *testing.T) {
	t.Parallel()
	m, diags := ParseManifest(fmt.Appendf(nil, vocabularyManifest, protocolcli.VerbVocabularyContract))
	if diag.HasErrors(diags) {
		t.Fatal(diags)
	}
	if diags := FullValidateManifest(m); diag.HasErrors(diags) {
		t.Fatalf("a vocabulary-only manifest must validate: %v", diags)
	}
	names := make([]string, 0, len(m.Verbs))
	for _, verb := range m.Verbs {
		names = append(names, verb.Name)
	}
	if want := []string{"build", "test", "publish"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("verbs = %v, want the declared order %v", names, want)
	}
	if !DeclaresContractSurface(m) {
		t.Fatal("a vocabulary is contract surface: an unstamped vocabulary-only manifest must not load as hook-only")
	}
	if got := RequiredLoadContract(m); got != protocolcli.VerbVocabularyContract {
		t.Fatalf("RequiredLoadContract = %d, want %d", got, protocolcli.VerbVocabularyContract)
	}
	if got := RequiredCLIContract(m); got != protocolcli.VerbVocabularyContract {
		t.Fatalf("RequiredCLIContract = %d, want %d", got, protocolcli.VerbVocabularyContract)
	}
}

func TestVerbs_LoadRefusesAVocabularyStampedBelowContract9(t *testing.T) {
	t.Parallel()
	data := fmt.Appendf(nil, vocabularyManifest, protocolcli.ExtensionDependenciesContract)
	m, err := NegotiateManifest("putnami.extension.json", data)
	if err == nil || m != nil {
		t.Fatalf("a vocabulary stamped %d loaded", protocolcli.ExtensionDependenciesContract)
	}
	for _, fragment := range []string{"requires 9 for its verb vocabulary", "re-package the extension"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error %q misses %q", err, fragment)
		}
	}
	if _, err := NegotiateManifest("putnami.extension.json", fmt.Appendf(nil, vocabularyManifest, protocolcli.VerbVocabularyContract)); err != nil {
		t.Fatalf("a vocabulary stamped 9 must load: %v", err)
	}
}

func TestVerbs_InvalidFixtureReportsExactCodes(t *testing.T) {
	t.Parallel()
	m := loadFixtureManifest(t, filepath.Join("fixtures/invalid", "verbs-invalid.json"))
	want := []string{
		"invalid-verb-name",
		"required-field",
		"duplicate-verb",
		"duplicate-verb-alias", "duplicate-verb-alias", "invalid-verb-alias",
		"invalid-enum",
		"invalid-verb-related", "invalid-verb-related", "invalid-verb-related",
	}
	if got := diagCodes(FullValidateManifest(m)); !reflect.DeepEqual(got, want) {
		t.Fatalf("codes = %v, want %v", got, want)
	}
}

func TestVerbs_AnAliasNamesNoCommandOrGroupOfTheManifest(t *testing.T) {
	t.Parallel()
	m := &Manifest{
		Commands:      map[string]CommandDefinition{"check": {}},
		CommandGroups: map[string]CommandGroupDefinition{"cloud": {}},
		Verbs:         []VerbDefinition{{Name: "build", Description: "Build.", Aliases: []string{"check", "cloud", "b"}}},
	}
	if got, want := diagCodes(ValidateVerbs(m)), []string{"duplicate-verb-alias", "duplicate-verb-alias"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("codes = %v, want %v", got, want)
	}
}

// While the well-known verb table gives a verb side effects, a vocabulary's
// default traits for that verb keep them: no vocabulary makes publish or
// deploy look harmless.
func TestVerbs_DefaultTraitsKeepAWellKnownVerbsSideEffects(t *testing.T) {
	t.Parallel()
	m := &Manifest{Verbs: []VerbDefinition{
		{Name: "publish", Description: "Publish.", Traits: &CommandTraits{InjectPublishChannels: true}},
		{Name: "deploy", Description: "Deploy.", Traits: &CommandTraits{SideEffects: SideEffectsRegistry}},
		{Name: "release", Description: "Release.", Traits: &CommandTraits{}},
		{Name: "package", Description: "Package.", Traits: &CommandTraits{SideEffects: SideEffectsCloud}},
	}}
	diags := ValidateVerbs(m)
	if got, want := diagCodes(diags), []string{"invalid-verb-traits", "invalid-verb-traits"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("codes = %v, want %v (%v)", got, want, diags)
	}
	if !strings.Contains(diags[0].String(), `"registry"`) || !strings.Contains(diags[1].String(), `"cloud"`) {
		t.Fatalf("diagnostics = %v, want the side effects each verb keeps", diags)
	}
}

// An empty vocabulary names nothing: it fails, and it does not stand in for a
// contribution in a manifest that otherwise declares nothing.
func TestVerbs_AnEmptyVocabularyIsRejected(t *testing.T) {
	t.Parallel()
	m, diags := ParseManifest([]byte(`{"name":"@acme/empty","verbs":[]}`))
	if diag.HasErrors(diags) {
		t.Fatal(diags)
	}
	if !hasCode(diag.Errors(ValidateManifest(m)), "empty-verbs") {
		t.Fatal("an empty vocabulary validated")
	}
}

// Every manifest written before the section keeps its verdict and its stamp.
// The mutation is the control that proves the validator is wired.
func TestVerbs_AdditiveForEveryValidFixture(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("fixtures/valid/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no valid fixture: %v", err)
	}
	for _, path := range files {
		m := loadFixtureManifest(t, path)
		if m.DeclaresVerbs() || ValidateVerbs(m) != nil {
			t.Fatalf("%s: the vocabulary validator spoke about a manifest without the section", path)
		}
		if RequiredLoadContract(m) >= protocolcli.VerbVocabularyContract {
			t.Fatalf("%s: a manifest without a vocabulary requires contract %d", path, RequiredLoadContract(m))
		}
		m.Verbs = []VerbDefinition{{Name: "Build"}}
		if !hasCode(ValidateVerbs(m), "invalid-verb-name") || RequiredLoadContract(m) != protocolcli.VerbVocabularyContract {
			t.Fatalf("%s: control: a defective vocabulary must be reported and require contract 9", path)
		}
	}
}
