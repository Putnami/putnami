package codegen

import (
	"bytes"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rogpeppe/go-internal/lockedfile"
	"go.putnami.dev/go/extension/internal/codegen/openapiutil"
	"go.putnami.dev/go/extension/internal/jobs/configextract"
	"go.putnami.dev/go/extension/internal/toolchain"
	protocfg "go.putnami.dev/protocol/config"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/genresult"
	"go.putnami.dev/sdk/extension/jsonl"
)

// describeMaxRuntime caps how long we wait on the describe binary. The cap
// bounds a binary that hangs in Configure.
const describeMaxRuntime = 60 * time.Second

// describePipeDrainDelay bounds how long Cmd.Wait may spend copying output
// after the describe process exits. A spawned descendant can inherit stdout or
// stderr and keep the pipe open after ready-marker or timeout termination; a
// short grace period retains already-buffered output without letting that
// descendant stall the build indefinitely.
const describePipeDrainDelay = 250 * time.Millisecond

// readyMarker is the framework's "start successful" log line, emitted by
// app.Application.Start once every Starter plugin has bound. Seeing it under
// PUTNAMI_DESCRIBE means the binary did NOT honor the env var (almost always
// because it was built against an older go.putnami.dev/app) and is now binding
// real listeners — abort the run immediately.
const readyMarker = "🤖 ready"

// describeOutputWriter captures the describe process's merged stdout/stderr
// stream and watches it for readyMarker. os/exec may copy the two streams from
// separate goroutines, so every operation is protected even though assigning
// the same writer to both Cmd.Stdout and Cmd.Stderr currently serializes writes.
//
// The marker search includes only the new bytes plus the longest possible
// prefix retained from the preceding write. This detects markers split across
// Write calls without repeatedly scanning the full, intentionally unbounded
// output buffer.
type describeOutputWriter struct {
	mu        sync.Mutex
	output    bytes.Buffer
	readyCh   chan<- struct{}
	readyOnce sync.Once
}

func newDescribeOutputWriter(readyCh chan<- struct{}) *describeOutputWriter {
	return &describeOutputWriter{readyCh: readyCh}
}

func (w *describeOutputWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	searchStart := w.output.Len() - len(readyMarker) + 1
	if searchStart < 0 {
		searchStart = 0
	}
	n, err := w.output.Write(p)
	if n > 0 && bytes.Contains(w.output.Bytes()[searchStart:], []byte(readyMarker)) {
		w.readyOnce.Do(func() {
			select {
			case w.readyCh <- struct{}{}:
			default:
			}
		})
	}
	return n, err
}

func (w *describeOutputWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.output.String()
}

// frameworkAppPath is the module path we look for in the binary's buildinfo
// to surface a useful "you need a newer version" error message when the
// describe run fails.
const frameworkAppPath = "go.putnami.dev/app"

// GeneratedClientOutputPort is the data output port build-describe reports the
// project-relative generated-client directories on. The manifest declares a port
// of this exact name (tasks.build-describe.outputs), and both the CLI scheduler
// and the v3 task declaration read the directory back under it — a generated
// client's location is chosen by the project's clientgen config, so this port is
// the only way the v3 task contract can name it (a declared output's pathFrom).
// Renaming it on either side silently stops the client from being captured, so
// the name is a constant the manifest-contract test pins.
const GeneratedClientOutputPort = "clientOutputs"

// RunDescribe is the entry point for the build-describe job. It compiles a
// host-platform binary of the project, runs it once with PUTNAMI_DESCRIBE
// set, and copies the artifacts written under .gen/ into the project tree
// (unless the project opted out via options.generate.schema=false).
//
// Describe mode is the canonical full-fidelity build-time generator: every
// runtime plugin that implements app.Describer contributes, so adding a new
// generator is a matter of implementing one method on a plugin — not editing
// this job. The static codegen runner remains for AST-only artifacts.
func RunDescribe(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	emit.PhaseStart("describe")
	res, err := runDescribe(ctx, emit)
	if err != nil {
		emit.PhaseEnd("describe", "failed")
		return "FAILED", nil, err
	}
	if res == nil {
		emit.PhaseEnd("describe", "skipped")
		return "SKIP", nil, nil
	}
	emit.PhaseEnd("describe", "success")
	// Both values are project-relative. The payload is stored in this task's
	// cache entry and digested by `cache verify`, so anything checkout- or
	// run-specific in it makes a task the manifest declares `deterministic`
	// disagree with itself between two equivalent runs. That is why the
	// host binary is not reported: it lives at an absolute machine-local path —
	// the project's slot under this worktree's scratch root, or a throwaway
	// temporary file when there is none — so no two checkouts could ever agree
	// on the name, and nothing reads it.
	return "OK", map[string]any{
		"artifacts":               res.Artifacts,
		GeneratedClientOutputPort: res.ClientOutputs,
	}, nil
}

// describeResult carries the outcome for the manifest.
type describeResult struct {
	Artifacts     []string `json:"artifacts"`
	ClientOutputs []string `json:"clientOutputs,omitempty"`
}

