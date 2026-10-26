package pkg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// A verb vocabulary is contract surface for the npm packager too: a
// vocabulary-only package is validated and stamped 9, and a defective one
// fails the package job unstamped, the same verdicts as the Go packager.
func TestCopyHookFiles_GatesAndStampsAVocabulary(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "first-party-packagers-stamp-and-gate-the-contract", "the-typescript-extension-refuses-what-the-protocol-refuses")
	spectest.Proves(t, "extension/manifest-contract", "first-party-packagers-stamp-and-gate-the-contract", "the-typescript-extension-stamps-the-rung-its-vocabulary-requires")
	spectest.Proves(t, "typescript/typescript-project-toolchain", "vocabulary-default-traits", "the-npm-packager-validates-and-stamps-a-vocabulary")
	projDir := stageSourceManifest(t, `{
	"name": "@putnami/testverbs",
	"verbs": [{"name": "build", "description": "Compile the project.", "aliases": ["b"]}]
}`)
	outDir := t.TempDir()
	if err := copyHookFiles(projDir, outDir, "1.0.0"); err != nil {
		t.Fatalf("a vocabulary-only manifest must be packaged: %v", err)
	}
	loaded, err := proto.LoadManifest(filepath.Join(outDir, proto.ManifestFilename))
	if err != nil {
		t.Fatalf("the packaged vocabulary must load: %v", err)
	}
	if loaded.CLIContract != protocolcli.VerbVocabularyContract {
		t.Fatalf("cliContract = %d, want %d", loaded.CLIContract, protocolcli.VerbVocabularyContract)
	}

	for name, manifest := range map[string]string{
		"an invalid verb name": `{"name": "@putnami/testverbs", "verbs": [{"name": "Build", "description": "Compile."}]}`,
		"an empty vocabulary":  `{"name": "@putnami/testverbs", "verbs": []}`,
	} {
		projDir := stageSourceManifest(t, manifest)
		outDir := t.TempDir()
		if err := copyHookFiles(projDir, outDir, "1.0.0"); err == nil {
			t.Fatalf("%s: a defective vocabulary must fail packaging", name)
		}
		data, _ := os.ReadFile(filepath.Join(outDir, proto.ManifestFilename))
		if strings.Contains(string(data), "cliContract") {
			t.Fatalf("%s: a refused manifest must not carry the cliContract stamp", name)
		}
	}
}
