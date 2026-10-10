package sdd

import (
	"go.putnami.dev/protocol/features/spectest"

	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	proto "go.putnami.dev/protocol/extension"
	sdkmanifest "go.putnami.dev/sdk/extension/manifest"
)

// This file is @putnami/sdd's manifest conformance harness. It runs the SAME
// predicates the CLI, the SDK and `putnami dev extension validate` run
// (proto.FullValidateManifest, which carries the v3 task-contract rules)
// against the REAL committed manifest, so a declaration a consumer's workspace
// would reject is rejected in this project's own test run first.
//
// On top of that it pins ONE AUTHOR: authoredManifest below is the manifest,
// written in Go through the extension SDK's builder, and the committed
// putnami.extension.json must be that document. A hand edit to the JSON that
// the authoring program does not make is a test failure here rather than a
// difference nobody notices.

const manifestPath = "putnami.extension.json"

// authoredManifest is the SDK-builder authoring of this extension's manifest —
// the single source of truth TestCommittedManifestIsTheAuthoredOne holds the
// committed JSON to.
//
// Two things the builder cannot express are set afterwards, and both are
// re-validated through sdkmanifest.Validate so nothing skips the protocol's
// verdict:
//
//   - runtime. The SDK builder (tooling/extension-sdk/manifest) has methods for
//     commands, command groups, tasks and tools, but none for the runtime
//     lifecycle primitive, so the RuntimeDefinition is assigned directly. That
//     gap is worth a Builder.Runtime method, which is an SDK change.
//   - cliContract. Build stamps it only when the manifest declares a contract
//     surface it can be earned against; see
//     TestScaffoldStampsItsContractExplicitly for why this manifest has to
//     carry the stamp anyway.
func authoredManifest(t *testing.T) *proto.Manifest {
	t.Helper()

	builder := sdkmanifest.New("@putnami/sdd", "").
		Command("sdd-selfcheck", proto.CommandDefinition{
			Description: "Report the @putnami/sdd runtime and the job context the orchestrator hands it.",
			Visibility:  "internal",
			Quiet:       true,
			Activation:  "workspace-once",
			Run:         []proto.PipelineStep{{ID: "selfcheck", Task: "sdd-selfcheck-exec"}},
		}).
		// `validate` is PROJECT-scoped and `validate-workspace` is
		// workspace-once, and they are two commands for that reason alone: one
		// command carries one activation, and features/specs are about a
		// project's own documents while architecture is about the workspace
		// graph (D1). That split is scheduling reality, not a user-facing
		// concept, so `alsoRuns` couples them at the request: asking for
		// `validate` plans `validate-workspace` too, with its own activation,
		// even when no project's features or specs are touched.
		Command("validate", proto.CommandDefinition{
			Description: "Validate a project's specification-driven declarations: its features and the specs that detail them.",
			// The command activates for a project that authors either artifact.
			// A project with neither has nothing to validate, so it is pruned at
			// plan time rather than skipped at run time.
			ActivationFiles: []string{"putnami.features.json", "specs/*.json"},
			AlsoRuns:        []string{"validate-workspace"},
			Run: []proto.PipelineStep{
				{ID: "features", Task: "features-validate"},
				// Specs resolve against the features that mint them, so the
				// order is a data dependency and not a preference: a spec
				// verdict read before the manifest verdict would report an
				// unauthored feature for a manifest that simply failed to parse.
				{ID: "specs", Task: "specs-validate", DependsOn: []string{"features"}},
			},
		}).
		Command("validate-workspace", proto.CommandDefinition{
			Description: "Validate the workspace's architecture declarations against its resolved project graph, the executable-spec enforce floor against the specs.baseline.json each enforced project commits, the settled decisions in the committed decisions.json registries, and the committed <lang>/samples/recipes.json indexes, write .github/CODEOWNERS from the owners each putnami.json declares, and check the links of every README.md file and doc/ tree of the workspace.",
			Activation:  "workspace-once",
			// The six steps are independent workspace-wide steps; none reads
			// another's answer, so no dependsOn orders them.
			Run: []proto.PipelineStep{
				{ID: "architecture", Task: "architecture-validate"},
				{ID: "specs-ratchet", Task: "specs-ratchet-validate"},
				{ID: "decisions", Task: "decisions-validate"},
				{ID: "recipes", Task: "recipes-validate"},
				{ID: "codeowners", Task: "codeowners-sync"},
				{ID: "docs-links", Task: "docs-links-validate"},
			},
		}).
		Task("features-validate", proto.TaskDefinition{
			Description: "Validate one project's durable feature manifest and the evidence backing its requirements. " +
				"Not cached, because its verdict reads three things no declared input holds: an evidence source binding records each bound file's executable bit " +
				"and each submodule's checked-out commit, which a `git:` input does not read; package-root evidence is matched against the project's version, " +
				"which the CLI gives a cacheable task as 0.0.0; and the report names the HEAD commit, which a replayed entry would report for another commit. " +
				"An under-declared key would serve a stale verdict while claiming to have checked. Keying it needs the executable bit and the submodule commit " +
				"in the `git:` digest, a report without HEAD, and a rule for the version.",
			Kind:      "command",
			Command:   "{extensionRuntime}",
			Args:      []string{"features-validate"},
			Cwd:       "{projectRoot}",
			TimeoutMs: 120000,
			// It reads the project's own source tree — the durable manifest and
			// every evidence fragment beside it — so it is serialized after the
			// tasks that REWRITE that tree. A formatter that reflows a JSON
			// document while this task parses it is a verdict about bytes that
			// no longer exist.
			Reads: []proto.ResourceRef{{ID: proto.ResourceIDSources}},
			Cache: sdkmanifest.NoCache(),
			Outputs: map[string]proto.TaskOutputPort{
				"data": {Description: "Feature validation report: counts, assessments and sorted diagnostics."},
			},
			Declares: sdkmanifest.Declares(),
		}).
		Task("specs-validate", proto.TaskDefinition{
			Description: "Validate one project's specs against the features they detail, resolve their decision links in this worktree, and report the completeness gaps around them. " +
				"It OWNS one project's rows and READS the workspace's authored identities: a spec is keyed by feature id, one spec per feature is a workspace-wide guarantee, " +
				"and a subset cannot prove either. The key follows the read set rather than the ownership — the two project ports name what this task reports on, " +
				"the workspace port names the identity tier it resolves against — because a key that stopped at the project would let a sibling's manifest change this verdict silently. " +
				"On a valid repository it additionally emits the executable-criteria projection (D9): the bounded (feature, requirement) → criterion join derived only from " +
				"manifests and specs, which core joins with run observations. The projection is a declared output so a cache hit restores it byte-identically.",
			Kind:      "command",
			Command:   "{extensionRuntime}",
			Args:      []string{"specs-validate"},
			Cwd:       "{projectRoot}",
			TimeoutMs: 120000,
			Inputs: map[string]proto.TaskInputPort{
				"manifest": {From: "project", Files: []string{"putnami.features.json"}},
				"specs":    {From: "project", Files: []string{"specs/*.json"}},
				// The identity tier, and the reason it is workspace-scoped is
				// measured rather than assumed: a feature relation resolves
				// against every authored manifest, and go/templates/go-library
				// declares a parent authored in tooling/scaffold. A per-project
				// read set reported that real relation as dangling.
				"identity": {From: "workspace", Files: []string{
					"**/putnami.features.json",
					"**/specs/*.json",
				}},
			},
			// Serialized after the project's source writers for the same reason
			// as features-validate — and here it also protects the KEY: a
			// verdict cached against pre-format bytes would be restored for a
			// tree whose files the formatter has since rewritten.
			Reads: []proto.ResourceRef{{ID: proto.ResourceIDSources}},
			// Cache captures the declared projection (no noOutput): losing the
			// projection on a hit would silently disarm the spec gate, which is
			// the one defect ADR 0002 names for run-scoped artifacts.
			Cache: cacheEnabled(),
			Outputs: map[string]proto.TaskOutputPort{
				"data": {Description: "Spec validation report: counts, completeness gaps and sorted diagnostics."},
			},
			Declares: sdkmanifest.Declares(
				sdkmanifest.Output("criteria", sdkmanifest.File("spec-criteria.json",
					sdkmanifest.InCommandOutput(),
					sdkmanifest.OptionalEmpty(),
					sdkmanifest.Describe("Executable-criteria projection: every textual spec requirement of this project joined to its same-ID authored feature requirement and criterion. Absent when the project's specs state no requirement."))),
			),
		}).
		Task("architecture-validate", proto.TaskDefinition{
			Description: "Automatically validate the workspace's ARC/DARC declarations according to committed options.sdd.verification.architecture: enforce blocks on coherent findings, report publishes them as advisory warnings, and off skips this automatic evaluation. " +
				"Policy/config/parse/provider-view errors remain blocking, and the effective mode and provenance are always reported. The evaluation compares declarations with the exact cross-domain edges of the resolved project graph. " +
				"It also reads each mapped project's committed capability manifest and compares the framework implementations recorded there with the declared imports, " +
				"reporting a declared active contract nothing implements and an implemented one nobody declared. That is committed-file evidence, never runtime observation. " +
				"Runs over the CURRENT WORKTREE ONLY and reads no git history: the shrink-only baseline comparison resolves a commit, which is state this task's key does not name, " +
				"so folding it in would make a restored verdict depend on which ref origin/HEAD points at. Dropping it is stricter, never laxer — nothing is excused as known debt — " +
				"and shrink-only ratcheting stays with the interactive `putnami architecture validate --baseline REF`.",
			Kind:      "command",
			Command:   "{extensionRuntime}",
			Args:      []string{"architecture-validate"},
			Cwd:       "{workspaceRoot}",
			TimeoutMs: 120000,
			Inputs: map[string]proto.TaskInputPort{
				"policy": {From: "workspace", Files: []string{"putnami.workspace.json"}},
				"declarations": {From: "workspace", Files: []string{
					"**/putnami.architecture.json",
					"**/architecture.baseline.json",
					"**/architecture.waivers.json",
				}},
				// Framework evidence. A committed capability
				// manifest carries the domain-access rows this verdict joins to
				// the declarations, so it is named in the key like every other
				// file the task opens. Reading it is what keeps the check
				// cacheable: the alternative — asking a build to run — would put
				// unkeyed state behind a restored verdict.
				"evidence": {From: "workspace", Files: []string{
					"**/schema/capabilities.json",
				}},
			},
			// No `reads` resource. The only files this task opens are the ARC
			// declarations and the two debt artifacts, and no task in any
			// extension writes them — a workspace-scoped `sources` read would
			// declare a conflict that does not exist and serialize against
			// nothing (project-scoped and workspace-scoped resources are
			// separate conflict keys).
			Cache: cacheEnabledNoOutput(),
			Outputs: map[string]proto.TaskOutputPort{
				"data": {Description: "Architecture validation report: effective mode and provenance, evaluation state, repository counts, coverage, findings and sorted diagnostics."},
			},
			Declares: sdkmanifest.Declares(),
		}).
		Task("specs-ratchet-validate", proto.TaskDefinition{
			Description: "Compare the executable-spec enforce floor against the specs.baseline.json each enforced project commits, with the protocol's shrink-only rule. " +
				"The CURRENT floor — every project whose effective options.sdd.verification.specs is enforce, with the feature#requirement identities its criteria make executable — " +
				"is derived from the same committed manifests, specs, and options every other gate surface reads. An enforced project that regressed to report/off, or lost a recorded " +
				"requirement, fails; the reviewed policy change is an edit to the project's committed baseline in the same diff. Growth never fails and only draws a nudge to raise the floor. " +
				"Runs over the CURRENT WORKTREE ONLY and reads no git history, so the declared input patterns cover the whole read set and a cached verdict stays honest.",
			Kind:      "command",
			Command:   "{extensionRuntime}",
			Args:      []string{"specs-ratchet-validate"},
			Cwd:       "{workspaceRoot}",
			TimeoutMs: 120000,
			Inputs: map[string]proto.TaskInputPort{
				// The whole read set: discovery (manifests and specs), policy
				// (project and workspace options), and the committed floor.
				"floor": {From: "workspace", Files: []string{
					"**/putnami.features.json",
					"**/specs/*.json",
					"**/putnami.json",
					"putnami.workspace.json",
					"**/specs.baseline.json",
				}},
			},
			// No `reads` resource for the same reason as architecture-validate:
			// every file this task opens is named by its input patterns, and no
			// task in any extension rewrites them.
			Cache: cacheEnabledNoOutput(),
			Outputs: map[string]proto.TaskOutputPort{
				"data": {Description: "Specs ratchet report: the derived enforce floor, the committed baseline's presence, the shrink/growth accounting, and sorted diagnostics."},
			},
			Declares: sdkmanifest.Declares(),
		}).
		Task("decisions-validate", proto.TaskDefinition{
			Description: "Prove the workspace's committed decisions.json registries, the root one and each project's, against this worktree. Each entry states a settled value with a stable id, the date it was settled and who settled it, " +
				"and carries either a json-value check validate proves or the reviewOnly mark a reviewer holds. A violation fails naming the decision \u2014 id, statement and settled date \u2014 so the way out is to " +
				"change the decision, not the code; an absent registry is adoption, never a failure. A check reads the files its own `files` globs name, and a repository authors those globs, " +
				"so the task reads the workspace's Git candidate cut (the tracked files and the untracked files no ignore rule excludes) and its key is the input `git:**`, which holds that cut: " +
				"editing, adding, deleting or renaming a file a glob can match moves the key, and an ignored file is neither read nor keyed. " +
				"There is no verification-mode knob: a decision a repository wrote down and dated is either held or re-decided, and a report mode would be the silent re-decision the registry exists to prevent.",
			Kind:      "command",
			Command:   "{extensionRuntime}",
			Args:      []string{"decisions-validate"},
			Cwd:       "{workspaceRoot}",
			TimeoutMs: 120000,
			// `git:**` holds the whole repository's candidate cut, the set the
			// task reads. The port is project-scoped because a workspace-once
			// task's project is rooted at the workspace root: a workspace port
			// alone would leave the project side of the key empty, and the CLI
			// keys an empty project side on every file under the root, ignored
			// build output included.
			Inputs: map[string]proto.TaskInputPort{
				"repository": {From: "project", Files: []string{"git:**"}},
			},
			// It reads arbitrary committed files a repository's own globs name,
			// so it is serialized after the tasks that REWRITE the tree: a
			// formatter reflowing a JSON document while this task parses it
			// would be a verdict about bytes that no longer exist, keyed on
			// bytes it did not read.
			Reads: []proto.ResourceRef{{ID: proto.ResourceIDSources}},
			Cache: cacheEnabledNoOutput(),
			Outputs: map[string]proto.TaskOutputPort{
				"data": {Description: "Decision report: registry presence, the settled/enforced/review-only accounting, the files checked, and one finding per violating file."},
			},
			Declares: sdkmanifest.Declares(),
		}).
		Task("recipes-validate", proto.TaskDefinition{
			Description: "Check every committed <lang>/samples/recipes.json against this worktree. Each recipe maps one intention to the sample that demonstrates it, " +
				"the framework primitives it uses and the hand-rolled shapes it replaces; a recipe whose sample is not a directory fails naming it, a second recipe for one intention fails, " +
				"and an absent index is adoption. The task reads the workspace's Git candidate cut, where a sample directory exists when it holds a candidate file, " +
				"and its key is the input `git:**`, which holds that cut: deleting, renaming or emptying a sample directory moves the key, and an ignored file does not.",
			Kind:      "command",
			Command:   "{extensionRuntime}",
			Args:      []string{"recipes-validate"},
			Cwd:       "{workspaceRoot}",
			TimeoutMs: 120000,
			Inputs: map[string]proto.TaskInputPort{
				"repository": {From: "project", Files: []string{"git:**"}},
			},
			// Serialized after the tasks that rewrite the tree, like
			// decisions-validate: a formatter reflowing an index while this task
			// parses it would be a verdict about bytes that no longer exist.
			Reads: []proto.ResourceRef{{ID: proto.ResourceIDSources}},
			Cache: cacheEnabledNoOutput(),
			Outputs: map[string]proto.TaskOutputPort{
				"data": {Description: "Recipe report: the index files read, the recipe count, and sorted diagnostics."},
			},
			Declares: sdkmanifest.Declares(),
		}).
		Task("docs-links-validate", proto.TaskDefinition{
			Description: "Check every relative link and anchor in every README.md file and doc/ tree of the workspace, once per run whatever the selection: the documents no project owns, the projects without a language extension, and a link from one project into another that a change to the other breaks. It applies the SDK's docslinks rule, which each language extension's lint-docs task applies inside its project. " +
				"A link may name any file of the workspace, so the task reads the workspace's Git candidate cut and its key is the input `git:**`, which holds that cut: " +
				"deleting or renaming a link target, or editing a heading, moves the key, and a link to an ignored file is broken, as it is in a clone.",
			Kind:      "command",
			Command:   "{extensionRuntime}",
			Args:      []string{"docs-links-validate"},
			Cwd:       "{workspaceRoot}",
			TimeoutMs: 120000,
			Inputs: map[string]proto.TaskInputPort{
				"repository": {From: "project", Files: []string{"git:**"}},
			},
			// Serialized after the tasks that rewrite the tree, like
			// recipes-validate: a formatter rewriting a README while this task
			// reads it would be a verdict about bytes that no longer exist.
			Reads: []proto.ResourceRef{{ID: proto.ResourceIDSources}},
			Cache: cacheEnabledNoOutput(),
			Outputs: map[string]proto.TaskOutputPort{
				"data": {Description: "Documentation link report: the number of documents checked and the sorted broken links."},
			},
			Declares: sdkmanifest.Declares(),
		}).
		Task("codeowners-sync", proto.TaskDefinition{
			Description: "Write .github/CODEOWNERS from the owners declared in options.sdd.owners: putnami.workspace.json gives the catch-all rule, a scope or project putnami.json gives its directory a rule, " +
				"and the spec-governance files stay with the workspace owners. The committed file is rewritten only when it differs, so it follows the declarations without a command of its own; " +
				"a workspace that declares no owners is left alone. The task reads the workspace's Git candidate cut, which holds the putnami.json of every project and every directory above one " +
				"and the CODEOWNERS file it rewrites, and its key is the input `git:**`, which holds that cut. A run that rewrote the file is never replayed: " +
				"the next run is keyed on the new bytes, and only a run that left the tree unchanged is.",
			Kind:      "command",
			Command:   "{extensionRuntime}",
			Args:      []string{"codeowners-sync"},
			Cwd:       "{workspaceRoot}",
			TimeoutMs: 120000,
			// Serialized after the tasks that rewrite the tree, like
			// decisions-validate: a formatter reflowing a putnami.json while this
			// task parses it would render owners from bytes that no longer exist.
			Reads: []proto.ResourceRef{{ID: proto.ResourceIDSources}},
			// It rewrites a tracked file, so it says so: the planner serializes it
			// with the other source writers, and the scheduler drops its file
			// digests after it runs, so a later task keys on the new bytes.
			Writes: []proto.ResourceRef{sdkmanifest.SourcesWrite()},
			// The port is project-scoped on purpose: the CLI proves a source
			// writer left the tree unchanged by rehashing its project-side
			// key patterns after the run, and only then replays its result.
			Inputs: map[string]proto.TaskInputPort{
				"repository": {From: "project", Files: []string{"git:**"}},
			},
			Cache: cacheEnabledNoOutput(),
			Outputs: map[string]proto.TaskOutputPort{
				"data": {Description: "CODEOWNERS report: whether owners are declared, the rendered rules with their declaring file, whether the file was rewritten, and sorted diagnostics."},
			},
			Declares: sdkmanifest.Declares(sdkmanifest.MutatesSources()),
		}).
		Task("sdd-selfcheck-exec", proto.TaskDefinition{
			Description: "Print the extension identity, the job-context protocol version and the resolved `selection` block, then exit. " +
				"Declares no outputs and no effects: it reads only the context file the orchestrator wrote and writes only its own result document. " +
				"Uncacheable on purpose — its whole verdict is about the environment of THIS invocation, so a restored answer from another one would be the single thing it must never report.",
			Kind:      "command",
			Command:   "{extensionRuntime}",
			Args:      []string{"selfcheck"},
			Cwd:       "{workspaceRoot}",
			TimeoutMs: 60000,
			Cache:     sdkmanifest.NoCache(),
			Outputs: map[string]proto.TaskOutputPort{
				"data": {Description: "Extension identity plus the job-context members the SDD engines read."},
			},
			Declares: sdkmanifest.Declares(),
		})

	// The four interactive command groups, and the flat command + task each of
	// their eighteen subcommands needs. They live in manifest_groups_test.go
	// because they are three times the manifest the validation jobs are, and the
	// one-author rule reads better when the table it authors is next to it.
	builder = withSDDCommandGroups(builder)

	// The five MCP tools, in manifest_tools_test.go for
	// the same reason. They are bounded questions asked by an agent instead of a
	// terminal, and they run the same engine builders.
	builder = withSDDMCPTools(builder)

	built, err := builder.Build()
	if err != nil {
		t.Fatalf("the authored manifest was rejected at authoring time: %v", err)
	}

	built.Runtime = &proto.RuntimeDefinition{
		Executable: "compiled/putnami-sdd",
		Toolchains: map[string]proto.RuntimeToolchain{
			"runtimeCompiler": {
				Lock: "go",
				Candidates: []proto.RuntimeToolchainCandidate{
					{From: proto.RuntimeToolchainCandidateEnvironment, Environment: "GOROOT", Path: "bin/go"},
					{From: proto.RuntimeToolchainCandidatePath, Path: "go"},
					{From: proto.RuntimeToolchainCandidatePutnamiHome, Path: "toolchains/go/go-{version}/go/bin/go"},
				},
				Probe: proto.RuntimeToolchainProbe{
					Args:        []string{"env", "GOVERSION"},
					Expect:      "go{version}",
					Unset:       []string{"GOROOT"},
					Environment: map[string]string{"GOTOOLCHAIN": "local"},
				},
				Environment: map[string]proto.RuntimeToolchainEnvironment{
					"GOROOT":      {From: proto.RuntimeToolchainEnvironmentAncestor, Levels: 2},
					"GOTOOLCHAIN": {From: proto.RuntimeToolchainEnvironmentLiteral, Value: "local"},
				},
				PrependPath: true,
			},
		},
		Prepare: &proto.RuntimePrepare{
			Command: "{extensionRoot}/bin/prepare",
			Args:    []string{"--output", "{runtimeOutput}"},
			// bin/prepare is in its OWN input set on purpose: without it,
			// editing the build script does not change the artifact digest and
			// the CLI keeps serving the binary the previous script produced.
			Inputs:     []string{"bin/prepare", "cmd/**", "go.mod", "go.sum", "internal/**"},
			Toolchains: []string{"runtimeCompiler"},
		},
	}
	built.CLIContract = protocolcli.CurrentContract
	// The bare `options.<name>` blocks this extension reads for itself, beyond
	// the ones keyed by its own name or path reference. Declaring them never
	// widens THIS extension's cache keys; it lets every other extension's key
	// drop a block its tasks cannot read. `publish` is listed because the spec
	// reader consults a project's publish block; being a command name, it stays
	// in the key of every extension providing `publish` regardless.
	built.OptionNamespaces = []string{"sdd", "publish"}

	if err := sdkmanifest.Validate(built); err != nil {
		t.Fatalf("the manifest is not conformant once the runtime is attached: %v", err)
	}
	return built
}

