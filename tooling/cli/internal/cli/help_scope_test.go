package cli

import (
	"slices"
	"testing"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/features/spectest"
	runner "go.putnami.dev/protocol/runner"
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
