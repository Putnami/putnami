package pkg

import (
	"os"
	"path/filepath"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// vocabularyOnlyManifest declares a verb vocabulary and nothing that runs, the
// shape of a tooling extension that gives the development verbs their meaning.
const vocabularyOnlyManifest = `{
	"name": "@putnami/testverbs",
	"verbs": [
		{"name": "build", "description": "Compile the project.", "aliases": ["b"]},
		{"name": "publish", "description": "Push artifacts.", "traits": {"injectPublishChannels": true, "sideEffects": "registry"}}
	]
}`

// A verb vocabulary is contract surface: the packager validates it strictly
// and stamps contract 9, in both package forms, so the artifact loads.
func TestGateStampsAVocabularyOnlyManifest(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "vocabulary-default-traits", "the-go-packager-stamps-a-vocabulary-only-manifest")
	for name, form := range map[string]packageForm{"put archive": putArchive, "npm package": npmPackage} {
		dir := stageManifest(t, vocabularyOnlyManifest)
		if err := gateAndStampManifestContract(dir, form); err != nil {
			t.Fatalf("%s: a vocabulary-only manifest must be stamped: %v", name, err)
		}
		loaded, err := proto.LoadManifest(filepath.Join(dir, proto.ManifestFilename))
		if err != nil {
			t.Fatalf("%s: the stamped manifest must load: %v", name, err)
		}
		if loaded.CLIContract != protocolcli.VerbVocabularyContract {
			t.Fatalf("%s: cliContract = %d, want %d", name, loaded.CLIContract, protocolcli.VerbVocabularyContract)
		}
	}
}

// A vocabulary next to a registry dependency takes the full validation, not
// the stamp-only arm of a hook-only dependent: a valid one is stamped 9, and
// a defective one fails packaging and leaves the staged manifest as it was.
func TestGateValidatesAVocabularyNextToARegistryDependency(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "vocabulary-default-traits", "the-go-packager-validates-a-vocabulary-next-to-a-dependency")
	valid := `{
	"name": "@putnami/testverbs",
	"extensionDependencies": ["@putnami/go"],
	"verbs": [{"name": "build", "description": "Compile the project."}]
}`
	dir := stageManifest(t, valid)
	if err := gateAndStampManifestContract(dir, putArchive); err != nil {
		t.Fatalf("a valid vocabulary with a dependency must be stamped: %v", err)
	}
	if got := stagedContract(t, dir); got != protocolcli.VerbVocabularyContract {
		t.Fatalf("cliContract = %d, want %d", got, protocolcli.VerbVocabularyContract)
	}

	for name, manifest := range map[string]string{
		"an invalid verb name": `{
	"name": "@putnami/testverbs",
	"extensionDependencies": ["@putnami/go"],
	"verbs": [{"name": "Build", "description": "Compile the project."}]
}`,
		"an empty vocabulary": `{
	"name": "@putnami/testverbs",
	"extensionDependencies": ["@putnami/go"],
	"verbs": []
}`,
	} {
		dir := stageManifest(t, manifest)
		if err := gateAndStampManifestContract(dir, putArchive); err == nil {
			t.Fatalf("%s: a defective vocabulary must fail packaging", name)
		}
		data, err := os.ReadFile(filepath.Join(dir, proto.ManifestFilename))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != manifest {
			t.Fatalf("%s: a refused manifest must stay unstamped, got:\n%s", name, data)
		}
	}
}
