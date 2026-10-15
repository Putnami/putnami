package extension

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

// vocabularyManifest declares bundle and publish in a vocabulary, at contract.
func vocabularyManifest(contract int) string {
	return fmt.Sprintf(`{
		"name": "@acme/dev",
		"cliContract": %d,
		"verbs": [
			{"name": "bundle", "description": "Bundle the project.", "traits": {"requiresRunnable": true, "sideEffects": "registry"}},
			{"name": "publish", "description": "Push artifacts."}
		]
	}`, contract)
}

const vocabularyLanguageManifest = `{
	"name": "@acme/lang",
	"cliContract": 4,
	"commands": {
		"bundle": {"run": [{"id": "b", "task": "t"}]},
		"bundle-fast": {"run": [{"id": "b", "task": "t"}], "traits": {"heavy": true}},
		"publish": {"run": [{"id": "p", "task": "t"}]}
	},
	"tasks": {"t": {"kind": "command", "command": "echo"}}
}`

func TestDiscovery_AJobWithoutTraitsTakesItsVerbsDefaultTraits(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "vocabulary-default-traits", "an-undeclared-job-takes-its-verbs-default-traits")
	dir := t.TempDir()
	writeExtensionManifest(t, filepath.Join(dir, "dev"), vocabularyManifest(protocolcli.VerbVocabularyContract))
	writeExtensionManifest(t, filepath.Join(dir, "lang"), vocabularyLanguageManifest)

	result, err := DiscoverExtensionsDetailed(dir, &wsproto.Config{}, []string{"dev", "lang"})
	if err != nil || len(result.Skipped) != 0 || len(result.Extensions) != 2 {
		t.Fatalf("discovery = %+v, %v", result, err)
	}
	lang := FindExtensionByName(result.Extensions, "@acme/lang")
	if lang == nil {
		t.Fatal("@acme/lang did not load")
	}
	want := map[string]CommandTraits{
		"bundle":      {RequiresRunnable: true, SideEffects: SideEffectsRegistry},
		"bundle-fast": {Heavy: true},
		"publish":     {InjectPublishChannels: true, SideEffects: SideEffectsRegistry},
	}
	for name, traits := range want {
		if got := lang.Jobs[name].Traits; got != traits {
			t.Errorf("%s traits = %+v, want %+v", name, got, traits)
		}
	}
}

func TestDiscovery_AVocabularyStampedBelowContract9DoesNotLoad(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "vocabulary-default-traits", "a-vocabulary-requires-contract-9")
	dir := t.TempDir()
	writeExtensionManifest(t, filepath.Join(dir, "dev"), vocabularyManifest(protocolcli.ExtensionDependenciesContract))
	writeExtensionManifest(t, filepath.Join(dir, "lang"), vocabularyLanguageManifest)

	result, err := DiscoverExtensionsDetailed(dir, &wsproto.Config{}, []string{"dev", "lang"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skipped) != 1 || !strings.Contains(result.Skipped[0].Reason.Error(), "requires 9 for its verb vocabulary") {
		t.Fatalf("skipped = %v, want the vocabulary refused for its contract", result.Skipped)
	}
	lang := FindExtensionByName(result.Extensions, "@acme/lang")
	if lang == nil || lang.Jobs["bundle"].Traits != (CommandTraits{}) {
		t.Fatal("a vocabulary that did not load gives no default traits")
	}
}