// committedManifest parses the shipped putnami.extension.json exactly as
// `putnami dev extension validate` and the package-time gate do.
func committedManifest(t *testing.T) *proto.Manifest {
	t.Helper()
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	m, diags := proto.ParseManifest(data)
	if m == nil || diag.HasErrors(diags) {
		t.Fatalf("parse extension manifest: %s", formatDiagnostics(diags))
	}
	return m
}

func formatDiagnostics(diags []diag.Diagnostic) string {
	if len(diags) == 0 {
		return "<none>"
	}
	msgs := make([]string, 0, len(diags))
	for _, d := range diags {
		msgs = append(msgs, d.String())
	}
	return strings.Join(msgs, "\n  ")
}

// TestCommittedManifestIsTheAuthoredOne is the one-author rule.
//
// Both documents are compared in the PROTOCOL's canonical form — what
// proto.ParseAndValidateManifest leaves a manifest in, which is the shape the
// CLI holds a loaded one in — so the comparison is over the document and never
// over an encoder's field order, a defaulted map spelling, or the JSON
// formatting a human chose. That normalization has to be the protocol's: a
// hand-rolled one would be a second opinion about canonical form, and the drift
// between the two opinions is exactly what this test exists to catch.
//
// It is also what makes the comparison possible at all. proto.Manifest's
// ExtensionDeps is a non-pointer struct with a custom marshaller, so
// `omitempty` cannot drop it: a builder-authored manifest round-trips through
// JSON inside Build and comes back with an initialized empty map, which
// encodes as `"extensionDependencies": {}`, while a committed file that never
// mentions the key encodes as `null`. NormalizeManifest initializes the map on
// both sides, so canonical form settles the difference and the committed file
// does not have to carry an empty key nobody wrote on purpose.
func TestCommittedManifestIsTheAuthoredOne(t *testing.T) {
	authored, err := json.Marshal(authoredManifest(t))
	if err != nil {
		t.Fatalf("encode the authored manifest: %v", err)
	}
	committed, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}

	got := canonical(t, canonicalDocument(t, committed))
	want := canonical(t, canonicalDocument(t, authored))
	if got != want {
		t.Errorf("%s is not the document the SDK builder authors.\n committed:\n%s\n authored:\n%s",
			manifestPath, got, want)
	}
}

