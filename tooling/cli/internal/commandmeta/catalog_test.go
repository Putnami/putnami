package commandmeta

import (
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// The catalog is the single command vocabulary (ADR 0001), so its internal
// consistency is what every derived surface inherits. These assertions are the
// ones a hand-maintained table could not make: they fail on the drift that used
// to be invisible until a user hit it.

func TestCatalog_PathsAreUniqueAndRooted(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "command-catalog", "the-catalog-is-unique-rooted-and-well-formed")
	seen := map[string]bool{}
	for _, command := range catalog {
		if command.Path == "" {
			t.Fatal("catalog contains an entry with no Path")
		}
		if seen[command.Path] {
			t.Errorf("catalog declares %q twice", command.Path)
		}
		seen[command.Path] = true
	}
	for _, command := range catalog {
		if command.Root() == command.Path {
			continue
		}
		if !seen[command.Root()] {
			t.Errorf("%q has no catalog entry for its root %q, so the bare invocation carries no metadata",
				command.Path, command.Root())
		}
	}
}

func TestCatalog_ListingRowsAreRenderable(t *testing.T) {
	for _, command := range catalog {
		if command.Category != "" && command.Summary == "" {
			t.Errorf("%q heads a %q listing row with no Summary", command.Path, command.Category)
		}
		if command.Label != "" && command.Category == "" {
			t.Errorf("%q sets Label %q but declares no Category, so the display override is never used",
				command.Path, command.Label)
		}
		if command.Kind != KindJob {
			continue
		}
		if command.Category != "" || command.Description != "" {
			t.Errorf("job command %q must be described by the protocol vocabulary, not by a catalog Category/Description",
				command.Path)
		}
	}
}

func TestCatalog_HelpEntriesAreWellFormed(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "command-catalog", "the-catalog-is-unique-rooted-and-well-formed")
	for _, command := range catalog {
		if command.Description == "" {
			if command.Usage != "" || len(command.Flags) > 0 || len(command.Examples) > 0 {
				t.Errorf("%q carries usage metadata but no Description, so no surface renders it", command.Path)
			}
			continue
		}
		if command.Usage == "" {
			t.Errorf("%q has a Description but no Usage line", command.Path)
			continue
		}
		if !strings.HasPrefix(command.Usage, "putnami ") {
			t.Errorf("%q usage %q does not start with %q", command.Path, command.Usage, "putnami ")
		}
		for _, example := range command.Examples {
			if !strings.HasPrefix(example, "putnami ") {
				t.Errorf("%q example %q does not start with %q", command.Path, example, "putnami ")
			}
		}
	}
}

func TestCatalog_DetailFromResolvesWithoutChains(t *testing.T) {
	for _, command := range catalog {
		if command.DetailFrom == "" {
			continue
		}
		if command.Description != "" || command.Usage != "" || len(command.Flags) > 0 || len(command.Examples) > 0 {
			t.Errorf("%q inherits its help from %q but also declares its own, so one of the two is dead",
				command.Path, command.DetailFrom)
		}
		source, ok := Lookup(command.DetailFrom)
		if !ok {
			t.Errorf("%q inherits its help from %q, which is not a catalog path", command.Path, command.DetailFrom)
			continue
		}
		if source.DetailFrom != "" {
			t.Errorf("%q inherits from %q, which itself inherits from %q — chains make the resolved help order-dependent",
				command.Path, source.Path, source.DetailFrom)
		}
		if source.Description == "" {
			t.Errorf("%q inherits its help from %q, which has none", command.Path, command.DetailFrom)
		}
		if detail, ok := Detail(command.Path); !ok || detail.Description != source.Description {
			t.Errorf("Detail(%q) does not resolve to %q's help", command.Path, command.DetailFrom)
		}
	}
}

// usageOmittedFlags records flags a command accepts but deliberately leaves out
// of its usage line, so the flag↔usage check below cannot rot into a lie.
var usageOmittedFlags = map[string]string{}

func TestCatalog_FlagsAgreeWithUsage(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "command-catalog", "the-catalog-is-unique-rooted-and-well-formed")
	for _, command := range catalog {
		for _, flag := range command.Flags {
			if !strings.HasPrefix(flag.Long, "--") {
				t.Errorf("%q flag %q is not a --long form", command.Path, flag.Long)
			}
			if flag.Short != "" && (!strings.HasPrefix(flag.Short, "-") || strings.HasPrefix(flag.Short, "--")) {
				t.Errorf("%q flag %q has short form %q, which is not a single-dash alias", command.Path, flag.Long, flag.Short)
			}
			if (flag.Type == FlagValue) != (flag.ValueName != "") {
				t.Errorf("%q flag %q: Type and ValueName disagree (%v vs %q) — a value flag needs a metavariable and a boolean must not have one",
					command.Path, flag.Long, flag.Type, flag.ValueName)
			}
			if flag.Description == "" {
				t.Errorf("%q flag %q has no description", command.Path, flag.Long)
			}
			key := command.Path + " " + flag.Long
			if strings.Contains(command.Usage, flag.Long) {
				if _, omitted := usageOmittedFlags[key]; omitted {
					t.Errorf("usageOmittedFlags still records %q but the usage line now spells it — delete the exception", key)
				}
				continue
			}
			if _, omitted := usageOmittedFlags[key]; omitted {
				continue
			}
			t.Errorf("%q declares flag %q, which its usage line does not spell: %q", command.Path, flag.Long, command.Usage)
		}
	}
	for key := range usageOmittedFlags {
		path, flag, _ := strings.Cut(key, " --")
		command, ok := Lookup(path)
		if !ok {
			t.Errorf("usageOmittedFlags records %q, which is not a catalog path — delete the exception", path)
			continue
		}
		found := false
		for _, declared := range command.Flags {
			if declared.Long == "--"+flag {
				found = true
			}
		}
		if !found {
			t.Errorf("usageOmittedFlags records %q, which the command no longer declares — delete the exception", key)
		}
	}
}