func runDescribe(ctx *pctx.Context, emit *jsonl.Emitter) (*describeResult, error) {
	projectPath := ctx.Project.FullPath
	if projectPath == "" {
		projectPath = filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)
	}
	if !hasMain(projectPath) {
		emit.Log("info", "describe: no main package found, skipping")
		return nil, nil
	}
	if !usesPutnamiApp(projectPath) {
		// Describe mode only applies to apps built on go.putnami.dev/app
		// — CLIs and other Go binaries don't honor PUTNAMI_DESCRIBE and
		// would either exit non-zero or do nothing useful.
		emit.Log("info", "describe: project does not import go.putnami.dev/app, skipping")
		return nil, nil
	}

	genDir := filepath.Join(projectPath, ".gen")
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		return nil, fmt.Errorf("prepare .gen: %w", err)
	}

	// Per-project lock: build/test pipelines both schedule build~describe
	// against the same project and the orchestrator can run them in parallel.
	// Two simultaneous runs would race on the binary's .gen/ writes and the
	// shared manifest; serialize them so the second run reuses the first
	// run's artifacts (the snapshot diff makes the second run a no-op for the
	// commit step).
	unlock, err := acquireDescribeLock(genDir)
	if err != nil {
		return nil, fmt.Errorf("acquire describe lock: %w", err)
	}
	defer unlock()

	emit.Progress(1, 4, "Compiling host binary")
	binPath, releaseBinary, err := compileHostBinary(ctx, projectPath)
	if err != nil {
		return nil, fmt.Errorf("compile host binary: %w", err)
	}
	defer releaseBinary()

	// Best-effort framework version probe — used to enrich error messages if
	// the binary doesn't honor PUTNAMI_DESCRIBE. A failure here just means we
	// emit a less helpful error later, not that we abort the run.
	fwVersion, _ := readFrameworkVersion(binPath)

	emit.Progress(2, 4, "Running describe mode")

	// Reset .gen/schema to build-generate's static output before anything
	// observes that tree. Generate CEDES .gen/schema to this task, so a generate
	// cache hit restores a .gen without it; rebuilding it from the copy generate
	// does own makes this run's starting tree identical whether generate executed
	// or was restored, AND drops whatever an earlier build left there.
	if err := resetCededSchemaToGenerateStaging(genDir); err != nil {
		return nil, fmt.Errorf("reset staged schema: %w", err)
	}

	// Snapshot what's already under .gen/schema/ (e.g. files the static
	// codegen runner produced earlier in the same build) so we only count
	// files describe itself wrote and don't double-list them in the manifest.
	before := snapshotSchemaDir(filepath.Join(genDir, schemaSubdir))

	cmdOut, runErr := runDescribeBinary(binPath, projectPath, genDir, fwVersion, describeMaxRuntime)
	if runErr != nil {
		// Surface the binary's own diagnostics so users can fix their app.
		if cmdOut != "" {
			emitCapabilityDiagnostics(emit, cmdOut)
			emit.Log("error", strings.TrimSpace(cmdOut))
		}
		return nil, fmt.Errorf("describe run: %w", runErr)
	}

	emit.Progress(3, 4, "Copying artifacts")
	commitSchemas := configextract.ProjectWantsCommittedSchemas(ctx)
	// Files build-generate staged under .gen/ this build but deliberately did
	// not commit (because describe will): describe must promote them to the
	// tracked tree even if the describe binary never rewrote them — e.g. an app
	// with HTTP routes but no runtime OpenAPI plugin keeps build-generate's
	// static stub, which is still the converged value.
	staged := stagedSchemaArtifacts(projectPath)
	artifacts, err := collectArtifacts(projectPath, genDir, commitSchemas, before, staged)
	if err != nil {
		return nil, fmt.Errorf("collect artifacts: %w", err)
	}
	// Mirror any typed clients the describe binary staged under .gen/clientgen
	// into the project tree (clients/<lang>). The scheduler captures that tree
	// under the "clients" cache resource so a describe cache hit rematerializes
	// it without re-running. Independent of the committed-schema opt-out: the
	// generated client is required for downstream compilation, not a doc artifact.
	clientArtifacts, clientOutputs, err := mirrorGeneratedClients(projectPath, genDir)
	if err != nil {
		return nil, fmt.Errorf("mirror generated clients: %w", err)
	}
	artifacts = append(artifacts, clientArtifacts...)
	// Nothing the describe binary wrote under .gen needs staging here. Every
	// subtree this task produces there — schema/, clientgen/, design/,
	// migrations.json, migration-bundle/ — is a subpath build-generate cedes and
	// this task declares, so the scheduler captures and restores each one from
	// where its producer wrote it, at the contract path consumers read.

	// Fold dependency-owned config blocks (emitted by the describe binary as
	// .gen/config-deps.json) into the workload's published schema/config.json,
	// before infra requirements are synced so library-owned secrets reach
	// committed infra/requirements.json.
	if err := mergeDependencyConfigSchema(ctx, projectPath, genDir, commitSchemas); err != nil {
		return nil, fmt.Errorf("merge dependency config: %w", err)
	}
	// build~generate already cleared stale scratch fragments for this build.
	// Do not clear here: config/AST generators may have written fragments that
	// must be folded together with Describe() output.
	syncGeneratedInfraRequirements(projectPath, emit)

	emit.Progress(4, 4, "Updating manifest")
	if err := mergeIntoManifest(projectPath, artifacts); err != nil {
		emit.Log("warn", "manifest merge failed: "+err.Error())
	}

	emit.Log("info", fmt.Sprintf("describe: %d artifact(s)", len(artifacts)))
	return &describeResult{Artifacts: artifacts, ClientOutputs: clientOutputs}, nil
}

