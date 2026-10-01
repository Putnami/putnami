// Package dbtestenv owns the database test environment a `putnami test` run
// executes against: policy, discovery, provisioning, the binding artifact the
// test task reads, and the crash-recovery lease a later invocation reclaims by.
//
// An earlier migration moved this out of the CLI. Core used to do all of it: it
// walked each selected test project's committed requirements closure, resolved
// the auto/require/skip policy, shelled out to the docker CLI to run Postgres,
// synthesized a pdb.TestBinding, injected it into the test subprocess through a
// private extraEnv seam, and folded a digest of that binding into the consuming
// job's cache key. Two of those steps are language policy (what a project's
// datasources are, what a test run needs), one is a provider (docker), and one
// was a workaround for the credential traveling through an env var core had to
// hide from its own cache key.
//
// The split this package draws is the one drawn for machine caches and
// C6c drew for infra aggregation:
//
//   - The ORCHESTRATOR owns the lifecycle. Which projects a workload depends on
//     arrives as the job context's project.dependencyClosure; the private
//     invocation tree the credential lives in, the non-secret lease that
//     survives a SIGKILL, the exactly-once finalizer, and the fail-closed leak
//     guard are the generic `invocation` primitive. Core knows
//     none of this is a database.
//
//   - This package owns everything provider-shaped: the policy precedence, the
//     closure merge, the Postgres server's identity, its reuse and reaping, the
//     synthesized binding, and the artifact contract the test task reads.
//
// Two providers implement the resource half (see provider.go). Docker starts a
// container this run owns — the local default. The provided-server provider
// consumes a Postgres the pipeline started and named in PUTNAMI_TEST_PG_URL,
// which is what lets a CI run keep the per-project policy, closure merge,
// artifact and lease it has locally instead of a runner reimplementing them
// around one workspace-union binding injected into every task.
//
// SECURITY. Exactly one file ever holds a credential: the invocation-scoped,
// `sensitive`-declared bindings artifact, which the orchestrator creates at mode
// 0600 inside a 0700 tree and destroys when the invocation ends. Nothing else
// this package writes — the lease, the container name, its label, every docker
// filter, every event, every diagnostic — can carry one, and the types make that
// checkable rather than conventional: Lease has no free-form member and
// serverConfig deliberately excludes the password.
package dbtestenv

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	pdb "go.putnami.dev/protocol/database"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/infra"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/ownerperm"
)

// EnvBindings is the environment variable carrying the resolved pdb.TestBinding
// JSON a test run reads. It MUST stay byte-identical to each language
// extension's own constant and to the manifest env-input name the test task
// declares (`"DATABASE_TEST_BINDINGS": {"from": "env"}`), which is what folds an
// EXTERNALLY supplied binding into the test job's cache key.
const EnvBindings = "DATABASE_TEST_BINDINGS"

// EnvCI is the de-facto standard CI marker set by GitHub Actions, GitLab,
// CircleCI and others. A CI run defaults to `require` and never provisions.
const EnvCI = "CI"

// BindingsArtifact and LeaseArtifact are the invocation-scoped declared output
// paths, relative to the job context's invocation.artifactRoot. Every extension
// that wires this package MUST declare the same two paths, because the two
// halves of the contract — the sensitive one the test task reads and the
// non-secret one the finalizer reclaims by — are what the manifest states.
const (
	BindingsArtifact = "database/bindings.json"
	LeaseArtifact    = "database/lease.json"
)

// ParamInfra and ParamInfraDown are the command parameters that steer policy.
// They are ordinary params, resolved by core with the usual precedence
// (CLI flag > project options > workspace `options.test.infra` > absent), so the
// `--infra` / `--infra-down` flags keep behaving exactly as they did when the
// CLI planner read them itself.
const (
	ParamInfra     = "infra"
	ParamInfraDown = "infra-down"
)

// Outcome is what one Up or Down did.
type Outcome string

