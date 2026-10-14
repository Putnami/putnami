package completion

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
)

// writeVocabularyCompletionWorkspace writes a workspace whose @acme/dev
// extension declares only a verb vocabulary, and whose @acme/lang extension
// declares the bundle command it gives meaning to.
func writeVocabularyCompletionWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"putnami.workspace.json":      `{"name": "vocabulary-completion-ws", "includes": ["dev", "lang"]}`,
		"dev/putnami.json":            `{"name": "@acme/dev"}`,
		"dev/putnami.extension.json":  `{"name": "@acme/dev", "cliContract": 9, "verbs": [{"name": "bundle", "description": "Bundle the project.", "aliases": ["x", "b"]}]}`,
		"lang/putnami.json":           `{"name": "@acme/lang"}`,
		"lang/putnami.extension.json": `{"name": "@acme/lang", "cliContract": 4, "commands": {"bundle": {"description": "Lang bundle.", "flags": {"minify": {"type": "boolean", "description": "Minify"}}, "run": [{"id": "b", "task": "t"}]}}, "tasks": {"t": {"kind": "command", "command": "echo", "cache": false}}}`,
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestGatherCompletionContext_OffersTheLoadedVocabulary(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "extension-verb-vocabulary", "completion-offers-the-loaded-vocabulary")
	ctx := gatherCompletionContext(writeVocabularyCompletionWorkspace(t), &wsproto.Config{})

	if !sharedtest.Contains(ctx.commands, "bundle") || ctx.commandDesc["bundle"] != "Bundle the project." {
		t.Fatalf("bundle = %q, want the vocabulary's description (commands %v)", ctx.commandDesc["bundle"], ctx.commands)
	}
	for _, alias := range []string{"x", "b"} {
		if got := ctx.commandDesc[alias]; got != "Alias for bundle" {
			t.Errorf("alias %s = %q, want Alias for bundle: the vocabulary claims it before the built-in aliases", alias, got)
		}
		if !containsStructuredFlag(ctx.structuredFlags[alias], "--minify") {
			t.Errorf("alias %s offers %+v, want the flags of bundle", alias, ctx.structuredFlags[alias])
		}
	}
	if got := ctx.commandDesc["t"]; got != "Alias for test" {
		t.Errorf("alias t = %q, want the built-in alias the vocabulary leaves free", got)
	}

	// A workspace alias of the same name hides a vocabulary alias.
	hidden := gatherCompletionContext(writeVocabularyCompletionWorkspace(t), &wsproto.Config{Aliases: map[string]string{"x": "lint"}})
	if got := hidden.commandDesc["x"]; got == "Alias for bundle" {
		t.Error("alias x is offered as the vocabulary's, but the workspace alias hides it")
	}
}