// capabilitiesSchemaRelPath is the canonical project-relative capability
// manifest path (mirrors go.putnami.dev/protocol/capabilities.CommittedPath;
// kept as a local literal for the same reason openapiutil.SchemaRelPath is).
const capabilitiesSchemaRelPath = "schema/capabilities.json"

// httpRoutesSchemaRelPath is the canonical project-relative HTTP route
// inventory path (mirrors go.putnami.dev/http.HTTPRoutesDescribePath without
// making the extension depend on the framework module).
const httpRoutesSchemaRelPath = "schema/http-routes.json"

// apiProtoSchemaRelPath is the canonical project-relative protobuf descriptor
// path (mirrors go.putnami.dev/proto.DefaultOutputPath without making the
// extension depend on the framework module).
const apiProtoSchemaRelPath = "schema/api.proto"

// isCapabilitiesSchemaPath reports whether rel points at the capability
// manifest, which the app's describe binary is the sole producer of.
func isCapabilitiesSchemaPath(rel string) bool {
	rel = strings.TrimPrefix(strings.ReplaceAll(rel, "\\", "/"), "./")
	return rel == capabilitiesSchemaRelPath
}

// isHTTPRoutesSchemaPath reports whether rel points at the route inventory,
// which only the app's describe binary produces for Go projects.
func isHTTPRoutesSchemaPath(rel string) bool {
	rel = strings.TrimPrefix(strings.ReplaceAll(rel, "\\", "/"), "./")
	return rel == httpRoutesSchemaRelPath
}

// isAPIProtoSchemaPath reports whether rel points at the protobuf descriptor of
// the app's API, which only the app's describe binary produces (the proto
// plugin writes it during describe).
func isAPIProtoSchemaPath(rel string) bool {
	rel = strings.TrimPrefix(strings.ReplaceAll(rel, "\\", "/"), "./")
	return rel == apiProtoSchemaRelPath
}

// isFeatureEvidencePath reports whether rel is a feature-evidence artifact
// (schema/feature-evidence/*.json). Like capabilities.json, these have a
// single producer — the app's describe binary (go/framework/app
// capabilities.go writes both) — so a differing prior copy in .gen/ is
// describe's own stale output from an earlier build, never a competing
// producer.
func isFeatureEvidencePath(rel string) bool {
	rel = strings.TrimPrefix(strings.ReplaceAll(rel, "\\", "/"), "./")
	return strings.HasPrefix(rel, "schema/feature-evidence/") && strings.HasSuffix(rel, ".json")
}

var capabilityDiagnosticPattern = regexp.MustCompile(`\[((?:capabilities\.(?:parse_error|unknown_field|invalid_protocol_version|missing_project|invalid_name|missing_provenance|invalid_source_kind|invalid_schema_kind|invalid_discoverer_kind|invalid_infra_kind|invalid_probe|invalid_phase|invalid_capability_kind|missing_requires|missing_required_provider|duplicate_provider|conflicting_provider))|(?:http_routes\.[a-z_]+))\]\s+([^:\r\n]+):\s*([^\r\n]+)`)

// emitCapabilityDiagnostics promotes the protocol validator's stable frames
// from the described application's output into runtime diagnostic events. The
// build CLI can then render and retain the protocol code instead of reducing a
// failed describe to an unstructured process-exit message.
func emitCapabilityDiagnostics(emit *jsonl.Emitter, output string) {
	matches := capabilityDiagnosticPattern.FindAllStringSubmatch(output, -1)
	for _, match := range matches {
		field := strings.TrimSpace(match[2])
		message := strings.TrimSpace(match[3])
		if field != "" {
			message = field + ": " + message
		}
		emit.DiagnosticWithCode("error", message, "", 0, 0, match[1])
	}
}

// configDepsFragmentName mirrors app.configDepsFragment: the file the describe
// binary writes dependency-owned config blocks to under .gen/. Kept in sync by
// convention — the framework owns the producer, this owns the consumer.
const configDepsFragmentName = "config-deps.json"

// configDepsDocument mirrors the framework's on-disk fragment shape.
type configDepsDocument struct {
	Blocks []protocfg.Block `json:"blocks"`
}

// mergeDependencyConfigSchema is the SOLE committer of the workload's published
// schema/config.json for app projects. It re-extracts
// the workload's own config blocks, unions them with the dependency-owned blocks
// the describe binary emitted (.gen/config-deps.json), and writes the converged
// schema plus its JSON Schema companion and infra secrets sidecar.
//
// It ALWAYS writes, even when the fragment is missing or empty. build-generate
// no longer commits the config schema for these projects (it only stages the
// own blocks to .gen/config-schema.json), so describe must produce the committed
// file: own+plugin blocks when dependencies contribute config, own-only blocks
// otherwise. That makes describe the single writer of schema/config.json, so a
// late-scheduled build~generate / test~generate can no longer clobber the merge
// and drop a plugin-contributed block (e.g. the `events` block) under --impacted.
//
// A workload with no config at all still gets no committed schema:
// MergeDependencyBlocks -> writeSchemaFromBlocks returns ok=false for zero total
// blocks, removing the stale .gen fallback and leaving the tracked tree untouched.
func mergeDependencyConfigSchema(ctx *pctx.Context, projectPath, genDir string, commit bool) error {
	depBlocks, err := readConfigDepsFragment(genDir)
	if err != nil {
		return err
	}
	outputPath := configextract.DefaultOutputPath
	if !commit {
		outputPath = configextract.FallbackOutputPath
	}
	_, _, err = configextract.MergeDependencyBlocks(projectPath, ctx.Project.Name, configSchemaVersion(ctx, projectPath, outputPath), outputPath, depBlocks)
	return err
}