const (
	// OutcomeProvisioned means a server was provisioned (or an interchangeable
	// one reused) and the bindings artifact was written.
	OutcomeProvisioned Outcome = "provisioned"
	// OutcomeExternal means an externally supplied DATABASE_TEST_BINDINGS won:
	// nothing was provisioned and nothing was written, so the inherited value
	// flows through untouched.
	OutcomeExternal Outcome = "external"
	// OutcomeNoDatabases means the project's requirements closure declares no
	// database, which is the zero-cost path for a unit-only project.
	OutcomeNoDatabases Outcome = "no-databases"
	// OutcomeNotProvisioned means the policy or the environment forbade
	// provisioning: a non-auto mode, a CI run, or no docker binary.
	OutcomeNotProvisioned Outcome = "not-provisioned"
	// OutcomeTornDown means `--infra-down` reaped every workspace-labeled
	// container and this run provisioned nothing.
	OutcomeTornDown Outcome = "torn-down"
	// OutcomeRetained means the finalizer deliberately left a reusable server
	// running for the next invocation — the session-reuse contract.
	OutcomeRetained Outcome = "retained"
	// OutcomeUnarmed means core delivered no invocation locator, so there is no
	// private tree to write into and nothing to finalize.
	OutcomeUnarmed Outcome = "unarmed"
)

// Result reports what one Up or Down did, in the shape a task result carries.
//
// It is NON-SECRET by construction: every member is an outcome word, a count, a
// policy word, or a digest. There is deliberately no path member and no
// free-form map — a task result travels to the event stream, the session file
// and the cache, and the leak guard failing the task is a worse way to discover
// that than not being able to spell the leak at all.
type Result struct {
	// Outcome is what happened.
	Outcome Outcome
	// Mode is the resolved policy (auto, require, or skip).
	Mode pdb.TestMode
	// Datasources is how many datasources the closure declared.
	Datasources int
	// Digest is the provisioned server's configuration digest — the value the
	// container's label carries. Hex only; never a credential.
	Digest string
	// Diagnostics are advisory findings. Never fatal: a policy the environment
	// cannot satisfy is reported so the test task's own failure has a cause.
	Diagnostics []diag.Diagnostic
}

// Data projects the result into a task's structured result data.
func (r Result) Data() map[string]any {
	return map[string]any{
		"outcome":     string(r.Outcome),
		"mode":        string(r.Mode),
		"datasources": r.Datasources,
		"digest":      r.Digest,
	}
}

// Lease is the non-secret record the producer writes beside the bindings
// artifact so the finalizer — and, after a SIGKILL, a later invocation's
// provider sweep — can reclaim the external resource without reading it.
//
// Like store.InvocationLease it has NO free-form member, and for the same
// reason: a lease is copied onto container labels, resource names and provider
// filters, and a label is a surface a secret may never reach.
type Lease struct {
	// Version is LeaseVersion.
	Version int `json:"version"`
	// InvocationID is the orchestrator's non-secret invocation handle.
	InvocationID string `json:"invocationId"`
	// Provider names the resource kind this lease reclaims.
	Provider string `json:"provider"`
	// Label is the container label key the provider filters on.
	Label string `json:"label"`
	// Digest is the server configuration digest — the label's value.
	Digest string `json:"digest"`
	// Container is the deterministic container name.
	Container string `json:"container"`
	// Retain records whether the finalizer must LEAVE the server running for
	// the next invocation (the session-reuse contract) or tear it down.
	Retain bool `json:"retain"`
}

// LeaseVersion is the lease record's format version. A record that does not
// carry it is not a lease this build wrote and the finalizer leaves it alone.
const LeaseVersion = 1

// providerID names the resource kind a DOCKER lease reclaims. It is a locator,
// not a credential. The provided-server provider has its own
// (providedProviderID), so a lease always states what it is a lease over.
const providerID = "postgres-container"

