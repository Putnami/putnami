package cli

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
)

// The core-isolation ratchets: three hard pins, set before any later change
// lands so every movement is a reviewed edit here rather than a silent
// increment.
//
// This program's whole shape is a subtraction: core keeps command dispatch, typed
// plans, execution, caching, canonical identity, security boundaries, events
// and results; everything provider-shaped (runtimes, metadata probes, native
// caches, preflight policy, infra defaults, test environments) moves behind
// exactly three generic lifecycle primitives. These pins are what keeps that
// subtraction subtracted — the same reasoning as structural_baseline_test.go
// and complexity_ceilings_test.go, extended to this program's own axes.

func TestNoFourthLifecyclePrimitive(t *testing.T) {
	t.Parallel()
	const want = 3
	if len(extensionproto.ValidLifecyclePrimitives) != want {
		t.Fatalf("protocol registry lists %d lifecycle primitives, pinned at %d — the closed "+
			"list moved. Core gains exactly three generic lifecycle primitives; everything "+
			"else is expressed as typed tasks. A fourth primitive is a program-level decision, not "+
			"an incremental one.", len(extensionproto.ValidLifecyclePrimitives), want)
	}
	expected := []extensionproto.LifecyclePrimitiveID{
		extensionproto.LifecyclePrimitiveInvocation,
		extensionproto.LifecyclePrimitiveRuntime,
		extensionproto.LifecyclePrimitiveWorkspace,
	}
	for i, primitive := range extensionproto.ValidLifecyclePrimitives {
		if primitive != expected[i] {
			t.Errorf("protocol primitive %d is %q, want %q — registry order or identity drifted",
				i, primitive, expected[i])
		}
	}
}

// providerTerms is the provider vocabulary that must not name a NEW core
// package. Short language tokens match as prefix or suffix of the package
// directory name; longer terms match anywhere in it.
var providerTerms = []string{
	"typescript", "python", "docker", "postgres", "node", "bun",
	"go", "ts", "py", "pg", "uv", "npm", "oci",
}

// providerPackageAllowlist names the packages the term scan must not fail on.
// Historically it held the provider-specific packages that existed TODAY and
// were scheduled for deletion by this program itself; no package of that kind may
// join it, because this program moves provider knowledge into extensions and a new
// provider-shaped core package is the exact regression the program exists to
// prevent. Entries of that kind LEAVE when a later change deletes them.
//
// It holds no such package now. internal/testinfra — the docker-CLI Postgres
// provisioner behind `putnami test` — was the last one, and an earlier change
// deleted it along with internal/jobs/test_infra.go: database test environments
// are an extension task over the generic invocation primitive
// (go.putnami.dev/sdk/extension/dbtestenv).
//
// The only other admissible entry is a SUBSTRING FALSE POSITIVE — the scan
// matches short language tokens as a prefix or suffix of the directory name, so
// a package whose name merely ends in one of them is caught by spelling, not by
// meaning. Such an entry must say which term it collides with and why the
// package is provider-neutral, so a reviewer can check the claim in one line
// instead of trusting the map.
var providerPackageAllowlist = map[string]string{
	// Substring false positive: "agentartifacts" ends in "ts". The package
	// materializes lock-pinned agent-workflow files (.agents/**, .claude/**)
	// into a workspace; it names no language, runtime, package manager or
	// image format, and its inputs are a lock entry and a content manifest.
	"internal/agentartifacts": "ends in the \"ts\" token by spelling; agent-workflow materialization is provider-neutral",
}

func matchesProviderTerm(pkgName string) string {
	for _, term := range providerTerms {
		if len(term) <= 3 {
			if strings.HasPrefix(pkgName, term) || strings.HasSuffix(pkgName, term) {
				return term
			}
		} else if strings.Contains(pkgName, term) {
			return term
		}
	}
	return ""
}