// readConfigDepsFragment reads the describe binary's dependency-config fragment.
// A missing fragment is not an error (the app contributed no library config);
// it returns nil blocks.
func readConfigDepsFragment(genDir string) ([]protocfg.Block, error) {
	data, err := os.ReadFile(filepath.Join(genDir, configDepsFragmentName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc configDepsDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", configDepsFragmentName, err)
	}
	return doc.Blocks, nil
}

// usesPutnamiApp requires an authored import of the framework app in this
// module. A generated client also imports app for its optional DI registration,
// but that does not make the project's executable a describable application.
// Local helper packages count: a main package may delegate app construction.
func usesPutnamiApp(projectPath string) bool {
	data, err := os.ReadFile(filepath.Join(projectPath, "go.mod"))
	if err != nil {
		return false
	}
	requiresApp := false
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		// Match both `require go.putnami.dev/app vX.Y.Z` and entries
		// inside a `require ( … )` block.
		if len(fields) >= 2 && fields[0] == "go.putnami.dev/app" ||
			len(fields) >= 3 && fields[0] == "require" && fields[1] == "go.putnami.dev/app" {
			requiresApp = true
			break
		}
	}
	if !requiresApp {
		return false
	}
	found := false
	err = filepath.WalkDir(projectPath, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if filePath != projectPath {
				if _, statErr := os.Stat(filepath.Join(filePath, "go.mod")); statErr == nil {
					return filepath.SkipDir
				} else if !errors.Is(statErr, os.ErrNotExist) {
					return statErr
				}
			}
			if filePath != projectPath && (strings.HasPrefix(entry.Name(), ".") ||
				entry.Name() == "vendor" || entry.Name() == "node_modules" || entry.Name() == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(filePath, ".go") || strings.HasSuffix(filePath, "_test.go") {
			return nil
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), filePath, nil, parser.ImportsOnly|parser.ParseComments)
		if parseErr != nil {
			return parseErr
		}
		if ast.IsGenerated(parsed) {
			return nil
		}
		for _, imported := range parsed.Imports {
			path, unquoteErr := strconv.Unquote(imported.Path.Value)
			if unquoteErr != nil {
				return unquoteErr
			}
			if path == "go.putnami.dev/app" {
				found = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	// An unreadable source tree must not silently suppress a required describe.
	return found || err != nil
}

// describeWillCommit reports whether the describe phase will run for projectPath
// and therefore become the sole committer of its converged schema sidecars.
// It is the exact conjunction runDescribe gates on (a main package built on
// go.putnami.dev/app), evaluated cheaply from the project tree so the
// build-generate job can call it too.
//
// When this is true, build-generate must NOT write the committed sidecars
// itself: it stages them to .gen/ and lets describe write the converged result
// to the tracked tree exactly once. That keeps the committed file atomic over
// the two-phase pipeline — a skipped/canceled/cached describe can no longer
// strand a strictly-degraded build-generate intermediate (the static OpenAPI
// stub, a deleted requirements.json) in the working tree.
func describeWillCommit(projectPath string) bool {
	return hasMain(projectPath) && usesPutnamiApp(projectPath)
}

// hasMain returns true when projectPath has a top-level main package or a
// cmd/<name>/main.go layout. Library projects have neither and skip describe.
func hasMain(projectPath string) bool {
	matches, _ := filepath.Glob(filepath.Join(projectPath, "*.go"))
	for _, p := range matches {
		if isMainPackage(p) {
			return true
		}
	}
	cmdEntries, _ := os.ReadDir(filepath.Join(projectPath, "cmd"))
	for _, e := range cmdEntries {
		if !e.IsDir() {
			continue
		}
		mains, _ := filepath.Glob(filepath.Join(projectPath, "cmd", e.Name(), "*.go"))
		for _, p := range mains {
			if isMainPackage(p) {
				return true
			}
		}
	}
	return false
}

// isMainPackage does a cheap text check for `package main` so we don't need
// to spin up a go/parser just to gate on whether the project is buildable
// as an executable.
func isMainPackage(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		return strings.HasPrefix(trimmed, "package main")
	}
	return false
}

// describeHostBinaryDir is the segment under the job's per-workspace scratch
// root that holds one reusable describe host binary per project, and
// describeHostBinaryName is the file inside it. The shape mirrors the
// TypeScript extension's incremental tsc state (<cacheRoot>/ts-types/...):
// mutable, gitignored, per-worktree scratch that no cache entry captures.
const (
	describeHostBinaryDir  = "go-describe"
	describeHostBinaryName = "host-binary"
)

// hostBinaryTarget returns the stable per-project path this project's describe
// host binary is built at, or "" when there is none to use.
//
// It is "" when the orchestrator handed no scratch root (an older CLI, or a
// direct call in a test), and when the project path is absent or is not local
// to the scratch root: absolute, escaping through "..", or, on Windows, rooted,
// carrying a volume name or naming a reserved device. Every one of those falls
// the caller back to the throwaway temporary file, which is always correct —
// only slower.
func hostBinaryTarget(ctx *pctx.Context) string {
	if ctx == nil {
		return ""
	}
	cacheRoot := strings.TrimSpace(ctx.CacheRoot)
	projectRel := strings.TrimSpace(ctx.Project.Path)
	if cacheRoot == "" || projectRel == "" {
		return ""
	}
	cleaned := filepath.Clean(filepath.FromSlash(projectRel))
	if !filepath.IsLocal(cleaned) {
		return ""
	}
	return filepath.Join(cacheRoot, describeHostBinaryDir, cleaned, describeHostBinaryName)
}

// compileHostBinary runs `go build -o <target>` for the project's describe
// entrypoint and returns the binary plus the release the caller must defer. We
// compile a host-platform binary because we're about to run it locally.
//
// The target is a STABLE per-project path under the job's scratch root, not a
// fresh temporary file, and the release for it is a no-op: the binary is left
// in place for the next describe of the same project. That is a measured cost,
// not a tidiness preference. `go build -o` reads the build ID already embedded
// in an existing target and, when it matches the one this build would produce,
// runs NO action at all — `go build -x` on an up-to-date target prints only its
// WORK line. It does not cache the LINK step, so a fresh output path, which can
// never match, re-links the whole binary every time. Measured on this
// repository's go/samples/service-to-service (23 MB binary, warm Go cache,
// interleaved runs in both orders): 1.24 s of processor time with a fresh path
// against 0.47 s with an up-to-date stable one. Deleting the stable target and
// rebuilding it at the same path costs the same 1.3 s as a fresh path, which is
// what isolates the win to the up-to-date target rather than to the path.
//
// Reuse cannot serve a stale binary: `go build` decides on its own build ID
// whether the target is current, so a source, dependency, flag or toolchain
// change re-links it. The correctness of the reuse is Go's, not this job's.
// That also bounds what two describes of one project with DIFFERENT host build
// configurations cost each other — `build --tags x` and a plain `test`, say:
// they alternate at one path and each re-links, ~1 s for a 23 MB binary, while
// every compile action stays shared in the Go cache. One path per project is
// the cheaper trade in a scratch root that nothing reaps.
//
// Two describe runs of the same project serialize on the .gen describe lock
// (build~describe and test~describe both schedule one), and the path is inside
// the per-WORKSPACE scratch root, so two worktrees never share a target.
func compileHostBinary(ctx *pctx.Context, projectPath string) (string, func(), error) {
	outPath := hostBinaryTarget(ctx)
	release := func() {}
	if outPath != "" {
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			outPath = ""
		}
	}
	if outPath == "" {
		tmp, err := os.CreateTemp("", "putnami-describe-*")
		if err != nil {
			return "", nil, err
		}
		outPath = tmp.Name()
		tmp.Close()
		release = func() { os.Remove(outPath) }
	}
	// A failure must not leave a half-written or stale target behind: the next
	// run would either execute it or pay the full rebuild anyway.
	fail := func(err error) (string, func(), error) {
		release()
		os.Remove(outPath)
		return "", nil, err
	}

	pkg, err := resolveDescribeEntrypoint(ctx, projectPath)
	if err != nil {
		return fail(err)
	}

	goBin, err := toolchain.ResolveGo()
	if err != nil {
		return fail(err)
	}
	// The build configuration is the project's host build, resolved once for
	// every step that compiles this entrypoint, so build~compile builds the same
	// program next. Any other configuration changes the action ID of `net` and of
	// everything above it on a host with a C compiler, and compile would recompile
	// what describe just compiled instead of linking it. The flags exclude
	// -ldflags on purpose — see toolchain.HostBuild.
	buildFlags, buildEnv := toolchain.HostBuildInvocation(ctx, nil, os.Environ(), projectPath, goBin)
	buildArgs := append([]string{"build", "-o", outPath}, buildFlags...)
	buildArgs = append(buildArgs, pkg)
	// goBin is resolved by the toolchain and pkg by the describe entrypoint
	// resolver; both are passed as direct argv without a shell.
	cmd := exec.Command(goBin, buildArgs...) //nolint:gosec
	cmd.Dir = projectPath
	// The environment resolves the workspace go.work explicitly, over an
	// inherited GOWORK=off.
	cmd.Env = buildEnv
	if out, err := cmd.CombinedOutput(); err != nil {
		return fail(fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out))))
	}
	return outPath, release, nil
}