// Up resolves the policy, provisions when it may, and writes the two
// invocation-scoped artifacts.
//
// The order of the gates is the whole contract, and each one is checked BEFORE
// the next can cost anything:
//
//  1. No invocation locator → unarmed. Nothing to write.
//  2. `--infra-down` → tear down everything this workspace started and
//     provision nothing (only when the mode and environment would have allowed
//     touching the provider at all — a CI or non-auto run still touches docker
//     not at all).
//  3. An externally supplied DATABASE_TEST_BINDINGS always wins. Nothing is
//     provisioned and nothing is written, so the inherited value reaches the
//     test task through the ambient environment and folds into its cache key
//     through the task's own `{"from": "env"}` input.
//  4. A closure declaring no database costs nothing: no provider call, no
//     binding.
//  5. FAIL CLOSED: no run may ever CREATE infrastructure outside the resolved
//     `auto` mode, off CI, with a docker binary on PATH. A server the pipeline
//     provided is not created by this run, so it is usable under `require` and
//     on CI — which is the whole point of naming one — while `skip` still
//     declines. See mayProvision.
func Up(ctx *pctx.Context, prov Provider) Result {
	mode := ResolveMode(ctx, os.Getenv(EnvCI) != "")
	result := Result{Mode: mode}

	root := invocationRoot(ctx)
	if root == "" {
		result.Outcome = OutcomeUnarmed
		return result
	}

	// Explicit teardown: reap everything this workspace labeled and provision
	// nothing. Gated by mayProvision so a CI or non-auto run never reaches the
	// docker CLI through this path either.
	if infraDown(ctx) {
		if !mayProvision(mode, prov) {
			result.Outcome = OutcomeNotProvisioned
			return result
		}
		if err := prov.TeardownAll(); err != nil {
			result.Diagnostics = append(result.Diagnostics, diag.Warningf(
				DiagnosticTeardownFailed, "", "test environment teardown did not complete: %v", err))
		}
		result.Outcome = OutcomeTornDown
		// The lease records the teardown intent so the finalizer sweeps again
		// rather than retaining a server this run was told to remove.
		_ = writeLease(root, Lease{
			Version:      LeaseVersion,
			InvocationID: invocationID(ctx),
			Provider:     prov.id(),
			Label:        prov.LabelKey(),
			Retain:       false,
		})
		return result
	}

	if strings.TrimSpace(os.Getenv(EnvBindings)) != "" {
		result.Outcome = OutcomeExternal
		return result
	}

	databases, diags := ClosureDatabases(ctx)
	result.Diagnostics = append(result.Diagnostics, diags...)
	result.Datasources = len(databases)
	if len(databases) == 0 {
		result.Outcome = OutcomeNoDatabases
		return result
	}

	if !mayProvision(mode, prov) {
		result.Outcome = OutcomeNotProvisioned
		if mode == pdb.TestModeRequire {
			result.Diagnostics = append(result.Diagnostics, diag.Warningf(
				DiagnosticNoProvider, "",
				"test mode %q needs a database this run may not provision; the tests consume the "+
					"%s your pipeline injects, and fail loudly without it", mode, EnvBindings))
		}
		return result
	}

	conn, digest, err := prov.Provision()
	if err != nil {
		// Auto mode is best-effort: a provisioning failure surfaces as "no
		// binding" so the test task decides (it fails under require and skips
		// its live cases otherwise), never as a hard setup failure that would
		// block a project's unit tests on a docker hiccup. A provided server
		// that is unreachable or misconfigured reports its cause the same way,
		// once per task instead of once per suite.
		result.Outcome = OutcomeNotProvisioned
		result.Diagnostics = append(result.Diagnostics, diag.Warningf(
			DiagnosticProvisionFailed, "", "could not provision a test database: %v", err))
		return result
	}
	result.Digest = digest

	// The provider chooses the isolation and reuse its binding carries; the run
	// contributes the resolved mode.
	policy := prov.bindingDefaults()
	policy.Mode = mode
	binding, err := SynthesizeBinding(policy, databases, conn)
	if err != nil {
		result.Outcome = OutcomeNotProvisioned
		result.Diagnostics = append(result.Diagnostics, diag.Errorf(
			DiagnosticInvalidBinding, "", "%v", err))
		return result
	}
	if err := writeBindings(root, binding); err != nil {
		result.Outcome = OutcomeNotProvisioned
		result.Diagnostics = append(result.Diagnostics, diag.Errorf(
			DiagnosticInvalidBinding, "", "write test binding: %v", err))
		return result
	}
	if err := writeLease(root, Lease{
		Version:      LeaseVersion,
		InvocationID: invocationID(ctx),
		Provider:     prov.id(),
		Label:        prov.LabelKey(),
		Digest:       digest,
		Container:    prov.ContainerName(digest),
		// RETAIN is the session-reuse contract: a still-running server whose
		// config digest matches is interchangeable across invocations, and
		// tearing it down at the end of every `putnami test` would trade a
		// container start (image pull included) for nothing. Its recovery is
		// not a timer either — the next Up reaps every digest-mismatched
		// container before it reuses or starts one. A provided server retains
		// for a stronger reason: this run never created it.
		Retain: true,
	}); err != nil {
		result.Diagnostics = append(result.Diagnostics, diag.Warningf(
			DiagnosticLeaseFailed, "", "write test environment lease: %v", err))
	}
	result.Outcome = OutcomeProvisioned
	return result
}

