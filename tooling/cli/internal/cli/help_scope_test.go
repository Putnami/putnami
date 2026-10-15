package cli

import (
	"errors"
	"slices"
	"testing"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/features/spectest"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/extension"
)

func scopeOf(commands ...string) *helpScope {
	scope := &helpScope{commands: map[string]bool{}}
	for _, command := range commands {
		scope.commands[command] = true
	}
	return scope
}

func jobNames(scope *helpScope) []string {
	var names []string
	for _, command := range scope.jobCommands() {
		names = append(names, command.Name)
	}
	return names
}

func aliasNames(scope *helpScope) []string {
	var names []string
	for _, alias := range scope.aliases() {
		names = append(names, alias.Name+"="+alias.Command)
	}
	return names
}

func flagNames(scope *helpScope) []string {
	var names []string
	for _, category := range scope.flagCategories() {
		for _, flag := range category.Flags {
			names = append(names, flag.Long)
		}
	}
	return names
}

var providerFlags = []string{"--where", "--providers", "--cache-trust"}

// TestHelpOutsideAWorkspaceListsEverything pins the nil scope, which is also
// the help of a workspace where an extension did not load.
func TestHelpOutsideAWorkspaceListsEverything(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "workspace-scoped-help", "help-lists-what-the-workspace-extensions-provide")
	if newHelpScope("", nil) != nil {
		t.Fatal("outside a workspace the scope must be nil")
	}
	if got := jobNames(nil); !slices.Contains(got, "deploy") || !slices.Contains(got, "validate") {
		t.Errorf("job commands = %v, want every conventional one", got)
	}
	if got := aliasNames(nil); !slices.Contains(got, "d=deploy") {
		t.Errorf("aliases = %v, want every built-in one", got)
	}
	flags := flagNames(nil)
	for _, flag := range providerFlags {
		if !slices.Contains(flags, flag) {
			t.Errorf("help outside a workspace misses %s", flag)
		}
	}
}

func TestHelpInAWorkspaceListsWhatItsExtensionsDeclare(t *testing.T) {
	t.Parallel()
	scope := scopeOf("build", "test", "lint", "validate")
	if got, want := jobNames(scope), []string{"build", "test", "lint", "validate"}; !slices.Equal(got, want) {
		t.Errorf("job commands = %v, want %v", got, want)
	}
	if got, want := aliasNames(scope), []string{"b=build", "t=test", "l=lint"}; !slices.Equal(got, want) {
		t.Errorf("aliases = %v, want %v", got, want)
	}
	flags := flagNames(scope)
	for _, flag := range providerFlags {
		if slices.Contains(flags, flag) {
			t.Errorf("help lists %s, which no loaded extension provides", flag)
		}
	}
	// A hosted launcher reads --credential-fd from the pinned CLI's help
	// wherever it runs (launch.credentialProbe).
	for _, listed := range []string{"--no-cache", "--credential-fd"} {
		if !slices.Contains(flags, listed) {
			t.Errorf("help must always list %s", listed)
		}
	}
}

func TestHelpListsAProviderFlagWhenAnExtensionServesIt(t *testing.T) {
	t.Parallel()
	flags := flagNames(scopeOf("build", cache.ProviderCommandName))
	if !slices.Contains(flags, "--cache-trust") {
		t.Error("a cache provider gives --cache-trust meaning, help misses it")
	}
	for _, hidden := range []string{"--where", "--providers"} {
		if slices.Contains(flags, hidden) {
			t.Errorf("help lists %s without its provider", hidden)
		}
	}
	if !slices.Contains(flagNames(scopeOf(runner.ProviderCommandName)), "--where") {
		t.Error("a runner provider gives --where meaning, help misses it")
	}
}

func TestHelpScopeKeepsBuiltinsAndDropsUndeclaredJobs(t *testing.T) {
	t.Parallel()
	scope := scopeOf()
	if len(scope.jobCommands()) != 0 || len(scope.aliases()) != 0 {
		t.Errorf("a workspace with no extension lists jobs %v and aliases %v", scope.jobCommands(), scope.aliases())
	}
	if !scope.listsJobCommand("context map") {
		t.Error("a built-in command is always listed")
	}
	if scope.listsJobCommand("format") {
		t.Error("an undeclared job command is not listed")
	}
}

// TestHelpScopeHidesNothingWhenAnExtensionDidNotLoad pins the fallback: the
// commands of an extension that did not load are unknown, so help hides none.
func TestHelpScopeHidesNothingWhenAnExtensionDidNotLoad(t *testing.T) {
	t.Parallel()
	loaded := &extension.ExtensionDescription{Commands: map[string]string{"build": "Build"}}
	if scope := helpScopeOf(&extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{loaded}}, nil); !scope.declares("build") || scope.declares("deploy") {
		t.Fatalf("every extension loaded: scope = %v, want build only", scope)
	}
	result := &extension.DiscoveryResult{
		Extensions: []*extension.ExtensionDescription{loaded},
		Skipped:    []extension.SkippedExtension{{Ref: "@acme/deploy", Reason: errors.New("manifest unreadable")}},
	}
	if scope := helpScopeOf(result, nil); !scope.declares("deploy") || !slices.Equal(jobNames(scope), jobNames(nil)) {
		t.Fatalf("an extension did not load: scope = %v, want one that hides nothing", scope)
	}
}