// resolveDescribeEntrypoint chooses the package to compile for describe mode.
// A project with multiple cmd/ binaries must opt in explicitly so describe
// cannot accidentally run a utility binary such as cmd/migrate.
func resolveDescribeEntrypoint(ctx *pctx.Context, projectPath string) (string, error) {
	if explicit := configuredDescribeEntrypoint(ctx); explicit != "" {
		return explicit, nil
	}
	if hasRootMainPackage(projectPath) {
		return ".", nil
	}

	candidates := cmdMainCandidates(projectPath)
	switch len(candidates) {
	case 0:
		return ".", nil
	case 1:
		return "./cmd/" + candidates[0], nil
	default:
		return "", fmt.Errorf("multiple cmd/ entrypoints found for describe (%s); set options.@putnami/go.describe.entrypoint in putnami.json to the application binary that wires app.Describer plugins", strings.Join(candidates, ", "))
	}
}

func configuredDescribeEntrypoint(ctx *pctx.Context) string {
	if ctx == nil {
		return ""
	}
	if v := ctx.Params.String("describe-entrypoint", "describeEntrypoint"); v != "" {
		return v
	}
	if v := ctx.Params.String("entrypoint"); v != "" {
		return v
	}

	for _, key := range []string{"@putnami/go", "go"} {
		raw, ok := ctx.Project.Options[key]
		if !ok {
			continue
		}
		var opts struct {
			Entrypoint         string `json:"entrypoint"`
			DescribeEntrypoint string `json:"describeEntrypoint"`
			Describe           struct {
				Entrypoint string `json:"entrypoint"`
			} `json:"describe"`
		}
		if json.Unmarshal(raw, &opts) != nil {
			continue
		}
		switch {
		case opts.Describe.Entrypoint != "":
			return opts.Describe.Entrypoint
		case opts.DescribeEntrypoint != "":
			return opts.DescribeEntrypoint
		case opts.Entrypoint != "":
			return opts.Entrypoint
		}
	}
	return ""
}