// Down is the finalizer: it reads the lease and does exactly what it says.
//
// A retained lease is the ordinary run: the server stays up for the next
// invocation and this reports `retained`. A lease with retain:false is the
// `--infra-down` path, and the sweep is idempotent — Up already tore everything
// down, and a second pass costs one `docker ps` and catches whatever a
// concurrent run started in between. No lease at all means the producer
// provisioned nothing, so there is nothing to finalize.
//
// The private artifact tree itself — the credential included — is destroyed by
// the orchestrator when the invocation ends, whatever this returns.
func Down(ctx *pctx.Context, prov Provider) Result {
	root := invocationRoot(ctx)
	if root == "" {
		return Result{Outcome: OutcomeUnarmed}
	}
	lease, ok := ReadLease(root)
	if !ok {
		return Result{Outcome: OutcomeNoDatabases}
	}
	if lease.Retain {
		return Result{Outcome: OutcomeRetained, Digest: lease.Digest}
	}
	result := Result{Outcome: OutcomeTornDown, Digest: lease.Digest}
	if err := prov.TeardownAll(); err != nil {
		result.Diagnostics = append(result.Diagnostics, diag.Warningf(
			DiagnosticTeardownFailed, "", "test environment teardown did not complete: %v", err))
	}
	return result
}

// Diagnostic codes. They are advisory: this task never fails a build, because a
// test environment that could not be provisioned is a fact the TEST task
// reports (it fails under `require` and skips its live cases otherwise), and
// failing setup as well would report the same problem twice with the less
// useful message.
const (
	DiagnosticNoProvider      = "test_env.no_provider"
	DiagnosticProvisionFailed = "test_env.provision_failed"
	DiagnosticInvalidBinding  = "test_env.invalid_binding"
	DiagnosticTeardownFailed  = "test_env.teardown_failed"
	DiagnosticLeaseFailed     = "test_env.lease_failed"
)

// ResolveMode resolves the pdb.TestMode policy with the precedence the CLI
// planner used to apply:
//
//	--infra flag  >  project/workspace `test.infra` option  >  default
//
// The first two arrive already merged on the job context's params (core resolves
// CLI flags over project options over workspace `options.test.infra`), so this
// reads ONE value and adds the environment default: `require` on CI — fail
// loudly rather than provision — and `auto` locally.
//
// An unrecognized value falls through to the default rather than erroring, so a
// typo degrades to the safe policy instead of breaking the run.
func ResolveMode(ctx *pctx.Context, ci bool) pdb.TestMode {
	if ctx != nil {
		if mode := pdb.TestMode(strings.TrimSpace(ctx.Params.String(ParamInfra))); mode.Valid() {
			return mode
		}
	}
	if ci {
		return pdb.TestModeRequire
	}
	return pdb.TestModeAuto
}