// TestHarnessRejectsAHandEditedManifest keeps the test above from being
// vacuous: a comparison that silently passed on everything would look
// identical to a passing one. Mutating the committed bytes in a way no
// authoring program would produce must fail the same comparison.
func TestHarnessRejectsAHandEditedManifest(t *testing.T) {
	committed, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	edited := strings.Replace(string(committed), `"timeoutMs": 60000`, `"timeoutMs": 60001`, 1)
	if edited == string(committed) {
		t.Fatal("the mutation did not apply; this harness is no longer checking anything")
	}

	authored, err := json.Marshal(authoredManifest(t))
	if err != nil {
		t.Fatalf("encode the authored manifest: %v", err)
	}
	if canonical(t, canonicalDocument(t, []byte(edited))) == canonical(t, canonicalDocument(t, authored)) {
		t.Fatal("a hand-edited manifest still matched the authored one; the comparison is not comparing")
	}
}

// TestExtensionManifestPassesFullValidation is the `putnami dev extension
// validate` gate as a unit test: the real manifest must produce ZERO
// diagnostics under the comprehensive protocol entry point, which includes the
// v3 task-contract harness (exactness, one owner per output, honest effects).
func TestExtensionManifestPassesFullValidation(t *testing.T) {
	m := committedManifest(t)
	if diags := proto.FullValidateManifest(m); len(diags) > 0 {
		t.Fatalf("manifest is not conformant:\n  %s", formatDiagnostics(diags))
	}
}