func hasRootMainPackage(projectPath string) bool {
	matches, _ := filepath.Glob(filepath.Join(projectPath, "*.go"))
	for _, p := range matches {
		if isMainPackage(p) {
			return true
		}
	}
	return false
}

func cmdMainCandidates(projectPath string) []string {
	entries, _ := os.ReadDir(filepath.Join(projectPath, "cmd"))
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mainFile := filepath.Join(projectPath, "cmd", e.Name(), "main.go")
		if _, err := os.Stat(mainFile); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// runDescribeBinary executes the binary with PUTNAMI_DESCRIBE=all and
// PUTNAMI_DESCRIBE_OUT pointing at .gen/.
//
// Two failure modes are handled explicitly:
//
//   - The binary was built against an older go.putnami.dev/app that doesn't
//     understand PUTNAMI_DESCRIBE. The binary falls through to the normal
//     start path and binds listeners. We watch the merged output stream for
//     the framework's "🤖 ready" log line and abort immediately when we see
//     it; the error names the resolved framework version (when readable from
//     the binary's buildinfo) so the fix is obvious.
//   - The binary hangs in Configure (the framework contract forbids this,
//     but we protect ourselves anyway). We cap the run at maxRuntime —
//     describeMaxRuntime in production; tests inject a much larger cap so a
//     CPU-starved machine cannot turn marker detection into a spurious timeout.
//
// PORT and GRPC_PORT are pinned to 0 so even if the binary races past the
// readiness-detection check it binds to ephemeral ports rather than whatever
// production port the project's config declares.
func runDescribeBinary(binPath, projectPath, genDir, fwVersion string, maxRuntime time.Duration) (string, error) {
	cmd := exec.Command(binPath)
	cmd.Dir = projectPath
	cmd.Env = append(os.Environ(),
		"PUTNAMI_DESCRIBE=all",
		"PUTNAMI_DESCRIBE_OUT="+genDir,
		"PORT=0",
		"GRPC_PORT=0",
	)

	readyCh := make(chan struct{}, 1)
	output := newDescribeOutputWriter(readyCh)
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.WaitDelay = describePipeDrainDelay

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start describe binary: %w", err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	select {
	case <-readyCh:
		// The binary started its real listener — too-old framework. Kill it,
		// then wait for os/exec to finish copying both streams before reading
		// the captured output and surfacing a targeted error.
		_ = cmd.Process.Kill()
		<-waitDone
		body := output.String()
		return body, fmt.Errorf("binary did not honor PUTNAMI_DESCRIBE — %s likely too old%s; bump the framework dependency or rebuild against a version that supports describe mode",
			frameworkAppPath, fwVersionSuffix(fwVersion))
	case err := <-waitDone:
		body := output.String()
		return body, err
	case <-time.After(maxRuntime):
		_ = cmd.Process.Kill()
		<-waitDone
		body := output.String()
		return body, fmt.Errorf("describe run timed out after %s%s",
			maxRuntime, fwVersionSuffix(fwVersion))
	}
}

// fwVersionSuffix renders " (go.putnami.dev/app v1.2.3)" for error messages
// when buildinfo gave us a version, otherwise "".
func fwVersionSuffix(fwVersion string) string {
	if fwVersion == "" {
		return ""
	}
	return fmt.Sprintf(" (%s %s)", frameworkAppPath, fwVersion)
}

// readFrameworkVersion extracts the resolved go.putnami.dev/app version from
// the compiled binary's embedded build info. Returns "" if the binary has no
// build info (very rare, e.g. stripped) or doesn't import the framework.
//
// When a `replace` directive points the framework at a different module
// version, build info exposes both the original requirement and the actual
// substitute via dep.Replace — we report the substitute since that's what
// actually shipped in the binary.
func readFrameworkVersion(binPath string) (string, error) {
	bi, err := buildinfo.ReadFile(binPath)
	if err != nil {
		return "", err
	}
	for _, dep := range bi.Deps {
		if dep == nil {
			continue
		}
		if dep.Path != frameworkAppPath {
			continue
		}
		if dep.Replace != nil && dep.Replace.Version != "" {
			return dep.Replace.Version, nil
		}
		return dep.Version, nil
	}
	return "", nil
}