// mayProvision reports whether this run may use its provider at all. Each
// condition is cheap and none of them is a daemon or network call.
//
// The gate is fail-closed about ONE thing: CREATING infrastructure. Only the
// resolved auto mode, off CI, with a docker binary on PATH, may start a server
// — that is what keeps a CI run from silently depending on a container someone
// left running, and it is unchanged.
//
// A server the pipeline PROVIDED is a different fact. This run did not create
// it, cannot leak it and cannot leave it behind, so the two conditions that
// exist to stop a run from starting infrastructure do not apply: `require` — the
// CI default, and the mode whose whole meaning is "a database must be there" —
// uses it, and so does `auto`. `skip` still declines, because a run that asked
// for no database gets none.
func mayProvision(mode pdb.TestMode, prov Provider) bool {
	if prov == nil {
		return false
	}
	if prov.ownsServer() {
		// The unchanged docker gate, in its original order: the two free
		// checks first, so a CI or non-auto run does not even pay for the PATH
		// lookup below.
		if mode != pdb.TestModeAuto || os.Getenv(EnvCI) != "" {
			return false
		}
	} else if mode == pdb.TestModeSkip {
		return false
	}
	return prov.available()
}

// infraDown reports whether the run asked for an explicit teardown.
func infraDown(ctx *pctx.Context) bool {
	return ctx != nil && ctx.Params.Bool(ParamInfraDown, false, "infraDown")
}

// ClosureDatabases returns the merged, deduplicated database requirements the
// project's dependency closure declares in its committed
// infra/requirements.json manifests, sorted by (name, engine) for determinism.
//
// The closure is the orchestrator's answer (project.dependencyClosure), the same
// set the infra-aggregation task walks, so provisioning and the deployability
// manifest can never disagree about what a workload needs. A missing manifest is
// not an error; a malformed one yields a diagnostic and contributes nothing.
func ClosureDatabases(ctx *pctx.Context) ([]infra.Database, []diag.Diagnostic) {
	if ctx == nil {
		return nil, nil
	}
	closure := ctx.Project.DependencyClosure
	if len(closure) == 0 {
		closure = []pctx.ProjectRef{{
			Name:     ctx.Project.Name,
			Path:     ctx.Project.Path,
			FullPath: ctx.Project.FullPath,
		}}
	}

	type key struct {
		name   string
		engine infra.Engine
	}
	seen := make(map[key]bool)
	var out []infra.Database
	var diags []diag.Diagnostic

	for _, member := range closure {
		root := projectRoot(ctx.WorkspaceRoot, member.Path, member.FullPath)
		if root == "" {
			continue
		}
		path := infra.ProjectRequirementsPath(root)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		m, d := infra.LoadPerProjectManifest(path)
		if diag.HasErrors(d) {
			for i := range d {
				d[i].Message = fmt.Sprintf("project %s: %s", member.Name, d[i].Message)
			}
			diags = append(diags, d...)
			continue
		}
		for _, db := range m.Databases {
			k := key{name: db.Name, engine: db.Engine}
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, db)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Engine < out[j].Engine
	})
	return out, diags
}

// The pool a TEST datasource gets, stated as the pgx connection parameters the
// runtime already honors (go.putnami.dev/database reads them off the DSN, and a
// connection that states one wins over the framework's struct default).
//
// The framework's service defaults — 10 connections per datasource, held idle
// for 30 minutes — are right for a long-lived service and wrong for a test
// binary. A workload declaring 24 datasources opens 240 connections to use one
// or two at a time, which exhausts a stock Postgres (max_connections 100) and
// forces a whole pipeline to serialize its test tasks around a limit most of
// them never touch.
//
// TWO, not one: a test that holds a transaction on one connection and runs a
// query beside it needs a second, and a pool of 1 turns that into a deadlock
// rather than a wait. ZERO minimum: a pool that maintains no warm connection
// costs nothing for the datasources a suite never touches — and a test binary
// touches most of its closure's datasources never. FIVE SECONDS idle: the size
// caps what a run may hold at once, the idle bound is what gives it back, and
// without it the cap is a floor for the whole run.
//
// A number a caller states explicitly still wins: these fill only what the
// connection left unsaid, exactly as the framework's own precedence does.
const (
	testPoolMaxConns     = "2"
	testPoolMinConns     = "0"
	testPoolMaxConnIdle  = "5s"
	paramPoolMaxConns    = "pool_max_conns"
	paramPoolMinConns    = "pool_min_conns"
	paramPoolMaxConnIdle = "pool_max_conn_idle_time"
)

