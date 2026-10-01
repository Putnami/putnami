package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The vertical-isolation ratchet.
//
// B0 carved internal/commands/shared and internal/commands/sharedtest out of
// the flat internal/commands package; BG1–BG3 then moved its 129 files, one
// vertical at a time, into internal/commands/<vertical>/ — agentctx,
// cachecmd, ci, completion, configcmd, doctor, extensions, lifecycle,
// migrate, sessions, versioncmd (composecmd, qualifycmd and treecmd came
// later). The plan assumed near-zero sharing
// between them, and R2 (a symbol 2+ verticals need moves to shared, never to
// a sibling vertical) held at every step because the compiler forced the
// question each time a file moved. Nothing forces it on a LATER change,
// though — a new import from one vertical straight into another compiles
// fine, and that is exactly how the flat package grew into 129 files in the
// first place: not in one commit, but one "just this once" edge at a time.
// This test makes the property structural instead of relying on review to
// catch every future edge.
//
// # Why AST, not go list or packages.Load
//
// Same reasoning as A5's model_decoupling_ratchet_test.go and A6a's
// engine_boundary_test.go: this package already reads its own module's
// source with go/ast for exactly this class of assertion, so a `go list`
// subprocess or a type-checking loader (golang.org/x/tools/go/packages)
// would add a dependency this test does not need. Reading import
// declarations off the AST is exact — a doc comment or a string literal
// naming a sibling vertical does not count — reusing importedAs, the same
// primitive the model-decoupling ratchet reads import lines with.
//
// # Why test files are IN SCOPE here, unlike A5
//
// model_decoupling_ratchet_test.go deliberately excludes test files: a
// fixture may reach for either half of a model/execution split without
// weakening the property that ships. That reasoning does not transfer here.
// A _test.go file importing a sibling vertical is exactly as much a
// layering fact as a production file doing it — lifecycle's
// agent_workflows_lifecycle_test.go reassigns agentctx's package-private
// fetch/resolve vars, which only a file outside agentctx could ever need to
// do. Excluding tests would leave this ratchet blind to half the edges the
// exception list below documents, so it scans both.
//
// # The exception list
//
// Measured against the tree as it stood when B13 landed (re-measure if a
// later change disagrees with this comment — the scan below is the source of
// truth, not the prose):
//
//   - lifecycle → agentctx: install, upgrade and workspace-init regenerate
//     the AI context and materialize agent workflows as one of their phases
//     (ContextGenerate/ContextGenerateWithWriter, DeclaredAgentArtifacts,
//     InstallAgentWorkflows, AdoptAgentWorkflows, DescribeAgentWorkflowPlan).
//     lifecycle orchestrates agentctx's setup; it does not reimplement it.
//   - lifecycle → extensions: install, upgrade, workspace-init and project
//     creation install extensions and templates as part of their own flow
//     (ExtensionsInstall*, TemplatesInstall*, ExtensionsUpdateWithOptions,
//     TemplatesUpdateWithOptions).
//   - lifecycle → versioncmd: install and upgrade check and pin the CLI
//     version they are installing or upgrading to (RefreshLockMetadata,
//     VersionUpdateWithOptions, VersionInstallFromSource), and workspace-init
//     records the lock's toolchain pins after its last dependency install
//     (RefreshLockMetadataWithResult).
//   - lifecycle → completion: upgrade refreshes shell completions once the
//     new binary is in place (RefreshShellCompletions).
// The list held one more edge until a later change: agentctx → sdd, for the
// feature design summaries and contract-drift vocabulary the agent-context MCP
// payload and the context pack embedded. It is gone because the sdd vertical is
// gone — the SDD surface ships as @putnami/sdd now — and the symbols followed
// their one remaining caller into agentctx (feature_design_summary.go,
// ContextOutcomeClean/Drift) rather than into shared, because R2's rule is
// about symbols 2+ verticals need and only one needed these.
//
// agentctx → lifecycle does not exist: the graph this table describes is
// acyclic, with lifecycle on top. The other seven verticals (cachecmd, ci,
// completion, configcmd, doctor, extensions, migrate, sessions, versioncmd)
// import no sibling at all — the plan's near-zero-sharing assumption holds for
// all of them.
//
// Every entry below names a FILE, not just an edge: a file that stops
// needing the edge goes stale on its own, and the second loop in
// TestStructuralBaseline_VerticalsStayIsolated deletes it by review rather
// than by someone eventually noticing the list is longer than the truth.

// commandsRoot is internal/commands, the container every vertical and both
// shared/sharedtest live under (see internal/commands/doc.go).
const commandsRoot = "internal/commands"

// commandsDirPrefix is commandsRoot's key-space prefix: this file's scan
// keys every path as internal/commands/<rest>.
const commandsDirPrefix = commandsRoot + "/"

// commandsImportPrefix is the shared root of every vertical's import path;
// appending a vertical's directory name gives its full import path.
const commandsImportPrefix = "go.putnami.dev/tooling/cli/internal/commands/"