func TestNoNewProviderSpecificCorePackage(t *testing.T) {
	t.Parallel()
	for _, pkg := range internalProductionPackages(t) {
		if _, allowed := providerPackageAllowlist[pkg]; allowed {
			continue
		}
		if term := matchesProviderTerm(filepath.Base(pkg)); term != "" {
			t.Errorf("package %s matches provider term %q and is not in the allowlist — this "+
				"program moves provider knowledge into extensions; a new provider-specific core "+
				"package reverses that. If the name is a false positive, say so in "+
				"providerPackageAllowlist with the reason.", pkg, term)
		}
	}

	// The allowlist itself only shrinks: a listed package that no longer exists
	// was deleted by a later change, and its entry must go with it.
	existing := make(map[string]bool)
	for _, pkg := range internalProductionPackages(t) {
		existing[pkg] = true
	}
	for pkg := range providerPackageAllowlist {
		if !existing[pkg] {
			t.Errorf("allowlisted package %s no longer exists — it was deleted; remove the "+
				"entry so the allowlist stays an inventory, not a graveyard", pkg)
		}
	}
}

// internalPackageCountCeiling pins the number of internal packages holding
// production Go files. Ceiling-only: this program DELETES packages (whatever the
// anti-reentry sweep finds), so shrinkage is success. Growth is the sprawl the
// count exists to catch — a new package is a new surface, and under this program
// the first question about any new surface is "why is this not
// extension-owned?".
//
// History: 37 at the start; 35 after an earlier change deleted internal/gocache and
// internal/buncache, whose collectors are now the owning extensions' own
// cache-gc commands; 34 after another earlier change deleted internal/testinfra, whose
// docker-CLI Postgres provisioner is now the extension SDK's dbtestenv package
// behind an ordinary typed task. The ceiling MOVES DOWN with each deletion —
// leaving it at the old value would let this program's own subtraction pay for
// later sprawl.
//
// The anti-reentry ratchets (provider_vocabulary_ratchets_test.go) hold it at 34 rather
// than lowering it, and the trade is stated
// rather than absorbed: they DELETED internal/config — 53 lines of
// `type X = workspace.X` over go.putnami.dev/protocol/workspace, fan-in 8, the
// highest-fan-in package in the tree and a shim behind every one of those edges
// — and ADDED internal/env, the 13-line PUTNAMI_* reader that package carried
// for no reason but its name. One package out, one package in, ~700 call sites
// repointed at the protocol they were always reaching for. A closeout that
// lowered the number by inlining the reader into a consumer would have bought
// the pin with a worse import graph.
//
// RAISED to 35, which adds internal/mapgen — the workspace-map
// generator (fragment build, inputs digest, reduce, render, write/check). It is
// a package rather than a corner of internal/commands because it has TWO
// consumers on opposite sides of a hard boundary: `putnami context map` in
// internal/commands, and the build attachment in internal/engine, which this
// module's own ratchet (provider_vocabulary_ratchets_test.go) forbids from importing
// internal/commands. Folding it into either consumer would reopen that import
// or duplicate the generator. Extension ownership — this program's first question
// about any new surface — is not available here: a third-party extension cannot
// attach steps to a foreign verb's pipeline, which is the same argument that
// puts the artifact in the framework's `context` family at all.
//
// RAISED to 36, which adds internal/changeplan. The immutable CI
// document is shared by the command adapter and future Cloud admission
// consumers; keeping canonical serialization and digest validation out of
// internal/commands prevents a second wire implementation from growing there.
//
// RAISED to 37, which adds internal/features. The deterministic local
// feature engine has three consumers across two hard boundaries: the validation
// and inspection commands, provisional snapshot rendering, and the immutable
// Git-tree reader used by revision diffing. The durable protocol package cannot
// own derived assessment state, internal/commands cannot be imported by the
// revision reader, and extension ownership would make framework-neutral
// workspace aggregation depend on one provider. A focused internal package is
// therefore the smaller surface than duplicating discovery and maturity rules.
//
// RAISED to 38, which adds internal/agentartifacts — the materializer
// for the lock-pinned agent-workflow artifact. Extension ownership, this program's
// first question, does not apply: the artifact is workspace content
// (.agents/**, .claude/**) carrying no language, runtime or toolchain, so
// giving it to one provider would make a workspace-wide install depend on which
// language extension happens to be installed. It is a package rather than a
// corner of internal/commands for a reason this change makes concrete: it ships
// NO command at all — lifecycle wiring comes later — and its guarantees
// (ownership proof, abort-before-first-write, transactional publish, idempotent
// recovery) are precisely the properties that must be provable without a CLI
// invocation. A library with no command surface cannot live inside the command
// package.
//
// RAISED to 39, which adds internal/commands/shared.
// This restructuring splits internal/commands — 129 flat files holding 12 unrelated
// verticals — into one subpackage per vertical, and a later ratchet
// (vertical_isolation_ratchet_test.go) makes
// a vertical importing a sibling vertical a structural failure. shared is the
// landing zone for the handful of helpers 2+ verticals genuinely need
// (WithResultData/ResultData's structured-failure-envelope carrier,
// ResolveProjectSelector, and the cancellation-aware RunGroupCombined/
// RunGroupStreaming subprocess helpers), so those verticals share one
// definition instead of each duplicating it or reopening a cross-vertical edge.
// Extension ownership does not apply: this is CLI-internal plumbing over the
// CLI's own error envelope, workspace model and subprocess lifecycle, nothing a
// third-party extension defines. It cannot fold into internal/commands itself,
// which this same restructuring is dissolving into the 12 vertical packages that need it.
//
// RAISED to 45, which moves the first five
// verticals out of the flat internal/commands package: internal/commands/
// migrate, configcmd, ci, doctor and completion. Each is a package rather
// than a corner of internal/commands for the same reason given for
// shared: internal/commands is the thing being dissolved, one vertical at a
// time, into the package-per-vertical shape ADR-noted above. A symbol two of
// these five (or one of these five and a not-yet-moved flat file) both
// needed moved to shared instead of duplicating (AppendUnique, FileExists,
// AtomicWriteFile, ShortSHA, ParseProjectFlag) rather than growing the
// package count further.
//
// The same change also adds internal/commands/sharedtest, a sibling of shared. Four
// symbols the same five verticals' tests need in common — CaptureStdout,
// DrainCapturedStream, CapturedStream, Contains — exist only to serve
// _test.go callers. Putting them in shared would give a PRODUCTION package a
// *testing.T parameter, pulling "testing" into `go list -deps
// ./cmd/putnami`'s graph and fattening the shipped binary with the test
// runtime. sharedtest holds exactly those four symbols and is imported
// solely by _test.go files, so it — and "testing" with it — never enters
// cmd/putnami's dependency graph, while the five verticals' tests still share
// one definition of this cohesive, deadlock-prone capture utility instead of
// duplicating it or reopening a cross-vertical edge.
//
// RAISED to 50, which moves the next five
// verticals out of the flat internal/commands package: internal/commands/
// sessions, versioncmd, cachecmd, extensions and sdd. Each is a package for
// the same reason given above. A symbol two of these five (or one of these five
// and a not-yet-moved flat file) both needed moved to shared instead of
// duplicating (SameCLIVersion folded into versioncmd once its only two
// callers landed there; GoCommandEnv/FindGoWork, EnsureProjectExtension,
// IsImplicitInstall/WithImplicitInstall, BuildExtensionMap/BuildTemplateMap).
// sharedtest grew the same way for test-only helpers (CaptureStderr,
// WriteLock, WriteJSONConfig, WriteFeatureFixture). Two symbols resisted a
// clean shared/vertical split — extensionsInstall and templatesInstall are
// entry points into a private call graph that stays in internal/commands/
// extensions, so ExtensionsInstallWithWriter/TemplatesInstallWithWriter are
// new thin exported wrappers rather than a move — logged here because they
// are the one place this commit was not a pure file move.
//
// RAISED to 51, which moves the last two (and
// largest) verticals out of the flat internal/commands package:
// internal/commands/agentctx and internal/commands/lifecycle. This one is a
// NET +1, not +2: it also EMPTIES flat internal/commands (down to
// surface_golden_test.go and testdata/, both non-production), which drops
// out of the package count the same slice it is replaced by two. isRegistryArtifactRef
// moved to shared (agentctx's context generation and lifecycle's implicit
// ensure pass both needed it); WriteContextTestExtension/
// ContextTestExtensionManifest moved to sharedtest for the same reason, one
// level down (agentctx's AI-context tests and lifecycle's Install tests).
// lifecycle orchestrates agentctx far more than the plan's cross-vertical
// table anticipated — upgrade/install/init all regenerate the AI context and
// materialize agent workflows as one of their phases — so agentctx exports
// grew accordingly (WriteAgentEntrypoints, DeclaredAgentArtifacts,
// InstallAgentWorkflows, AdoptAgentWorkflows, DescribeAgentWorkflowPlan,
// SplitAgentArtifactReference, SanitizeArtifactSource, ClaudeEntrypointPath,
// and the
// AgentArtifactRef/AgentArtifactResolution/ResolveAgentArtifactPin/
// NewAgentArtifactFetcher stubbing seam) rather than being promoted to
// shared: each is agentctx's own subject matter, not a cross-cutting utility,
// the same distinction drawn above for extensionsInstall/templatesInstall.
// contextGenerate is exported as ContextGenerateWithWriter (not
// ContextGenerate — that name was already taken) for the one lifecycle
// caller, Install, that needs the custom-writer seam neither ContextGenerate
// nor ContextGenerateQuiet exposes. One test file crossed the vertical the
// plan named for it: agent_workflows_lifecycle_test.go reassigns unexported
// package vars on BOTH sides (lifecycle's workspace_init.go install stages
// via stubWorkspaceInitStages, agentctx's fetch/resolve seams), which only
// one package can do from inside a _test.go file, so it lives in lifecycle
// (whose private vars it stubs directly) and reaches agentctx through the
// exports above.
//
// RAISED to 52, the restructuring's last code
// change: internal/commands gains a doc.go (package commands, no logic), so
// the container directory that the previous change had emptied down to a _test.go file
// counts as a production package again. The alternative — deleting the root
// package outright and moving surface_golden_test.go and testdata/surface/
// into internal/cli — was available and would have held the ceiling at 51;
// doc.go was preferred because the import path
// go.putnami.dev/tooling/cli/internal/commands stays meaningful (it is what
// every vertical subpackage's path is rooted under) and the layout that
// replaced the flat package gets a place to be documented, rather than
// dissolving without a trace. This change also adds this file's sibling,
// TestStructuralBaseline_VerticalsStayIsolated
// (vertical_isolation_ratchet_test.go), which is what keeps the twelve
// verticals counted here from re-fusing into each other now that nothing
// forces every new symbol through the compiler's R2 question.
//
// LOWERED to 50, which removes internal/features and
// internal/commands/sdd: the SDD vertical (features, specs, architecture,
// contracts) now ships as the @putnami/sdd extension, which is this program's own
// first question about any surface — "can an extension own this?" — answered
// yes for the first time on a surface that was already built in. The ceiling
// follows the tree down rather than being left where it was: a ceiling two
// above reality is two packages of standing permission nobody reviewed.
//
// RAISED to 51: internal/specgate is core's half of the
// executable-spec verification gate — the policy binding, the observation
// collector, and the join through protocols/features' pure evaluator that the
// engine's post-session finalizer sanctions with. It CANNOT be part of the
// SDD extension already extracted, by that same program's own boundary: the extension judges
// what a run is expected to prove, while collection reads every job's results
// and sanction happens exactly once in core's canonical session reducer
// (fixed decision 9). It is a leaf policy package in the internal/mapgen
// shape — the engine holds only the attachment — not an SDD surface moving
// back into core: it parses no manifest and no spec, only the projection
// artifact and protocol types.
// RAISED to 52: internal/commands/treecmd is the `tree` vertical — the
// content identity of the worktree the CLI runs in. It is a vertical of its own
// rather than a member of an existing one because every candidate host breaks a
// rule the layout already enforces: internal/commands/sessions requires a
// workspace on every path, while `tree fingerprint` must answer inside a
// throwaway repository that has no putnami.json, and fusing the two would put a
// workspace-free surface behind a workspace-bound package's name. The
// calculation itself is NOT here — it lives in internal/git, the git boundary,
// where the session recorder reads it too; this package holds the choice
// between the two output shapes, which is what a vertical is for. It also holds
// `tree verify`, which checks an agent's recorded evidence against that same
// identity and needs no workspace either; it adds no package.
// RAISED to 53: internal/runnersource owns the effectful immutable
// source boundary (bounded worktree capture, private CAS publication and safe
// materialization). It cannot live in protocols/runner, whose wire types must
// remain pure, nor internal/git, whose existing HEAD-bound fingerprint has a
// different identity domain. It imports neither planner nor scheduler and adds
// no execution engine. ADR 0032 records this deliberately separate boundary.
// RAISED to 54 by ADR 0033: internal/sessionreporter is the engine-owned
// lifecycle transport leaf. It cannot be an extension task: reporting must
// survive failed/canceled DAG prerequisites and read the enclosing finalized
// session. Keeping bounded process supervision, durable cursors and credential
// custody here avoids putting transport policy into the engine or disk store.
// Interpretation and downstream effects remain exclusively provider-owned.
// RAISED to 55 (ADR 0032): internal/runnerprovider is the runner
// provider client and session-bundle importer — the transport half of portable
// execution, as internal/cacheprovider is the transport half of the remote
// cache. It spawns and supervises the provider process, transfers missing
// source blobs, follows the attempt, and imports the canonical session bundle
// atomically with schema, identity and digest validation. It contains no
// planner, scheduler or reducer: the engine projects the request and validates
// the expected plan on the executing side, and nothing in this package names
// a jobs execution symbol. It cannot live in the engine (transport and process
// supervision are not run assembly) nor in runnersource (which must stay a
// pure source boundary with no RPC).
// RAISED to 56: internal/sessionstream is the session's one event
// stream — the single producer over events.jsonl, the declared-subscriber
// contract (subscribe from a position, non-blocking wake, read by offset from
// disk, forward-only acknowledgement, final marker) and the subscribers.json
// evidence write. It is a leaf over the standard library and the protocol types,
// because its three consumers cannot share any existing host: the session
// recorder (internal/workspace_state) produces into it, the reporter transport
// (internal/sessionreporter) subscribes live, and the sessions commands read a
// finalized stream. workspace_state imports the planner and scheduler model, so
// hosting the contract there would pull that model into the transport leaf, and
// the engine cannot host it because both workspace_state and sessionreporter sit
// below the engine. It adds no execution stage and no consumer policy.
//
// RAISED 56 → 58 (`putnami compose`): internal/compose owns a
// composition's lifetime — the runsWith closure, per-member reverse proxies,
// per-composition databases, the typed-readiness wait, and the lease the next
// invocation reaps — and internal/commands/composecmd is the vertical that
// chooses what reaches the terminal. The composition cannot live in the engine
// (it supervises long-lived processes the engine deliberately withholds, see
// Request.WithholdServeSteps) nor in internal/watch (a replan trigger, which
// compose uses for its target), and `qualify --target local` consumes it as a
// library, which rules out folding it into the command vertical. It plans
// nothing and schedules nothing: the serve pipelines are planned and their
// finite steps executed by Engine.Run.
//
// RAISED 58 → 60 (ADR 0039): internal/qualify derives a workload's
// smoke contract from its route inventory, runs it against a target and
// reduces the run to one fail-closed verdict, and internal/commands/qualifycmd
// is the `qualify` vertical over it. The runner is a package rather than part
// of the vertical because a second consumer is already contracted: the local
// target composes workloads through internal/compose, and the verdict
// rules must stay testable without a CLI invocation. It is not extension-owned
// for the reason `tree` is not: it names no language, runtime or toolchain,
// reads only protocol documents (http-routes, platform, qualify) and speaks
// plain HTTP, and a verdict that depended on which language extension is
// installed could not be compared across workloads.
//
// RAISED 60 → 61: internal/cli/clitest is the test-only support
// package of the end-to-end packages under internal/cli/e2e — one real CLI
// invocation with both process streams captured, the placement fixture it
// runs in, and the runner conformance harness (the local isolated provider
// the three portable-execution packages re-exec their test binaries as). It
// is the internal/cli counterpart of internal/commands/sharedtest (described above)
// and exists for the same reason: those helpers take a *testing.T, so they
// cannot sit in a production package, and the e2e tests cannot share
// unexported helpers with package cli because each is its own package — which
// is the point, since a package is a test binary and the 35 sessions they run
// no longer hold internal/cli's other tests serial. It is imported by _test.go
// files only and never by cmd/putnami's graph. It adds no vertical and no
// engine stage.
//
// RAISED 61 → 64 (Windows port): three test-only support
// packages, which make the tests that start a program or read the home
// directory run on Windows. internal/fixtureproc places a program that stands
// in for the one the code under test starts (an extension command, a hook, a
// toolchain), so a fixture needs no POSIX shell. internal/fixtureproc/program
// is that program's main package, built once per test process without the
// race detector: a race-instrumented test binary re-executed as the fixture
// spends a second starting on every run. internal/hometest points the home
// directory at a temporary one through HOME and USERPROFILE, the variable
// os.UserHomeDir reads on Windows. They take a *testing.T, so they cannot sit
// in a production package, and the tests of several packages share them. They
// are imported by _test.go files only and never by cmd/putnami's graph. They
// add no vertical and no engine stage.
//
// RAISED 64 → 65: internal/credentialprovider is the client of the
// credential-provider seam (protocols/registry ADR 0002) — the purpose-keyed
// credential an invocation enables with --providers. It resolves the one
// extension declaring the reserved command, supervises that process over its
// RPC, and caches one answer per purpose until its refresh instant. It cannot
// live in internal/extension, which its first consumer (registry downloads)
// sits in: it launches the provider through jobs.PrepareProviderLaunch, and
// internal/jobs imports internal/extension. It cannot live in internal/jobs
// either, whose providers run a task graph; this one only answers bearers. It
// adds no vertical and no engine stage.
//
// RAISED 65 → 66 (ADR 0055): internal/runcredential holds the run credential a
// hosted runner hands the engine on the descriptor --credential-fd names. It
// reads and closes that descriptor before anything else starts, keeps the
// bearer out of every environment and file, and hands it on when the engine
// replaces its own image. internal/launch performs that replacement, so the
// package must stay a leaf that launch can import: it cannot live in
// internal/credentialprovider, which imports internal/jobs and
// internal/extension, nor in internal/launch, whose other callers need none of
// it. It adds no vertical and no engine stage.
const internalPackageCountCeiling = 66

func TestInternalPackageCountCeiling(t *testing.T) {
	t.Parallel()
	pkgs := internalProductionPackages(t)
	if len(pkgs) > internalPackageCountCeiling {
		t.Errorf("internal/ holds %d production packages, ceiling %d — %d added.\n  Packages:\n    %s",
			len(pkgs), internalPackageCountCeiling, len(pkgs)-internalPackageCountCeiling,
			strings.Join(pkgs, "\n    "))
	}
}

// internalProductionPackages returns the sorted module-relative directories
// under internal/ that contain at least one production .go file.
func internalProductionPackages(t *testing.T) []string {
	t.Helper()
	root := moduleRoot(t)
	production, _ := goSources(t, root)
	seen := make(map[string]bool)
	for _, rel := range production {
		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == "internal" || strings.HasPrefix(dir, "internal/") {
			seen[dir] = true
		}
	}
	pkgs := make([]string, 0, len(seen))
	for dir := range seen {
		pkgs = append(pkgs, dir)
	}
	sort.Strings(pkgs)
	return pkgs
}