// withTestPoolDefaults returns the connection with the test-pool parameters
// filled in. A key the connection already carries is kept — that is how
// PUTNAMI_TEST_PG_URL (whose query becomes these params) overrides a default it
// disagrees with, per key rather than all-or-nothing.
//
// It builds a new map rather than filling the caller's in place: the provider's
// connection is handed back once and mapped onto every datasource, so mutating
// it would make this binding's defaults a property of the provider.
func withTestPoolDefaults(conn pdb.Connection) pdb.Connection {
	defaults := map[string]string{
		paramPoolMaxConns:    testPoolMaxConns,
		paramPoolMinConns:    testPoolMinConns,
		paramPoolMaxConnIdle: testPoolMaxConnIdle,
	}
	params := make(map[string]string, len(conn.Params)+len(defaults))
	for key, value := range defaults {
		params[key] = value
	}
	for key, value := range conn.Params {
		params[key] = value
	}
	conn.Params = params
	return conn
}

// SynthesizeBinding builds the per-project pdb.TestBinding (Postgres-only in v1)
// by mapping every declared datasource onto the provisioned server connection,
// then validates it through the protocol's own strict parser so an invalid
// binding can never reach a test run. It returns the canonical JSON.
//
// Per-datasource database/schema isolation is the RUNTIME provider's job (the
// framework's test provider creates an isolated database or schema per suite
// from this binding), which is why every datasource maps onto the same physical
// server here. What this package decides is the POLICY that job runs under, and
// it is the provider's choice: an empty Isolation or Reuse is omitted from the
// JSON, leaving the runtime provider's own defaults in force.
//
// The pool a test datasource gets is decided here too, and for both providers:
// this is the one place every emitted binding passes through, and the reason to
// bound a test pool does not depend on who started the server (see
// withTestPoolDefaults).
func SynthesizeBinding(policy TestPolicy, databases []infra.Database, conn pdb.Connection) (string, error) {
	conn = withTestPoolDefaults(conn)
	tb := pdb.TestBinding{
		ProtocolVersion: pdb.ProtocolVersion,
		Mode:            policy.Mode,
		Isolation:       policy.Isolation,
		Reuse:           policy.Reuse,
		Databases:       make(map[string]pdb.Database, len(databases)),
	}
	for _, db := range databases {
		if db.Engine != infra.EnginePostgres {
			return "", fmt.Errorf("datasource %q engine %q is not supported (postgres-only in v1)",
				db.Name, db.Engine)
		}
		physical := conn
		tb.Databases[db.Name] = pdb.Database{
			Engine:     pdb.EnginePostgres,
			Schema:     firstSchema(db.Schemas),
			Connection: &physical,
		}
	}
	raw, err := json.Marshal(tb)
	if err != nil {
		return "", fmt.Errorf("marshal test binding: %w", err)
	}
	if _, diags := pdb.ParseAndValidateTestBinding(raw); diag.HasErrors(diags) {
		return "", fmt.Errorf("synthesized test binding is invalid: %s", diagMessages(diags))
	}
	return string(raw), nil
}