// parseCommandsASTs parses every .go file under internal/commands — both
// production and test — keyed by its module-relative slash path
// (internal/commands/<vertical>/file.go, or internal/commands/shared/...).
//
// moduleProductionASTs (A4/A5) is the production-only counterpart, used
// where test fixtures are legitimately out of scope. This scan needs test
// files too (see the doc comment above), and verticals live only in
// tooling/cli's own tree, so it reads that tree directly rather than
// spanning cliModules.
func parseCommandsASTs(t *testing.T, root string) map[string]*ast.File {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(commandsRoot))
	production, test := goSources(t, dir)
	fset := token.NewFileSet()
	files := make(map[string]*ast.File, len(production)+len(test))
	for _, rel := range append(append([]string{}, production...), test...) {
		file, err := parser.ParseFile(fset, filepath.Join(dir, filepath.FromSlash(rel)), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", commandsDirPrefix+rel, err)
		}
		files[commandsDirPrefix+rel] = file
	}
	if len(files) == 0 {
		t.Fatal("parsed no files under internal/commands — the walk root is wrong")
	}
	return files
}

// commandsSubpackage returns the first path segment after
// internal/commands/ (a vertical's directory name, or "shared"/
// "sharedtest"), and whether rel was nested that deep at all. A file
// directly under internal/commands/ (doc.go, surface_golden_test.go)
// reports "", false.
func commandsSubpackage(rel string) (string, bool) {
	rest := strings.TrimPrefix(rel, commandsDirPrefix)
	if rest == rel {
		return "", false
	}
	name, _, nested := strings.Cut(rest, "/")
	return name, nested
}

// commandVerticals returns the vertical package names found in files —
// every internal/commands subdirectory holding at least one .go file, except
// shared and sharedtest, which are landing zones for helpers, not verticals.
//
// It reads the same scan the rest of this test uses rather than a second
// directory walk, so a vertical renamed, merged or split shows up here in
// the same step it shows up in the check below — there is no separate list
// to remember to update.
func commandVerticals(files map[string]*ast.File) []string {
	set := map[string]bool{}
	for rel := range files {
		name, nested := commandsSubpackage(rel)
		if !nested || name == "shared" || name == "sharedtest" {
			continue
		}
		set[name] = true
	}
	verticals := make([]string, 0, len(set))
	for name := range set {
		verticals = append(verticals, name)
	}
	sort.Strings(verticals)
	return verticals
}

// verticalOwner returns the vertical rel belongs to, or false if rel is not
// inside one of verticals (the root package's own files, or shared/
// sharedtest).
func verticalOwner(rel string, verticals []string) (string, bool) {
	name, nested := commandsSubpackage(rel)
	if !nested {
		return "", false
	}
	for _, vertical := range verticals {
		if vertical == name {
			return name, true
		}
	}
	return "", false
}

// verticalCrossImportException is one file that imports a sibling vertical's
// package on purpose, with the reasoning that makes the edge a documented
// layering fact instead of drift back toward the flat package it replaced.
//
// An exception is a written justification, never a silent hole: it names the
// exact file and the exact vertical it reaches into, so the reviewer's
// question is "is that edge really a layering fact?" rather than "why does
// this compile at all?". The list is checked in both directions below — an
// exception that stopped applying fails, so it is deleted by review rather
// than left as a standing permission the next re-coupling falls through.
type verticalCrossImportException struct {
	// file is the file in the internal/commands/<vertical>/... key space.
	file string
	// imports is the OTHER vertical's directory name the file needs.
	imports string
	// why explains what makes the edge real.
	why string
}

// Reasoning shared by every file on the same edge; see the package doc
// comment above for the fuller account of each.
const (
	whyLifecycleAgentctx = "install, upgrade and workspace-init regenerate the AI context and " +
		"materialize agent workflows as one of their phases (ContextGenerate/ContextGenerateWithWriter, " +
		"DeclaredAgentArtifacts, InstallAgentWorkflows, AdoptAgentWorkflows, DescribeAgentWorkflowPlan, " +
		"AgentWorkflowPhaseApplies), and the zero-init ensure pass materializes lock-pinned agent " +
		"workflows beside extensions and templates (EnsureAgentWorkflows) " +
		"— lifecycle orchestrates agentctx's setup, it does not reimplement it"
	whyLifecycleExtensions = "install, upgrade, workspace-init and project creation install extensions " +
		"and templates as part of their own flow (ExtensionsInstall*, TemplatesInstall*, " +
		"ExtensionsUpdateWithOptions, TemplatesUpdateWithOptions)"
	whyLifecycleVersioncmd = "install and upgrade check and pin the CLI version they are installing or " +
		"upgrading to (RefreshLockMetadata, VersionUpdateWithOptions, VersionInstallFromSource), and " +
		"workspace-init records the lock's toolchain pins after its last dependency install " +
		"(RefreshLockMetadataWithResult)"
	whyLifecycleCompletion = "upgrade refreshes shell completions once the new binary is in place " +
		"(RefreshShellCompletions)"
)

