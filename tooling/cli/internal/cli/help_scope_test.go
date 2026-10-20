package cli

import (
	"errors"
	"slices"
	"strings"
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

// TestHelpOutsideAWorkspaceListsNoExtension pins the nil scope: outside a
// workspace no extension is loaded, so help lists no job command, no alias
// and no provider-bound flag, and says how to add an extension.
func TestHelpOutsideAWorkspaceListsNoExtension(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "workspace-scoped-help", "help-outside-a-workspace-says-no-extension-is-loaded")
	if newHelpScope("", nil) != nil {
		t.Fatal("outside a workspace the scope must be nil")
	}
	if got := jobNames(nil); len(got) != 0 {
		t.Errorf("job commands = %v, want none: the core declares no job command", got)
	}
	if got := aliasNames(nil); len(got) != 0 {
		t.Errorf("aliases = %v, want none: the core declares no alias", got)
	}
	flags := flagNames(nil)
	for _, flag := range providerFlags {
		if slices.Contains(flags, flag) {
			t.Errorf("help outside a workspace lists %s, which no loaded extension provides", flag)
		}
	}
	if !slices.Contains(flags, "--credential-fd") {
		t.Error("help must always list --credential-fd")
	}
	text := oneLine(noJobCommandHelp(nil))
	for _, want := range []string{"No extension is loaded.", "putnami.workspace.json", "putnami extensions install"} {
		if !strings.Contains(text, want) {
			t.Errorf("help outside a workspace says %q, want it to contain %q", text, want)
		}
	}
	if oneLine(noJobCommandHelp(scopeOf())) == text {
		t.Error("a workspace whose extensions declare no job command must not say that no extension is loaded")
	}
}

func TestHelpInAWorkspaceListsWhatItsExtensionsDeclare(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "workspace-scoped-help", "help-lists-what-the-workspace-extensions-provide")
	scope := helpScopeOf(&extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{
		{Name: "@acme/go", Commands: map[string]string{"validate": "", "build": "", "test": "", "lint": ""}},
	}}, nil)
	if got, want := jobNames(scope), []string{"build", "lint", "test", "validate"}; !slices.Equal(got, want) {
		t.Errorf("job commands = %v, want %v", got, want)
	}
	if got := aliasNames(scope); len(got) != 0 {
		t.Errorf("aliases = %v, want none without a vocabulary", got)
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

// TestHelpListsOnlyTheCommandsAUserTypes pins what help lists after the
// vocabulary's verbs: a public command by name, with the description of the
// first extension that gives one, and no internal command, provider command,
// command a command group carries, or command a core path or a command group
// shadows.
func TestHelpListsOnlyTheCommandsAUserTypes(t *testing.T) {
	t.Parallel()
	zeta := &extension.ExtensionDescription{
		Name:              "@acme/zeta",
		Commands:          map[string]string{"build": "Zeta build.", "workspace-install": "", cache.ProviderCommandName: "", "zeta-login": "", "zeta-logs": ""},
		CommandVisibility: map[string]string{"workspace-install": "internal"},
		CommandGroups: map[string]extension.CommandGroupDefinition{"zeta": {Subcommands: map[string]extension.SubcommandDefinition{
			"login": {Command: "zeta-login"},
			"ops":   {Subcommands: map[string]extension.SubcommandDefinition{"logs": {Command: "zeta-logs"}}},
		}}},
	}
	alpha := &extension.ExtensionDescription{Name: "@acme/alpha", Commands: map[string]string{"build": "", "check": "Check.", "cli": "Stamp the CLI.", "zeta": "Run the zeta job."}}
	scope := helpScopeOf(&extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{zeta, nil, alpha}}, nil)
	want := []CommandInfo{{Name: "build", Description: "Zeta build"}, {Name: "check", Description: "Check"}}
	if got := scope.jobCommands(); !slices.Equal(got, want) {
		t.Errorf("job commands = %v, want %v", got, want)
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
	if scope := helpScopeOf(result, nil); !scope.declares("deploy") || !slices.Equal(jobNames(scope), []string{"build"}) {
		t.Fatalf("an extension did not load: scope = %v, want one that hides nothing and lists what loaded", scope)
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
		t.Errorf("job commands = %v, want the vocabulary's declared verbs, then the other commands by name: %v", got, want)
	}
	// ship and its alias are not listed, because no loaded extension declares
	// ship, and the core declares no alias of test.
	if got, want := aliasNames(scope), []string{"x=bundle", "b=bundle"}; !slices.Equal(got, want) {
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
	if got := scope.related("test"); got != nil {
		t.Errorf("related(test) = %v: a command no vocabulary names relates to nothing", got)
	}
	if got, want := scope.related("cache verify"), relatedCommands["cache verify"]; len(want) == 0 || !slices.Equal(got, want) {
		t.Errorf("related(cache verify) = %v, want the catalog's %v", got, want)
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
	if got, want := jobNames(scope), []string{"bundle", "build", "ship", "test"}; !slices.Equal(got, want) {
		t.Fatalf("job commands = %v, want the vocabulary's verbs, then the loaded commands: %v", got, want)
	}
	if got, want := aliasNames(scope), []string{"x=bundle", "b=bundle", "S=ship"}; !slices.Equal(got, want) {
		t.Errorf("aliases = %v, want the vocabulary's %v", got, want)
	}
}

func TestHelpWithoutAVocabularyListsNoAlias(t *testing.T) {
	t.Parallel()
	var nilScope *helpScope
	if aliases := nilScope.commandAliases(nil); aliases.vocabulary != nil {
		t.Errorf("outside a workspace no vocabulary alias applies: %v", aliases.vocabulary)
	}
	if got, want := nilScope.related("cache verify"), relatedCommands["cache verify"]; !slices.Equal(got, want) {
		t.Errorf("related(cache verify) = %v, want the catalog's %v", got, want)
	}
	scope := helpScopeOf(&extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{
		{Name: "@acme/go", Commands: map[string]string{"build": "", "test": ""}},
	}}, nil)
	if got, want := jobNames(scope), []string{"build", "test"}; !slices.Equal(got, want) {
		t.Errorf("job commands = %v, want %v", got, want)
	}
	if got := aliasNames(scope); len(got) != 0 {
		t.Errorf("aliases = %v, want none: the core declares no alias", got)
	}
}