// TestScaffoldStampsItsContractExplicitly pins a decision that is invisible in
// the JSON.
//
// Build earns the cliContract stamp only for a manifest that declares a
// contract surface — commands, command groups or tools
// (proto.DeclaresContractSurface) — because the packager never stamps a
// hook-only manifest and requiring a stamp from one would reject every
// framework package. This manifest DOES declare a command, so the stamp is
// earned rather than asserted, and it must be present: since contract 3 an
// unstamped manifest that declares a surface does not load at all
// (proto.LoadManifest), which is the silent "extension skipped" this project's
// discovery check exists to prevent.
func TestScaffoldStampsItsContractExplicitly(t *testing.T) {
	m := committedManifest(t)
	if !proto.DeclaresContractSurface(m) {
		t.Fatal("the manifest declares no command, command group or tool; proto.ValidateManifest requires at least one and LoadManifest would treat it as hook-only")
	}
	if m.CLIContract != protocolcli.CurrentContract {
		t.Fatalf("cliContract = %d, want %d; an unstamped manifest that declares a surface is rejected by LoadManifest and the extension disappears from discovery",
			m.CLIContract, protocolcli.CurrentContract)
	}
}

// TestRuntimeIsPreparedFromItsOwnSources pins the runtime declaration, and one
// entry of it in particular.
//
// The artifact digest is taken over `inputs`, so an input the build reads but
// does not declare is a stale binary served after every edit to it. bin/prepare
// is that input: it is the build script, and a change to it changes the
// artifact. cmd/** and internal/** cover the sources — internal/** matches
// nothing yet and is declared anyway, so the engine move
// invalidates the prepared runtime instead of arriving under an unchanged
// digest.
func TestRuntimeIsPreparedFromItsOwnSources(t *testing.T) {
	m := committedManifest(t)
	if !m.DeclaresRuntime() {
		t.Fatal("the manifest declares no runtime; every task of this extension names {extensionRuntime}")
	}
	if got, want := m.Runtime.Executable, "compiled/putnami-sdd"; got != want {
		t.Errorf("runtime executable = %q, want %q", got, want)
	}
	if !m.Runtime.RequiresPreparation() {
		t.Fatal("the runtime declares no prepare step; a local extension has no shipped binary to run")
	}
	wantInputs := []string{"bin/prepare", "cmd/**", "go.mod", "go.sum", "internal/**"}
	if got := append([]string(nil), m.Runtime.Prepare.Inputs...); !reflect.DeepEqual(got, wantInputs) {
		t.Errorf("prepare inputs = %v, want %v", got, wantInputs)
	}
	if got, want := m.Runtime.Prepare.Toolchains, []string{"runtimeCompiler"}; !reflect.DeepEqual(got, want) {
		t.Errorf("prepare toolchains = %v, want %v", got, want)
	}
	compiler, ok := m.Runtime.Toolchains["runtimeCompiler"]
	if !ok {
		t.Fatal("runtimeCompiler is not declared; prepare cannot resolve the locked Go compiler")
	}
	if compiler.Lock != "go" {
		t.Errorf("runtimeCompiler lock = %q, want go", compiler.Lock)
	}
	if !compiler.PrependPath {
		t.Error("runtimeCompiler does not prepend its resolved executable to PATH")
	}
}