// acquireDescribeLock takes an exclusive advisory lock on .gen/.describe.lock
// so concurrent describe runs against the same project (build~describe and
// test~describe scheduled in parallel within a single putnami invocation)
// serialize instead of trampling each other's .gen/ writes.
//
// Backed by lockedfile (the same primitive cmd/go uses for the module cache):
// portable across Unix and Windows, and crash-safe — locks are released when
// the holder process exits even if it didn't call the unlock function.
func acquireDescribeLock(genDir string) (func(), error) {
	mu := lockedfile.MutexAt(filepath.Join(genDir, ".describe.lock"))
	unlock, err := mu.Lock()
	if err != nil {
		return nil, err
	}
	return unlock, nil
}

// resetCededSchemaToGenerateStaging rebuilds .gen/schema from build-generate's
// staged output before this run observes that tree.
//
// .gen/schema is build-describe's declared output and build-generate cedes it
// (ADR 0005). collectArtifacts merges the two OpenAPI producers against the
// BEFORE snapshot, and stagedSchemaArtifacts promotes what generate staged but
// the describe binary never rewrote. Generate keeps its own copy under
// .gen/generate-staging/schema, and this rebuilds .gen/schema from it.
//
// It REMOVES the tree first, so the starting state is a function of generate's
// output alone whether generate ran in this invocation or was served from cache,
// and whatever an earlier build left behind is gone.
//
// A missing mirror is not an error: a project whose generators produce no schema
// file simply hands over an empty tree.
func resetCededSchemaToGenerateStaging(genDir string) error {
	src := generateStagingRoot(genDir)
	dst := filepath.Join(genDir, schemaSubdir)
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == src {
				return nil
			}
			return err
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o644)
	})
}

// schemaSnapshot maps an absolute path under .gen/schema/ to its previous
// content and mtime+size signature. collectArtifacts uses the signature to
// tell what describe wrote and the content to merge duplicate producers.
type schemaSnapshot map[string]schemaFileSnapshot

type schemaFileSnapshot struct {
	signature string
	content   []byte
}

// snapshotSchemaDir captures a quick fingerprint of every file under dir.
func snapshotSchemaDir(dir string) schemaSnapshot {
	out := schemaSnapshot{}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		out[path] = schemaFileSnapshot{
			signature: fmt.Sprintf("%d-%d", info.ModTime().UnixNano(), info.Size()),
			content:   body,
		}
		return nil
	})
	return out
}

