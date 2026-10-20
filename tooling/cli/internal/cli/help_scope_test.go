package cli

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/features/spectest"
	registry "go.putnami.dev/protocol/registry"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/extension"
)

func scopeOf(commands ...string) *helpScope {
	scope := &helpScope{commands: map[string]bool{}}
	for _, command := range commands {
		scope.commands[command] = true
	}
	return scope
}

// declaredScope returns the scope of a workspace whose one extension declares
// commands, with the job commands help lists among them.
func declaredScope(commands ...string) *helpScope {
	declared := map[string]string{}
	for _, command := range commands {
		declared[command] = ""
	}
	return helpScopeOf(&extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{
		{Name: "@acme/flow", Commands: declared},
	}}, nil)
}

// groupScope returns the scope of a workspace whose one extension declares
// only the command group name, which carries the command name-run.
func groupScope(name string) *helpScope {
	return helpScopeOf(&extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{{
		Name:     "@acme/flow",
		Commands: map[string]string{name + "-run": ""},
		CommandGroups: map[string]extension.CommandGroupDefinition{name: {
			Description: "Group.",
			Subcommands: map[string]extension.SubcommandDefinition{"run": {Command: name + "-run"}},
		}},
	}}}, nil)
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
	flags := flagNames(declaredScope("build", cache.ProviderCommandName))
	if !slices.Contains(flags, "--cache-trust") {
		t.Error("a cache provider gives --cache-trust meaning, help misses it")
	}
	for _, hidden := range []string{"--where", "--providers"} {
		if slices.Contains(flags, hidden) {
			t.Errorf("help lists %s without its provider", hidden)
		}
	}
	if !slices.Contains(flagNames(declaredScope("build", runner.ProviderCommandName)), "--where") {
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

// The Markdown and man references list the global flags help lists.
func TestReferencesListTheFlagsHelpLists(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "job-flags-follow-job-commands", "the-references-and-completion-list-the-flags-help-lists")
	for _, c := range []struct {
		name      string
		scope     *helpScope
		jobFlags  bool
		providers bool
	}{
		{"outside a workspace", nil, false, false},
		{"a workspace with no extension", scopeOf(), false, false},
		{"an extension that declares a job command", declaredScope("build"), true, false},
		{"an extension that also declares the credential provider", declaredScope("build", registry.CredentialProviderCommand), true, true},
	} {
		var markdown, man strings.Builder
		printHelpMarkdown(&markdown, c.scope)
		printHelpMan(&man, c.scope)
		for format, reference := range map[string]string{"Markdown": markdown.String(), "man": man.String()} {
			if got := strings.Contains(reference, "--max-parallel"); got != c.jobFlags {
				t.Errorf("%s: the %s reference lists --max-parallel = %v, want %v", c.name, format, got, c.jobFlags)
			}
			if got := strings.Contains(reference, "--providers"); got != c.providers {
				t.Errorf("%s: the %s reference lists --providers = %v, want %v", c.name, format, got, c.providers)
			}
			if !strings.Contains(reference, "--credential-fd") {
				t.Errorf("%s: the %s reference must always list --credential-fd", c.name, format)
			}
			if c.jobFlags {
				continue
			}
			// Not even a description names a flag the reference does not list.
			for _, flag := range unlistedJobFlags() {
				if namesFlag(reference, flag) {
					t.Errorf("%s: the %s reference names %s, which it does not list", c.name, format, flag)
				}
			}
		}
	}
}

// unlistedJobFlags returns the global flags that only extension commands read
// and that no core command declares as its own flag.
func unlistedJobFlags() []string {
	own := map[string]bool{}
	for _, command := range commandmeta.Commands() {
		for _, flag := range command.Flags {
			own[flag.Long] = true
		}
	}
	var names []string
	for _, flag := range commandmeta.GlobalFlags() {
		if flag.JobOnly && !own[flag.Long] {
			names = append(names, flag.Long)
		}
	}
	return names
}

// namesFlag reports whether text names flag as a whole word: "--exclude" is
// not named by "--exclude-tag".
func namesFlag(text, flag string) bool {
	return regexp.MustCompile(`(^|[^A-Za-z0-9-])` + regexp.QuoteMeta(flag) + `($|[^A-Za-z0-9-])`).MatchString(text)
}

// jobOnlyFlags returns the global flags that only extension commands read.
func jobOnlyFlags() []string {
	var names []string
	for _, flag := range commandmeta.GlobalFlags() {
		if flag.JobOnly {
			names = append(names, flag.Long)
		}
	}
	return names
}

func TestHelpListsTheJobFlagsOnlyWithAJobCommand(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "job-flags-follow-job-commands", "help-and-completion-hide-the-job-flags-where-no-job-command-is-loaded")
	jobFlags := jobOnlyFlags()
	for _, flag := range []string{"--retry", "--max-parallel", "--impacted-strict", "--plan"} {
		if !slices.Contains(jobFlags, flag) {
			t.Errorf("the catalog does not mark %s as read by job commands only", flag)
		}
	}
	for _, flag := range []string{"--credential-fd", "--no-cache", "--projects", "--dry-run", "--providers", "--verbose", "--output", "--help"} {
		if slices.Contains(jobFlags, flag) {
			t.Errorf("the catalog marks %s as read by job commands only, but a core command or help reads it", flag)
		}
	}

	cases := []struct {
		name    string
		scope   *helpScope
		listsIt bool
	}{
		{"outside a workspace", nil, false},
		{"a workspace with no extension", scopeOf(), false},
		{"an extension that declares only a provider command", declaredScope(cache.ProviderCommandName), false},
		{"an extension that declares a job command", declaredScope("build"), true},
		{"an extension that declares only a command group", groupScope("ship"), true},
		{"an extension that declares only a group a core root shadows", groupScope("projects"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			flags := flagNames(c.scope)
			help := renderHelp(c.scope)
			for _, flag := range commandmeta.GlobalFlags() {
				if !flag.JobOnly {
					continue
				}
				// A provider-bound flag also needs its provider, which none of
				// these workspaces declares.
				want := c.listsIt && len(flag.Providers) == 0
				if got := slices.Contains(flags, flag.Long); got != want {
					t.Errorf("flag listing has %s = %v, want %v", flag.Long, got, want)
				}
			}
			if got := strings.Contains(help, "--max-parallel"); got != c.listsIt {
				t.Errorf("help text lists --max-parallel = %v, want %v", got, c.listsIt)
			}
			for _, flag := range []string{"--credential-fd", "--no-cache", "--projects", "--dry-run", "--output", "--help"} {
				if !slices.Contains(flags, flag) {
					t.Errorf("help must always list %s", flag)
				}
			}
		})
	}

	t.Run("the parser keeps every flag", func(t *testing.T) {
		t.Parallel()
		global, rest, err := parseFlags([]string{"--retry", "2", "--max-parallel", "3", "--impacted-strict"})
		if err != nil || len(rest) != 0 || global.Retry != 2 || global.MaxParallel != 3 || !global.ImpactedStrict {
			t.Errorf("parseFlags = %+v, %v, %v: a flag that only extension commands read stays accepted", global, rest, err)
		}
	})
}
