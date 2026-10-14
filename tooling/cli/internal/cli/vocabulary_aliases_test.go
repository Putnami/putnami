package cli

import (
	"maps"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

func TestVocabularyAliasesResolveFirstAndNeverShadowACommand(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "extension-verb-vocabulary", "vocabulary-aliases-resolve-first-and-never-shadow-a-command")
	dev := &extension.ExtensionDescription{Name: "@acme/dev", Verbs: []extension.VerbDefinition{
		{Name: "bundle", Description: "Bundle the project.", Aliases: []string{"b", "x", "help", "fmt", "cloud"}},
	}}
	lang := &extension.ExtensionDescription{
		Name:          "@acme/lang",
		Commands:      map[string]string{"bundle": "", "fmt": ""},
		CommandGroups: map[string]extension.CommandGroupDefinition{"cloud": {}},
	}
	vocabulary := commandVocabulary([]*extension.ExtensionDescription{lang, dev}).AliasMap()
	if want := map[string]string{"b": "bundle", "x": "bundle"}; !maps.Equal(vocabulary, want) {
		t.Fatalf("vocabulary aliases = %v, want %v: an alias never takes a core root, a command or a command group", vocabulary, want)
	}

	// The vocabulary comes before the built-in aliases, which come before the
	// workspace's.
	aliases := commandAliases{vocabulary: vocabulary, user: map[string]string{"b": "user", "t": "user", "u": "build"}}
	parsed := parseArgs([]string{"b,t,x,u"}, aliases, nil)
	if parsed.Err != nil {
		t.Fatal(parsed.Err)
	}
	if want := []string{"bundle", "test", "bundle", "build"}; !slices.Equal(parsed.Commands, want) {
		t.Fatalf("commands = %v, want %v", parsed.Commands, want)
	}
	if got := resolveAliasOrSelf("x", aliases); got != "bundle" {
		t.Errorf("resolveAliasOrSelf(x) = %q, want bundle", got)
	}

	// Without a vocabulary, parsing is unchanged.
	if got := ParseArgs([]string{"b,u"}, map[string]string{"u": "test"}, nil).Commands; !slices.Equal(got, []string{"build", "test"}) {
		t.Errorf("commands = %v, want [build test]", got)
	}
}