// vocabularyDiscovery is a workspace whose @acme/dev vocabulary declares
// bundle, build and ship, and whose language extension declares bundle, build
// and test.
func vocabularyDiscovery() *extension.DiscoveryResult {
	dev := &extension.ExtensionDescription{Name: "@acme/dev", Verbs: []extension.VerbDefinition{
		{Name: "bundle", Description: "Bundle the project.", Aliases: []string{"x", "b"}, Related: []string{"build", "check", "context map"}},
		{Name: "build", Description: "Compile the project."},
		{Name: "ship", Description: "Ship the project.", Aliases: []string{"S"}},
	}}
	lang := &extension.ExtensionDescription{Name: "@acme/lang", Commands: map[string]string{"bundle": "", "build": "", "test": ""}}
	return &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{lang, dev}}
}

func TestHelpReadsTheLoadedVocabularyFirst(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "extension-verb-vocabulary", "help-lists-the-loaded-vocabulary-first")
	scope := helpScopeOf(vocabularyDiscovery(), nil)
	if got, want := jobNames(scope), []string{"bundle", "build", "test"}; !slices.Equal(got, want) {
		t.Errorf("job commands = %v, want the vocabulary's declared verbs, then the catalog's: %v", got, want)
	}
	// The vocabulary claims b; ship and its alias are not listed, because no
	// loaded extension declares ship.
	if got, want := aliasNames(scope), []string{"x=bundle", "b=bundle", "t=test"}; !slices.Equal(got, want) {
		t.Errorf("aliases = %v, want %v", got, want)
	}
	if scope.listsJobCommand("ship") || scope.listsJobCommand("check") {
		t.Error("a path outside the catalog is not a built-in: help lists it only when an extension declares it")
	}
	if !scope.listsJobCommand("context map") {
		t.Error("a built-in command is always listed")
	}
	if got, want := scope.related("bundle"), []string{"build", "check", "context map"}; !slices.Equal(got, want) {
		t.Errorf("related(bundle) = %v, want the verb's %v", got, want)
	}
	if got := scope.related("build"); got != nil {
		t.Errorf("related(build) = %v: the vocabulary's verb wins whole", got)
	}
	if got, want := scope.related("test"), relatedCommands["test"]; !slices.Equal(got, want) {
		t.Errorf("related(test) = %v, want the catalog's %v", got, want)
	}
	if got := scope.commandAliases(map[string]string{"u": "test"}); got.vocabulary["x"] != "bundle" || got.user["u"] != "test" {
		t.Errorf("alias tables = %+v", got)
	}
}

func TestHelpKeepsTheVocabularyWhenAnExtensionDidNotLoad(t *testing.T) {
	t.Parallel()
	result := vocabularyDiscovery()
	result.Skipped = []extension.SkippedExtension{{Ref: "@acme/deploy", Reason: errors.New("manifest unreadable")}}
	scope := helpScopeOf(result, nil)
	names := jobNames(scope)
	if want := []string{"bundle", "build", "ship", "test"}; len(names) < len(want) || !slices.Equal(names[:len(want)], want) {
		t.Fatalf("job commands = %v, want %v first", names, want)
	}
	if !slices.Contains(names, "deploy") || slices.Contains(names[2:], "build") {
		t.Errorf("job commands = %v, want every catalog verb once", names)
	}
	if got := aliasNames(scope); !slices.Contains(got, "S=ship") || slices.Contains(got, "b=build") {
		t.Errorf("aliases = %v, want the vocabulary's, and no built-in it claims", got)
	}
}

func TestHelpWithoutAVocabularyIsUnchanged(t *testing.T) {
	t.Parallel()
	var nilScope *helpScope
	if aliases := nilScope.commandAliases(nil); aliases.vocabulary != nil {
		t.Errorf("outside a workspace no vocabulary alias applies: %v", aliases.vocabulary)
	}
	if got, want := nilScope.related("deploy"), relatedCommands["deploy"]; !slices.Equal(got, want) {
		t.Errorf("related(deploy) = %v, want the catalog's %v", got, want)
	}
	scope := helpScopeOf(&extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{
		{Name: "@acme/go", Commands: map[string]string{"build": "", "test": ""}},
	}}, nil)
	if got, want := jobNames(scope), []string{"build", "test"}; !slices.Equal(got, want) {
		t.Errorf("job commands = %v, want %v", got, want)
	}
	if got, want := aliasNames(scope), []string{"b=build", "t=test"}; !slices.Equal(got, want) {
		t.Errorf("aliases = %v, want %v", got, want)
	}
}