// BindingFrom reads the invocation-scoped bindings artifact a test task must
// run against, or ("", false) when this run provisioned none.
//
// It is the ONLY way the credential reaches a test task, and it is deliberately
// a file read rather than an environment variable: an env var is inherited by
// every descendant process and shows up in `ps`, while the artifact lives at
// mode 0600 in a 0700 tree the orchestrator destroys when the invocation ends.
// An EXTERNALLY supplied binding is not this — it stays in the ambient
// environment, where the test task's own cache-key input can see it.
func BindingFrom(ctx *pctx.Context) (string, bool) {
	root := invocationRoot(ctx)
	if root == "" {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(BindingsArtifact)))
	if err != nil {
		return "", false
	}
	// The producer RESERVES the artifact at 0600 before it runs, so an empty
	// file is "nothing was provisioned", not "an empty binding".
	binding := strings.TrimSpace(string(data))
	if binding == "" {
		return "", false
	}
	return binding, true
}

// ReadLease reads the non-secret lease from an invocation artifact root. A
// record without the current version is not one this build wrote and is
// reported absent, so a forward-format lease is left alone rather than acted on.
func ReadLease(artifactRoot string) (Lease, bool) {
	data, err := os.ReadFile(filepath.Join(artifactRoot, filepath.FromSlash(LeaseArtifact)))
	if err != nil {
		return Lease{}, false
	}
	var lease Lease
	if json.Unmarshal(data, &lease) != nil || lease.Version != LeaseVersion {
		return Lease{}, false
	}
	return lease, true
}

// writeBindings writes the credential into the reserved sensitive artifact.
//
// It writes IN PLACE (O_TRUNC on the existing file) rather than through a
// temp-file rename, because the orchestrator pre-created that file at mode 0600
// and a rename would replace it with one created at the process umask. Before
// the credential is written, the file is restricted to its owner: mode 0600,
// and on Windows, where the mode bits do not keep other users out, a DACL
// that grants the current user alone. The orchestrator re-asserts the mode
// afterwards; not needing it to is better.
func writeBindings(artifactRoot, binding string) error {
	path := filepath.Join(artifactRoot, filepath.FromSlash(BindingsArtifact))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := ownerperm.Restrict(path, 0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.WriteString(binding); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// writeLease publishes the non-secret lease beside the credential.
func writeLease(artifactRoot string, lease Lease) error {
	path := filepath.Join(artifactRoot, filepath.FromSlash(LeaseArtifact))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(lease)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// invocationRoot returns the private artifact root core delivered, or "" when
// this node participates in no finalizes relation.
func invocationRoot(ctx *pctx.Context) string {
	if ctx == nil || ctx.Invocation == nil {
		return ""
	}
	return strings.TrimSpace(ctx.Invocation.ArtifactRoot)
}

// invocationID returns the non-secret invocation handle, or "".
func invocationID(ctx *pctx.Context) string {
	if ctx == nil || ctx.Invocation == nil {
		return ""
	}
	return ctx.Invocation.ID
}

// projectRoot resolves a project reference to an absolute directory. The
// context's absolute path is authoritative when present; the workspace-relative
// one is the fallback, so a reference that carries only a path still resolves.
func projectRoot(workspaceRoot, path, fullPath string) string {
	if fullPath != "" {
		return fullPath
	}
	if path == "" {
		return workspaceRoot
	}
	return filepath.Join(workspaceRoot, path)
}

// firstSchema returns the owning schema for a datasource, defaulting to "public"
// (Postgres requires a schema on every binding entry).
func firstSchema(schemas []string) string {
	for _, s := range schemas {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return "public"
}

// diagMessages joins error diagnostic messages for an error string.
func diagMessages(diags []diag.Diagnostic) string {
	errs := diag.Errors(diags)
	msgs := make([]string, 0, len(errs))
	for _, d := range errs {
		msgs = append(msgs, d.Message)
	}
	return strings.Join(msgs, "; ")
}

// digestOf is the stable, canonical identity of a serializable value: a sha256
// over its JSON encoding, truncated to 16 hex characters. Hex only, so no
// credential substring can ever appear in a value derived through it.
func digestOf(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}