// changedSince reports whether path didn't exist in the snapshot or has a
// different signature now — i.e. the describe run produced or updated it.
func (s schemaSnapshot) changedSince(path string) bool {
	prev, hadIt := s[path]
	if !hadIt {
		return true
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	now := fmt.Sprintf("%d-%d", info.ModTime().UnixNano(), info.Size())
	return now != prev.signature
}

func (s schemaSnapshot) content(path string) ([]byte, bool) {
	prev, ok := s[path]
	if !ok {
		return nil, false
	}
	body := make([]byte, len(prev.content))
	copy(body, prev.content)
	return body, true
}

// collectArtifacts walks .gen/ for files that look like generated specs and
// (when commit is requested) mirrors the converged result into the project
// tree. Returns the list of project-relative paths it touched, for the
// manifest.
//
// before is the snapshot taken before describe ran. A file is processed when
// EITHER:
//
//   - describe produced/updated it (changedSince) — the snapshot's prior copy,
//     when present, is either build-generate's static output or stale output
//     restored from an earlier describe run. OpenAPI artifacts are merged,
//     known describe-only artifacts take the fresh value, and unknown
//     duplicates are rejected; or
//   - build-generate staged it for deferred commit (staged[rel]) — describe is
//     the sole committer for app projects, so it must promote even the
//     artifacts the describe binary never rewrote (e.g. the static OpenAPI stub
//     of an app with HTTP routes but no runtime OpenAPI plugin).
//
// Files that are neither changed nor staged (e.g. stale .gen/ leftovers from a
// removed producer) are ignored, so describe never resurrects them into the
// tracked tree.
func collectArtifacts(projectPath, genDir string, commit bool, before schemaSnapshot, staged map[string]bool) ([]string, error) {
	var artifacts []string

	err := filepath.WalkDir(genDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(genDir, path)
		if relErr != nil {
			return relErr
		}
		// Plugins are encouraged to write under a stable sub-tree (typically
		// "schema/"). Skip everything outside it so internal scratch files
		// (e.g. .gen/version.json) don't get committed by accident.
		if !strings.HasPrefix(rel, "schema"+string(filepath.Separator)) {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		changed := before.changedSince(path)
		// Process describe-touched files and build-generate-staged files; ignore
		// everything else (untouched, unstaged — i.e. stale .gen/ leftovers).
		if !changed && !staged[relSlash] {
			return nil
		}
		// Only files describe itself rewrote count as describe artifacts in the
		// manifest; staged-but-unchanged files were already listed by generate.
		if changed {
			artifacts = append(artifacts, filepath.Join(".gen", rel))
		}

		// Don't commit gz companions — they're build-time/runtime caches.
		if strings.HasSuffix(rel, ".gz") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Merge/conflict-check only applies when describe rewrote the file over
		// build-generate's prior copy. A staged-but-unchanged file is already
		// the converged value (generate's canonical output); take it as-is.
		if changed {
			if previous, ok := before.content(path); ok {
				switch {
				case openapiutil.IsOpenAPIPath(relSlash):
					body, err = openapiutil.Merge(previous, body, "build-generate", "build-describe")
					if err != nil {
						return err
					}
				case isCapabilitiesSchemaPath(relSlash), isHTTPRoutesSchemaPath(relSlash),
					isFeatureEvidencePath(relSlash), isAPIProtoSchemaPath(relSlash):
					// capabilities.json, http-routes.json, api.proto and the
					// feature-evidence artifacts have a single producer — the app's
					// describe binary.
					// A differing prior copy is describe's OWN stale output from an
					// earlier incremental build or a build-generate cache restore,
					// not a competing producer, so take the fresh describe output as
					// authoritative rather than failing with a false multi-producer
					// conflict. body already holds it.
					//
					// Promotion is deliberately VERBATIM, here as for every artifact:
					// the committed copy is byte-identical to the .gen one. The
					// single-project stability contract is enforced where the
					// manifest is produced (the emitter scopes packages[] to the
					// project's capability surface, and canonical emission drops
					// anything outside it), never by a second projection applied on the
					// way into the tree — two artifacts that disagree would leave "which
					// one is the manifest" unanswerable for publish, doctor and the
					// conformance pack. The reachable closure stays in .gen/version.json,
					// which is never promoted.
				case !bytes.Equal(previous, body):
					return fmt.Errorf("multiple schema producers wrote %s with different content; no merger is registered for this artifact", relSlash)
				}
			}
		}
		if openapiutil.IsOpenAPIPath(relSlash) {
			body, err = openapiutil.Canonicalize(body)
			if err != nil {
				return err
			}
		}
		// Re-materialize the .gen/ copy only when we changed its bytes (a merge
		// or canonicalization); a staged-but-unchanged file is already correct.
		if changed {
			if err := os.WriteFile(path, body, 0o644); err != nil {
				return err
			}
		}
		if !commit {
			return nil
		}
		dst := filepath.Join(projectPath, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			return err
		}
		artifacts = append(artifacts, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return artifacts, nil
}

// stagedSchemaArtifacts reads .gen/generate-result.json and returns the set of
// schema-relative paths (e.g. "schema/openapi.json") that build-generate wrote
// under .gen/ this build. For app projects build-generate defers committing
// these (describe is the sole committer); describe consults this set so
// it promotes every staged artifact — even ones its binary never rewrote.
//
// A missing or unparsable manifest yields nil: callers treat that as "nothing
// staged", which preserves the legacy behavior of committing only the files
// describe itself touched.
func stagedSchemaArtifacts(projectPath string) map[string]bool {
	data, err := os.ReadFile(filepath.Join(projectPath, ".gen", "generate-result.json"))
	if err != nil {
		return nil
	}
	var manifest GenerateResult
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil
	}
	const genPrefix = ".gen/schema/"
	staged := make(map[string]bool)
	for _, s := range manifest.Schemas {
		slug := filepath.ToSlash(s)
		if strings.HasPrefix(slug, genPrefix) {
			// ".gen/schema/openapi.json" -> "schema/openapi.json"
			staged[strings.TrimPrefix(slug, ".gen/")] = true
		}
	}
	return staged
}

// mergeIntoManifest opens .gen/generate-result.json (written by the static
// codegen runner) and appends the describe artifacts under "schemas". When
// the manifest doesn't exist yet (project has no static visitors) we create
// a minimal one so downstream tasks see consistent shape.
func mergeIntoManifest(projectPath string, artifacts []string) error {
	manifestPath := filepath.Join(projectPath, ".gen", "generate-result.json")
	var manifest GenerateResult
	if data, err := os.ReadFile(manifestPath); err == nil {
		_ = json.Unmarshal(data, &manifest)
	}
	if manifest.Exports == nil {
		manifest.Exports = map[string]string{}
	}
	manifest.Schemas = append(manifest.Schemas, artifacts...)

	// Surface the .gen copy of each schema artifact so downstream tasks have a
	// stable place to read regardless of commit opt-out. The runner-owned
	// export keys mirror what the static codegen runner emits.
	//
	// The value is the project-relative path, never an absolute one: this file
	// is captured into the cache and restored into other checkouts, so a path
	// that names THIS worktree makes the manifest non-relocatable and its bytes
	// checkout-dependent. Consumers join it with the project root.
	for _, rel := range artifacts {
		slug := filepath.ToSlash(rel)
		genRel, ok := strings.CutPrefix(slug, ".gen/")
		if !ok {
			continue
		}
		base := path.Base(genRel)
		key := strings.TrimSuffix(base, path.Ext(base)) + "-spec"
		manifest.Exports[key] = slug
	}

	// Relativize the WHOLE manifest, not just the entries added above: the file
	// being merged into may predate the project-relative rule or have been restored from a cache
	// entry an older extension wrote, and rewriting its absolute values back out
	// verbatim would reintroduce exactly the checkout-dependence this removes.
	relative := &GenerateResult{
		Hash:    manifest.Hash,
		Mode:    manifest.Mode,
		Exports: genresult.Relativize(projectPath, manifest.Exports),
		Assets:  genresult.Relativize(projectPath, manifest.Assets),
		Schemas: genresult.RelativizeList(projectPath, manifest.Schemas),
	}

	data, err := json.MarshalIndent(relative, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, data, 0o644)
}
