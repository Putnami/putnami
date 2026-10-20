package cli

import (
	"bytes"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

func renderHelp(scope *helpScope) string {
	var out bytes.Buffer
	printHelpTo(&out, scope)
	return out.String()
}

// helpRowCount counts the help lines that start with name as a whole word.
func helpRowCount(help, name string) int {
	count := 0
	for _, line := range strings.Split(help, "\n") {
		if strings.HasPrefix(line, "  ") && strings.HasPrefix(strings.TrimSpace(line), name) {
			rest := strings.TrimPrefix(strings.TrimSpace(line), name)
			if rest == "" || rest[0] == ' ' {
				count++
			}
		}
	}
	return count
}

func groupsScope() *helpScope {
	zeta := &extension.ExtensionDescription{
		Name:     "@acme/zeta",
		Commands: map[string]string{"build": "Build.", "ship": "Flat ship."},
		CommandGroups: map[string]extension.CommandGroupDefinition{
			"zeta":     {Description: "Run zeta.", Subcommands: map[string]extension.SubcommandDefinition{}},
			"ship":     {Description: "Ship the workspace", Subcommands: map[string]extension.SubcommandDefinition{"get": {Command: "ship-get"}}},
			"projects": {Description: "Unreachable projects", Subcommands: map[string]extension.SubcommandDefinition{}},
		},
	}
	alpha := &extension.ExtensionDescription{
		Name: "@acme/alpha",
		CommandGroups: map[string]extension.CommandGroupDefinition{
			"cloud": {Description: "Manage the cloud", Subcommands: map[string]extension.SubcommandDefinition{}},
			"bare":  {Subcommands: map[string]extension.SubcommandDefinition{}},
			"zeta":  {Description: "Alpha zeta.", Subcommands: map[string]extension.SubcommandDefinition{}},
		},
	}
	return helpScopeOf(&extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{zeta, nil, alpha}}, nil)
}

func TestHelpListsTheCommandGroupsOfTheLoadedExtensions(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "help-lists-command-groups", "help-lists-the-command-groups-of-the-loaded-extensions")
	t.Run("section", helpGroupsSection)
	t.Run("once", helpListsAGroupOnce)
	t.Run("core", helpKeepsTheCoreRowsOfAStructuredRoot)
	t.Run("absent", helpWithoutGroups)
}

func helpGroupsSection(t *testing.T) {
	t.Parallel()
	help := renderHelp(groupsScope())

	section := "Commands (from extensions):\n" +
		"  bare\n" +
		"  cloud                        Manage the cloud\n" +
		"  ship                         Ship the workspace\n" +
		"  zeta                         Alpha zeta\n\n"
	if !strings.Contains(help, section) {
		t.Fatalf("help misses the section, sorted by name, in the column format of the categories:\n%s", help)
	}
	if !strings.Contains(help, "-v, --verbose") {
		t.Errorf("the writer holds no global flag line:\n%s", help)
	}
	build, groups, core := strings.Index(help, "Build (from extensions):"), strings.Index(help, section), strings.Index(help, "\nCI:\n")
	if build < 0 || groups < build || core < groups {
		t.Errorf("the section must follow Build (from extensions) and precede the core categories: build %d, groups %d, core %d", build, groups, core)
	}
}

func helpListsAGroupOnce(t *testing.T) {
	t.Parallel()
	help := renderHelp(groupsScope())

	// The extension declares a flat ship command and a group ship, and
	// dispatch sends ship to the group first.
	if got := helpRowCount(help, "ship"); got != 1 {
		t.Errorf("help lists ship %d times, want once, as the group:\n%s", got, help)
	}
	build := help[strings.Index(help, "Build (from extensions):"):strings.Index(help, "Commands (from extensions):")]
	if strings.Contains(build, "ship") {
		t.Errorf("the Build section lists the flat command ship that the group takes over: %q", build)
	}
	if !strings.Contains(build, "build") {
		t.Errorf("the Build section lost build: %q", build)
	}
}

// helpKeepsTheCoreRowsOfAStructuredRoot pins the dispatch order for a core
// command: the core answers projects without loading extensions, so a group
// of that name is unreachable and help neither lists it nor drops the core
// rows.
func helpKeepsTheCoreRowsOfAStructuredRoot(t *testing.T) {
	t.Parallel()
	help := renderHelp(groupsScope())
	if strings.Contains(help, "Unreachable projects") || strings.Contains(commandGroupsOf(help), "projects") {
		t.Errorf("help lists a group named like the core command projects:\n%s", help)
	}
	for _, row := range []string{"projects list", "projects create"} {
		if !strings.Contains(help, row) {
			t.Errorf("help lost the core row %q:\n%s", row, help)
		}
	}
}

// commandGroupsOf returns the text of the groups section of help.
func commandGroupsOf(help string) string {
	_, section, _ := strings.Cut(help, commandGroupsTitle)
	section, _, _ = strings.Cut(section, "\n\n")
	return section
}

func helpWithoutGroups(t *testing.T) {
	t.Parallel()
	outside := renderHelp(nil)
	if strings.Contains(outside, commandGroupsTitle) || !strings.Contains(outside, "No extension is loaded.") {
		t.Errorf("help outside a workspace must keep the no-extension text and have no groups section:\n%s", outside)
	}
	plain := helpScopeOf(&extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{
		{Name: "@acme/go", Commands: map[string]string{"build": ""}},
	}}, nil)
	if strings.Contains(renderHelp(plain), commandGroupsTitle) {
		t.Error("a workspace whose extensions declare no group must have no groups section")
	}
	if !strings.Contains(renderHelp(plain), "projects list") {
		t.Error("without a group named projects, the core rows stay")
	}
}