// TestValidationCommandsCarryTheActivationsD1Settles pins the shape of the two
// job commands, because it is the reason there are two of them.
//
// `validate` is project-scoped and activates on the artifacts it validates, so
// a project that authors neither is pruned at plan time instead of running a
// task that would skip. `validate-workspace` is workspace-once because its
// subject is the workspace graph. One command cannot carry both activations,
// which is what makes the split a contract and not a preference.
func TestValidationCommandsCarryTheActivationsD1Settles(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "opt-in-activation", "validation-commands-carry-the-settled-activations")
	m := committedManifest(t)

	project, found := m.Commands["validate"]
	if !found {
		t.Fatal("the manifest declares no `validate` command")
	}
	if project.Activation != "" {
		t.Errorf("validate activation = %q, want the default project scope", project.Activation)
	}
	if want := []string{"putnami.features.json", "specs/*.json"}; !reflect.DeepEqual(project.ActivationFiles, want) {
		t.Errorf("validate activationFiles = %v, want %v", project.ActivationFiles, want)
	}
	// Order is a data dependency: specs resolve against the features that mint
	// them, so a spec verdict read before the manifest verdict would report an
	// unauthored feature for a manifest that merely failed to parse.
	if len(project.Run) != 2 ||
		project.Run[0].Task != "features-validate" ||
		project.Run[1].Task != "specs-validate" ||
		!reflect.DeepEqual(project.Run[1].DependsOn, []string{"features"}) {
		t.Errorf("validate pipeline = %+v, want features-validate then specs-validate", project.Run)
	}

	workspace, found := m.Commands["validate-workspace"]
	if !found {
		t.Fatal("the manifest declares no `validate-workspace` command")
	}
	if workspace.Activation != "workspace-once" {
		t.Errorf("validate-workspace activation = %q, want workspace-once", workspace.Activation)
	}
	// The six workspace-wide steps are independent — none reads another's
	// answer — so no dependsOn orders them.
	if len(workspace.Run) != 6 ||
		workspace.Run[0].Task != "architecture-validate" ||
		workspace.Run[1].Task != "specs-ratchet-validate" ||
		workspace.Run[2].Task != "decisions-validate" ||
		workspace.Run[3].Task != "recipes-validate" ||
		workspace.Run[4].Task != "codeowners-sync" ||
		workspace.Run[5].Task != "docs-links-validate" ||
		len(workspace.Run[1].DependsOn) != 0 ||
		len(workspace.Run[2].DependsOn) != 0 ||
		len(workspace.Run[3].DependsOn) != 0 ||
		len(workspace.Run[4].DependsOn) != 0 ||
		len(workspace.Run[5].DependsOn) != 0 {
		t.Errorf("validate-workspace pipeline = %+v, want the architecture step, the specs ratchet, the decision gate, the recipe gate, the CODEOWNERS step and the documentation link gate", workspace.Run)
	}
	// Both are user-facing: they are what `putnami validate` and the CI gate
	// run. The internal self-check is the only hidden command here.
	for _, name := range []string{"validate", "validate-workspace"} {
		if visibility := m.Commands[name].Visibility; visibility != "" {
			t.Errorf("command %q has visibility %q; a gate command a user runs is not internal", name, visibility)
		}
	}
	if m.Commands["sdd-selfcheck"].Visibility != "internal" {
		t.Error("the self-check is a diagnostic, not a surface; it must stay internal")
	}
}

