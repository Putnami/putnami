package cli

import (
	"maps"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// aliasingExtensions are a vocabulary whose bundle verb declares aliases that
// collide with a core root, a catalog job verb, an extension command, an
// extension command group and a built-in alias, and the language extension
// that declares the colliding command and group.
func aliasingExtensions() []*extension.ExtensionDescription {
	dev := &extension.ExtensionDescription{Name: "@acme/dev", Verbs: []extension.VerbDefinition{
		{Name: "bundle", Description: "Bundle the project.", Aliases: []string{"b", "x", "help", "deploy", "fmt", "cloud", "w"}},
	}}
	lang := &extension.ExtensionDescription{
		Name:          "@acme/lang",
		Commands:      map[string]string{"bundle": "", "fmt": ""},
		CommandGroups: map[string]extension.CommandGroupDefinition{"cloud": {}},
	}
	return []*extension.ExtensionDescription{lang, dev}
}

func TestVocabularyAliasesResolveFirstAndNeverShadowACommand(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "extension-verb-vocabulary", "vocabulary-aliases-resolve-first-and-never-shadow-a-command")
	vocabulary := commandVocabulary(aliasingExtensions(), nil).AliasMap()
	if want := map[string]string{"b": "bundle", "x": "bundle", "w": "bundle"}; !maps.Equal(vocabulary, want) {
		t.Fatalf("vocabulary aliases = %v, want %v: an alias never takes a catalog command, an extension command or a command group", vocabulary, want)
	}

	// The vocabulary comes before the built-in aliases, which come before the
	// workspace's.
	cfg := &wsproto.Config{Aliases: map[string]string{"t": "user", "u": "build"}}
	aliases, warnings := processAliases(cfg, false, aliasingExtensions())
	if len(warnings) != 0 {
		t.Fatalf("warnings = %q, want none", warnings)
	}
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

func TestAWorkspaceAliasWinsOverAVocabularyAlias(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "extension-verb-vocabulary", "a-workspace-alias-wins-over-a-vocabulary-alias")
	cfg := &wsproto.Config{Aliases: map[string]string{"x": "test", "b": "lint"}}
	aliases, warnings := processAliases(cfg, false, aliasingExtensions())
	want := []string{
		"warning: the workspace alias b (lint) hides the alias b of bundle that @acme/dev declares",
		"warning: the workspace alias x (test) hides the alias x of bundle that @acme/dev declares",
	}
	if !slices.Equal(warnings, want) {
		t.Fatalf("warnings = %q, want %q", warnings, want)
	}
	// x is the workspace's; b keeps its built-in meaning, which a workspace
	// alias never overrides; w stays the vocabulary's.
	parsed := parseArgs([]string{"x,b,w"}, aliases, nil)
	if parsed.Err != nil {
		t.Fatal(parsed.Err)
	}
	if want := []string{"test", "build", "bundle"}; !slices.Equal(parsed.Commands, want) {
		t.Fatalf("commands = %v, want %v", parsed.Commands, want)
	}

	// Help and impact-plan read the same tables.
	scope := helpScopeOf(&extension.DiscoveryResult{Extensions: aliasingExtensions()}, cfg.Aliases)
	if got := scope.commandAliases(cfg.Aliases); !maps.Equal(got.vocabulary, map[string]string{"w": "bundle"}) {
		t.Errorf("help alias tables = %+v, want only the vocabulary alias no workspace alias hides", got)
	}
}

func TestAProviderProcessResolvesNoVocabularyAlias(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "extension-verb-vocabulary", "a-provider-process-resolves-no-vocabulary-alias")
	cfg := &wsproto.Config{Aliases: map[string]string{"u": "test"}}
	aliases, warnings := processAliases(cfg, true, aliasingExtensions())
	if aliases.vocabulary != nil || aliases.user != nil || warnings != nil {
		t.Fatalf("provider aliases = %+v, warnings %q, want neither repository nor vocabulary aliases", aliases, warnings)
	}
	parsed := parseArgs([]string{"x,u,b"}, aliases, nil)
	if parsed.Err != nil {
		t.Fatal(parsed.Err)
	}
	if want := []string{"x", "u", "build"}; !slices.Equal(parsed.Commands, want) {
		t.Fatalf("commands = %v, want %v: only the built-in aliases apply", parsed.Commands, want)
	}
}

func TestImpactPlanResolvesTheVocabularyAliases(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "extension-verb-vocabulary", "impact-plan-resolves-the-vocabulary-aliases")
	user := map[string]string{"S": "lint"}
	scope := helpScopeOf(vocabularyDiscovery(), user)
	got, err := impactPlanCommands("x,t,S", scope.commandAliases(user))
	if err != nil || !slices.Equal(got, []string{"bundle", "test", "lint"}) {
		t.Fatalf("impactPlanCommands = %v, %v; want [bundle test lint]", got, err)
	}
}