var verticalCrossImportExceptions = []verticalCrossImportException{
	{file: "internal/commands/lifecycle/install.go", imports: "agentctx", why: whyLifecycleAgentctx},
	{file: "internal/commands/lifecycle/upgrade.go", imports: "agentctx", why: whyLifecycleAgentctx},
	{file: "internal/commands/lifecycle/workspace_init.go", imports: "agentctx", why: whyLifecycleAgentctx},
	{file: "internal/commands/lifecycle/ensure.go", imports: "agentctx", why: whyLifecycleAgentctx},
	{
		file:    "internal/commands/lifecycle/install_test.go",
		imports: "agentctx",
		why: whyLifecycleAgentctx +
			" — proven here against agentctx's own ClaudeEntrypointPath fixture layout",
	},
	{
		file:    "internal/commands/lifecycle/agent_workflows_lifecycle_test.go",
		imports: "agentctx",
		why: whyLifecycleAgentctx + ". This file stubs package-private vars on BOTH sides " +
			"(lifecycle's own install stages and agentctx's AgentContentMigrationInterrupt " +
			"seam), which only one package can do from inside a _test.go " +
			"file, so it lives in lifecycle and reaches agentctx through its exported seams",
	},
	{file: "internal/commands/lifecycle/install.go", imports: "extensions", why: whyLifecycleExtensions},
	{file: "internal/commands/lifecycle/upgrade.go", imports: "extensions", why: whyLifecycleExtensions},
	{file: "internal/commands/lifecycle/workspace_init.go", imports: "extensions", why: whyLifecycleExtensions},
	{file: "internal/commands/lifecycle/projects_create.go", imports: "extensions", why: whyLifecycleExtensions},
	{file: "internal/commands/lifecycle/install.go", imports: "versioncmd", why: whyLifecycleVersioncmd},
	{file: "internal/commands/lifecycle/upgrade.go", imports: "versioncmd", why: whyLifecycleVersioncmd},
	{file: "internal/commands/lifecycle/workspace_init.go", imports: "versioncmd", why: whyLifecycleVersioncmd},
	{
		file:    "internal/commands/lifecycle/upgrade_test.go",
		imports: "versioncmd",
		why: whyLifecycleVersioncmd +
			" — proven here by stubbing versionUpdateWithOptions' result and error shapes",
	},
	{file: "internal/commands/lifecycle/upgrade.go", imports: "completion", why: whyLifecycleCompletion},
}

// TestStructuralBaseline_VerticalsStayIsolated fails if a vertical
// subpackage of internal/commands imports a sibling vertical's package,
// unless the file is on the exception list above. Allowed imports —
// internal/commands/shared, internal/commands/sharedtest, everything under
// go.putnami.dev/cli/model/, and CLI support packages (internal/jobs,
// internal/workspace, internal/output, …) — are never matched by this scan,
// because none of them is a sibling vertical's import path; they need no
// allowlist of their own.
func TestStructuralBaseline_VerticalsStayIsolated(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	files := parseCommandsASTs(t, root)

	verticals := commandVerticals(files)
	if len(verticals) == 0 {
		t.Fatal("found no vertical subpackages under internal/commands — the layout changed; re-key this test")
	}

	justified := map[string]bool{}
	for _, exception := range verticalCrossImportExceptions {
		justified[exception.file+" "+exception.imports] = true
	}

	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	applied := map[string]bool{}
	for _, rel := range rels {
		owner, ok := verticalOwner(rel, verticals)
		if !ok {
			continue
		}
		file := files[rel]
		for _, other := range verticals {
			if other == owner {
				continue
			}
			importPath := commandsImportPrefix + other
			if _, imported := importedAs(file, importPath, other); !imported {
				continue
			}
			key := rel + " " + other
			if justified[key] {
				applied[key] = true
				continue
			}
			t.Errorf("%s imports %s — a command vertical must not import a sibling vertical directly "+
				"(the vertical-isolation ratchet). Allowed imports are internal/commands/shared, "+
				"go.putnami.dev/cli/model/*, and CLI support packages (internal/jobs, internal/workspace, "+
				"internal/output, …).\n"+
				"  If %s genuinely needs to call into %s's command flow, add a verticalCrossImportException "+
				"naming why. If it just needs a helper, that helper belongs in internal/commands/shared "+
				"instead.",
				rel, importPath, owner, other)
		}
	}

	for _, exception := range verticalCrossImportExceptions {
		key := exception.file + " " + exception.imports
		if !applied[key] {
			t.Errorf("%s no longer imports internal/commands/%s, so its verticalCrossImportException is "+
				"stale — delete it. An exception nobody needs is a standing permission the next "+
				"re-coupling falls through, which is the whole reason the list is checked in both "+
				"directions.", exception.file, exception.imports)
		}
	}

	// Non-vacuity: the scan must have matched at least one real cross-vertical
	// import (the exceptions above are all real edges measured in the tree), or
	// the walk, the import-path construction or verticalOwner's prefix match is
	// broken and every check above passed however a violation came back.
	if len(applied) == 0 {
		t.Fatal("no verticalCrossImportException was applied — the cross-vertical import scan matched " +
			"nothing, so it would pass however a violation came back")
	}
}