// TestCachePolicyMatchesTheDeclaredReadSet is D8 as an assertion, per task.
//
// The rule it enforces is one sentence: a task may be cacheable only when its
// declared inputs COVER what it reads — which is not the same as what it
// REPORTS ON. specs-validate reports on one project and resolves against every
// authored manifest and spec in the workspace, so it declares both.
// architecture-validate reads the workspace's ARC declarations, capability
// evidence, and adoption policy, which is exactly what it declares. The four
// workspace steps whose read set no narrow pattern names (decisions, recipes,
// codeowners and docs links) read the repository's Git candidate cut and
// declare `git:**`, which holds it. features-validate reads evidence source
// bindings, which record executable bits and submodule commits no declared
// input holds, so it is uncacheable rather than cached against a key that
// omits part of its inputs.
//
// It asserts the SCOPE of each port rather than its file list: the lists are
// held byte-for-byte by TestCommittedManifestIsTheAuthoredOne against the
// authoring above, while a port that stayed spelled the same and moved between
// project and workspace scope is a key change nothing else would catch.
func TestCachePolicyMatchesTheDeclaredReadSet(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "declared-inputs-cover-the-read-set", "the-cache-policy-matches-the-declared-read-set")
	spectest.Proves(t, "tooling/specification-driven-development", "architecture-adoption-policy", "architecture-policy-is-part-of-cache-key")
	m := committedManifest(t)

	// The one task whose read set no declared input holds: an evidence source
	// binding records each bound file's executable bit and each submodule's
	// checked-out commit, which a `git:` input does not read, and the report
	// names the HEAD commit.
	features := m.Tasks["features-validate"]
	if features.Cache.IsEnabled() {
		t.Error("task \"features-validate\" is cacheable although no declared input holds its read set")
	}
	if len(features.Inputs) > 0 {
		t.Errorf("task \"features-validate\" declares inputs %v; an uncacheable task's ports would read as a key it does not have",
			sortedKeys(features.Inputs))
	}

	// The workspace steps whose read set no narrow pattern names. Each reads
	// the repository's Git candidate cut, and `git:**` holds exactly that cut.
	// The port is project-scoped because a workspace-once task's project is
	// rooted at the workspace root, and a project side left empty would key on
	// every file under the root, ignored build output included.
	for _, name := range []string{"decisions-validate", "recipes-validate", "codeowners-sync", "docs-links-validate"} {
		task := m.Tasks[name]
		if !task.Cache.IsEnabled() || !task.Cache.NoOutput {
			t.Errorf("task %q cache = %+v, want enabled with noOutput: its key holds the candidate cut it reads", name, task.Cache)
		}
		want := map[string]proto.TaskInputPort{"repository": {From: "project", Files: []string{"git:**"}}}
		if !reflect.DeepEqual(task.Inputs, want) {
			t.Errorf("task %q inputs = %+v, want %+v", name, task.Inputs, want)
		}
	}

	// port scope -> the reason it cannot move.
	wantScopes := map[string]map[string]string{
		"specs-validate": {
			"manifest": "project",
			"specs":    "project",
			// The identity tier. A feature relation resolves against every
			// authored manifest, so a sibling's manifest changes this verdict; a
			// project-scoped port would not move the key when it does.
			"identity": "workspace",
		},
		// Project-rooted patterns here would key a workspace verdict on one
		// project's files. The policy port covers the committed mode that changes
		// whether the same coherent findings block.
		"architecture-validate": {"declarations": "workspace", "evidence": "workspace", "policy": "workspace"},
	}
	for _, name := range sortedKeys(wantScopes) {
		task := m.Tasks[name]
		if !task.Cache.IsEnabled() {
			t.Errorf("task %q is uncacheable although its inputs cover its read set", name)
		}
		// specs-validate is the one cacheable task whose output the store must
		// capture: its criteria projection arms the spec gate, and a hit that
		// restored the verdict without the artifact would silently disarm it.
		// architecture-validate stays pure data, so it stays noOutput.
		wantNoOutput := name != "specs-validate"
		if task.Cache == nil || task.Cache.NoOutput != wantNoOutput {
			t.Errorf("task %q noOutput = %v, want %v", name, task.Cache != nil && task.Cache.NoOutput, wantNoOutput)
		}
		if got := sortedKeys(task.Inputs); !reflect.DeepEqual(got, sortedKeys(wantScopes[name])) {
			t.Errorf("task %q declares ports %v, want %v", name, got, sortedKeys(wantScopes[name]))
			continue
		}
		for port, want := range wantScopes[name] {
			declared := task.Inputs[port]
			if declared.From != want {
				t.Errorf("task %q port %q reads from %q, want %q", name, port, declared.From, want)
			}
			if len(declared.Files) == 0 {
				t.Errorf("task %q port %q declares no files; an empty port keys on nothing", name, port)
			}
		}
	}
}