func TestCatalog_PositionalsAgreeWithUsage(t *testing.T) {
	for _, command := range catalog {
		for _, positional := range command.Positionals {
			if positional.Name == "" {
				t.Errorf("%q declares a positional with no name", command.Path)
				continue
			}
			if positional.Required {
				if !strings.Contains(command.Usage, "<"+positional.Name+">") {
					t.Errorf("%q declares required positional %q, which its usage line does not spell as <%s>: %q",
						command.Path, positional.Name, positional.Name, command.Usage)
				}
				continue
			}
			if !strings.Contains(command.Usage, "["+positional.Name+"]") &&
				!strings.Contains(command.Usage, "[<"+positional.Name+">]") {
				t.Errorf("%q declares optional positional %q, which its usage line does not spell as [%s]: %q",
					command.Path, positional.Name, positional.Name, command.Usage)
			}
		}
	}
}

func TestCatalog_RelatedCommandsResolve(t *testing.T) {
	for _, command := range catalog {
		for _, related := range command.Related {
			if related == command.Path {
				t.Errorf("%q suggests itself", command.Path)
			}
			if _, ok := Lookup(related); !ok {
				t.Errorf("%q suggests %q, which is not a catalog path", command.Path, related)
			}
		}
	}
}

// TestCatalog_WorkspaceRequirementsAreExplained keeps the workspace data honest:
// the CLI's requirement is deliberately not uniform (machine-global stores,
// ~/.putnami/bin management, workspace bootstrapping), and every departure from
// "needs a workspace" must name the handler behavior it codifies. Agreement with
// the handlers themselves is asserted by internal/cli's
// TestCatalog_WorkspaceRequirementMatchesHandlers.
func TestCatalog_WorkspaceRequirementsAreExplained(t *testing.T) {
	for _, command := range catalog {
		need := command.Workspace
		if need.Required && !need.Conditional() {
			if need.Note != "" {
				t.Errorf("%q needs a workspace unconditionally, so its Note is noise: %q", command.Path, need.Note)
			}
			if len(need.OverrideFlags) > 0 {
				t.Errorf("%q declares OverrideFlags with no ExemptFlags to override", command.Path)
			}
			continue
		}
		if need.Note == "" {
			t.Errorf("%q does not plainly require a workspace and carries no Note explaining why", command.Path)
		}
		for _, flag := range append(append([]string(nil), need.ExemptFlags...), need.OverrideFlags...) {
			if !strings.HasPrefix(flag, "-") {
				t.Errorf("%q workspace exemption names %q, which is not a flag", command.Path, flag)
			}
		}
		if !need.Required && (len(need.ExemptFlags) > 0 || len(need.OverrideFlags) > 0) {
			t.Errorf("%q never requires a workspace, so its exemption flags never fire", command.Path)
		}
	}
}

func TestWorkspaceNeed_RequiresWorkspace(t *testing.T) {
	cases := []struct {
		path string
		args []string
		want bool
	}{
		{"projects list", nil, true},
		{"projects list", []string{"--global"}, true},
		{"init", nil, false},
		{"migrate vnext", nil, false},
		{"cache gc", nil, false},
		{"cache clean", nil, true},
		{"cache clean", []string{"--all"}, false},
		{"version list", nil, true},
		{"version list", []string{"--global"}, false},
		{"version use", []string{"-g"}, false},
		{"version get", []string{"--global"}, true},
		{"upgrade", nil, true},
		{"upgrade", []string{"--global"}, false},
		{"upgrade", []string{"-g"}, false},
		{"upgrade", []string{"--from-source"}, true},
		{"upgrade", []string{"--from-source", "--global"}, true},
		{"upgrade", []string{"--global", "--from-source"}, true},
	}
	for _, testCase := range cases {
		command, ok := Lookup(testCase.path)
		if !ok {
			t.Fatalf("catalog has no entry for %q", testCase.path)
		}
		if got := command.Workspace.RequiresWorkspace(testCase.args); got != testCase.want {
			t.Errorf("%q %v: RequiresWorkspace = %v, want %v", testCase.path, testCase.args, got, testCase.want)
		}
	}
}