// TestEveryTaskCarriesAV3Declaration pins that the contract is COMPLETE. v3 is
// additive per task, so a task that quietly loses (or never gains) its
// `declares` block keeps v2 inferred capture while the rest of the manifest is
// declared — the mixed state this contract exists to end. The version itself is
// asserted with it, because a manifest whose tasks all lost the block would
// still validate as v2.
func TestEveryTaskCarriesAV3Declaration(t *testing.T) {
	m := committedManifest(t)
	if got := proto.ManifestProtocolVersion(m); got != proto.ProtocolVersionV3 {
		t.Fatalf("manifest protocol version = %d, want %d (v3 task declarations)", got, proto.ProtocolVersionV3)
	}
	for _, name := range sortedKeys(m.Tasks) {
		if m.Tasks[name].Declares == nil {
			t.Errorf("task %q has no v3 declaration; every task in this manifest must state its outputs, effects and source mutation (an empty object states \"none of the three\")", name)
		}
	}
}

// TestNoTaskDeclaresAnEffectOrOwnsAPath pins HONEST EFFECTS and output
// ownership, and it is universal rather than a per-task table: no task in this
// manifest reaches the network, a registry or the cloud, and only one owns a
// declared output path. A task added later has to justify itself here rather
// than be excused by a row someone remembered to add.
//
// One task owns one path, and the exception is the justification:
// specs-validate emits the executable-criteria projection (D9), the
// bounded derivation core joins with run observations, and declaring it is
// exactly what makes a cache hit restore it instead of silently disarming the
// spec gate.
//
// Some tasks nevertheless WRITE into the source tree: the authoring
// subcommands (`specs init`, `contracts generate` and the others
// TestTheTwoWritingSubcommandsSaySo pins), and the codeowners-sync step, which
// rewrites .github/CODEOWNERS (TestCodeownersSyncDeclaresItWritesSources).
// That is `mutatesSources`, not an effect. Whether a task is
// CACHEABLE is a third question, and
// TestCachePolicyMatchesTheDeclaredReadSet answers it.
func TestNoTaskDeclaresAnEffectOrOwnsAPath(t *testing.T) {
	m := committedManifest(t)

	for _, name := range sortedKeys(m.Tasks) {
		declares := m.Tasks[name].Declares
		if declares == nil {
			continue // TestEveryTaskCarriesAV3Declaration owns that failure.
		}
		if len(declares.Effects) > 0 {
			t.Errorf("task %q declares effects %v; nothing here reaches the network, a registry, a toolchain cache or the cloud, and an effect would make the cacheable tasks dishonest", name, declares.Effects)
		}
		if name == "specs-validate" {
			output, owns := declares.Outputs["criteria"]
			if len(declares.Outputs) != 1 || !owns {
				t.Errorf("specs-validate must declare exactly the criteria projection, got %v", sortedKeys(declares.Outputs))
				continue
			}
			if output.Kind != "file" || output.Root != proto.OutputRootCommandOutput ||
				output.Path != "spec-criteria.json" || !output.OptionalEmpty {
				t.Errorf("criteria projection declaration = %+v; it must be an optional file at spec-criteria.json under command-output", output)
			}
			continue
		}
		if len(declares.Outputs) > 0 {
			t.Errorf("task %q claims declared output paths %v; only specs-validate owns a path here (the criteria projection), and that exception is justified above", name, sortedKeys(declares.Outputs))
		}
	}
	// The self-check is uncacheable for a reason none of the others share: its
	// whole verdict is about the environment of THIS invocation, so a restored
	// answer from another one would be the single thing it must never report.
	if m.Tasks["sdd-selfcheck-exec"].Cache.IsEnabled() {
		t.Error("sdd-selfcheck-exec is cacheable; a restored self-check reports another invocation's environment")
	}

	if diags := proto.ValidateOutputOwnership(m); len(diags) > 0 {
		t.Fatalf("declared outputs are not exclusively owned:\n  %s", formatDiagnostics(diags))
	}
}