func TestCatalog_CategoriesGroupInDeclarationOrder(t *testing.T) {
	categories := Categories()
	if len(categories) == 0 {
		t.Fatal("Categories() returned nothing")
	}
	seen := map[string]bool{}
	rows := 0
	for _, category := range categories {
		if seen[category.Name] {
			t.Errorf("category %q appears twice, so the listing splits it", category.Name)
		}
		seen[category.Name] = true
		if len(category.Commands) == 0 {
			t.Errorf("category %q has no commands", category.Name)
		}
		rows += len(category.Commands)
	}
	declared := 0
	for _, command := range catalog {
		if command.Category != "" {
			declared++
		}
	}
	if rows != declared {
		t.Errorf("Categories() rendered %d rows from %d cataloged rows", rows, declared)
	}
}

func TestCatalog_ReferenceCommandsCoverEveryDocumentedPathOnce(t *testing.T) {
	reference := map[string]bool{}
	for _, command := range ReferenceCommands() {
		if reference[command.Path] {
			t.Errorf("the reference renders %q twice", command.Path)
		}
		reference[command.Path] = true
	}
	var missing []string
	for _, command := range catalog {
		if command.Kind != KindStructured || !command.Documented() {
			continue
		}
		// A path that shares another path's help is documented under the
		// canonical spelling, so the reference must not repeat it.
		if command.DetailFrom != "" {
			if reference[command.Path] {
				t.Errorf("the reference renders %q, which only re-spells %q", command.Path, command.DetailFrom)
			}
			continue
		}
		if !reference[command.Path] {
			missing = append(missing, command.Path)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("documented commands the man/Markdown reference never renders: %v", missing)
	}
}

// TestBuiltinAliasesResolveToCatalogCommands keeps the shortcut table honest
// against the vocabulary it abbreviates: an alias for a command the catalog does
// not know expands to an unknown command at dispatch.
func TestBuiltinAliasesResolveToCatalogCommands(t *testing.T) {
	for _, alias := range BuiltinAliasList() {
		if _, ok := Lookup(alias.Command); !ok {
			t.Errorf("alias %q expands to %q, which is not a catalog path", alias.Name, alias.Command)
		}
	}
}

func TestPublicJobCommands_MatchCatalogOrder(t *testing.T) {
	var want []string
	for _, command := range catalog {
		if command.Kind == KindJob {
			want = append(want, command.Path)
		}
	}
	var got []string
	for _, command := range PublicJobCommands() {
		got = append(got, command.Name)
		if command.Description == "" {
			t.Errorf("job command %q has no description", command.Name)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("PublicJobCommands() = %v, want the catalog's job order %v", got, want)
	}
}

// TestResolveFlags_FollowsDetailFromAndReportsUnknownPaths pins the contract
// slice A1c's parser binds to: a KNOWN path's flag list is authoritative (so an
// undeclared flag is rejected), an alternate spelling inherits the flags of the
// path it documents from, and an UNKNOWN path reports itself as unknown so the
// parser stays lenient rather than rejecting a flag it cannot judge.
func TestResolveFlags_FollowsDetailFromAndReportsUnknownPaths(t *testing.T) {
	source, ok := ResolveFlags("workspace init")
	if !ok || len(source) == 0 {
		t.Fatalf("ResolveFlags(%q) = %v, %v — expected the documented flag list", "workspace init", source, ok)
	}
	alias, ok := ResolveFlags("init")
	if !ok {
		t.Fatal(`ResolveFlags("init") reported the path unknown`)
	}
	if len(alias) != len(source) {
		t.Errorf("init has %d flags, workspace init has %d — DetailFrom must carry the flag surface", len(alias), len(source))
	}

	if flags, ok := ResolveFlags("context pack"); ok {
		t.Errorf("ResolveFlags(%q) = %v, true — an uncataloged path must report unknown so the parser stays lenient", "context pack", flags)
	}

	// A catalog path with no flags is still KNOWN: only globals are accepted.
	if flags, ok := ResolveFlags("projects list"); !ok || len(flags) != 0 {
		t.Errorf("ResolveFlags(%q) = %v, %v — want an empty but known flag surface", "projects list", flags, ok)
	}
}

// TestCatalog_FlagValuesAreClosedSets guards the one catalog field the parser
// ENFORCES rather than merely displays: a Flag.Values list rejects everything
// outside it, so an entry must be a genuinely closed set and must belong to a
// value-taking flag.
func TestCatalog_FlagValuesAreClosedSets(t *testing.T) {
	for _, command := range catalog {
		for _, flag := range command.Flags {
			if len(flag.Values) == 0 {
				continue
			}
			if flag.Type != FlagValue {
				t.Errorf("%q flag %q enumerates values but takes none", command.Path, flag.Long)
			}
			seen := map[string]bool{}
			for _, value := range flag.Values {
				if value == "" {
					t.Errorf("%q flag %q enumerates an empty value", command.Path, flag.Long)
				}
				if seen[value] {
					t.Errorf("%q flag %q enumerates %q twice", command.Path, flag.Long, value)
				}
				seen[value] = true
			}
		}
	}
}