// canonicalDocument re-reads a manifest through the protocol's own strict parse
// and normalization, then decodes it into a plain document, so two manifests
// are compared in the shape the CLI holds them in.
func canonicalDocument(t *testing.T, data []byte) map[string]any {
	t.Helper()
	parsed, diags := proto.ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("manifest failed the protocol's strict parse: %s\n%s", formatDiagnostics(diags), data)
	}
	encoded, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("encode normalized manifest: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return document
}

// canonical re-encodes a decoded document with every key sorted, so a diff
// shows a real difference and never a field-order difference.
func canonical(t *testing.T, document map[string]any) string {
	t.Helper()
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	return string(data)
}

func sortedKeys[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// cacheEnabledNoOutput is the D8 policy a cacheable validation task with a
// pure-data result carries: the store keeps a status-only entry instead of
// capturing a shared output directory that holds sibling steps' files.
//
// The SDK has NoCache() and no positive counterpart, because "cache this" is
// not one decision — enabled, noOutput, determinism and restore mode are
// independent — so the policy is written out here rather than hidden behind a
// helper that would have to guess three of them.
func cacheEnabledNoOutput() *proto.TaskCachePolicy {
	enabled := true
	return &proto.TaskCachePolicy{Enabled: &enabled, NoOutput: true}
}

// cacheEnabled is the policy for a cacheable task whose declared outputs the
// store must capture and restore. specs-validate moved here when it gained the
// criteria projection (D9): noOutput would keep the verdict and drop the
// artifact, and a cache hit that loses the projection silently disarms the
// spec gate.
func cacheEnabled() *proto.TaskCachePolicy {
	enabled := true
	return &proto.TaskCachePolicy{Enabled: &enabled}
}

// TestCodeownersSyncDeclaresItWritesSources pins both halves of the source
// write codeowners-sync performs. Without them the scheduler keeps the digest
// of the CODEOWNERS it just replaced, and a later task in the same run keys on
// bytes that no longer exist.
//
// It also pins what makes caching the writer sound. The CLI replays a source
// writer only when rehashing its project-side key patterns after the run finds
// the bytes unchanged, and it refuses to replay a writer that has none. The
// task therefore keys on a project-scoped `git:**` port, which holds the
// CODEOWNERS file it rewrites: a run that rewrote it is never replayed.
func TestCodeownersSyncDeclaresItWritesSources(t *testing.T) {
	task := committedManifest(t).Tasks["codeowners-sync"]
	if task.Declares == nil || !task.Declares.MutatesSources {
		t.Errorf("codeowners-sync declares %+v, want mutatesSources", task.Declares)
	}
	writes := false
	for _, resource := range task.Writes {
		if resource.ID == proto.ResourceIDSources {
			writes = true
		}
	}
	if !writes {
		t.Errorf("codeowners-sync writes %+v, want the sources resource", task.Writes)
	}
	if !task.Cache.IsEnabled() || !task.Cache.NoOutput {
		t.Errorf("codeowners-sync cache = %+v, want enabled with noOutput", task.Cache)
	}
	port, found := task.Inputs["repository"]
	if !found || port.From != "project" || !reflect.DeepEqual(port.Files, []string{"git:**"}) {
		t.Errorf("codeowners-sync inputs = %+v, want the project-scoped port repository on git:**, "+
			"the patterns the CLI rehashes to prove a run left CODEOWNERS unchanged", task.Inputs)
	}
}
