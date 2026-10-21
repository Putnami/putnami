package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.putnami.dev/protocol/infra"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/genresult"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/build"
	"go.putnami.dev/typescript/extension/internal/errs"
	"go.putnami.dev/typescript/extension/internal/hooks"
)

// GeneratedClientOutputPort is the data output port build-generate reports the
// project-relative generated-client directory on. The manifest declares a port
// of this exact name (tasks.build-generate.outputs), and the consumer reads the
// directory back under it — a generated client's location is chosen by the
// project's clientgen config, so this port is the only way the v3 task contract
// can name it (a declared output's pathFrom). Renaming it on either side
// silently stops the client from being captured, so the name is a constant the
// manifest-contract test pins.
const GeneratedClientOutputPort = "clientOutput"

func runBuildGenerate(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	if len(ctx.SelectedProjects) > 0 {
		return runBuildGenerateBatch(ctx)
	}
	projectPath := filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)

	emit.PhaseStart("generate")
	genResult, err := doGenerate(ctx, emit, projectPath)
	if err != nil {
		emit.PhaseEnd("generate", "failed")
		for _, d := range generateDiagnostics(err) {
			emit.DiagnosticWithCode("error", d.Message, "", 0, 0, d.Code)
		}
		return "FAILED", nil, err
	}
	emit.PhaseEnd("generate", "success")
	emit.Metric("generate-hash", 1, "count")

	data := generateResultData(projectPath, genResult)
	// Report the generated client directory so the scheduler captures clients/
	// alongside .gen as a generate cache output (a cache hit must rematerialize the
	// client, not just .gen). Reported only when a client was actually emitted.
	if out, ok := generatedClientOutput(projectPath); ok {
		data[GeneratedClientOutputPort] = out
	}
	return "OK", data, nil
}

// generateResultData builds the generate job's wire payload. It is the one
// place solo and batch agree on that shape, and it reports exactly what
// .gen/generate-result.json holds: project-relative paths.
//
// The payload is not a debug echo — the scheduler stores it in this task's cache
// entry and `cache verify` digests it — so an absolute path here makes two
// equivalent live runs in two worktrees disagree just as surely as one in the
// file.
func generateResultData(projectPath string, genResult *build.GenerateResult) map[string]any {
	return map[string]any{
		"hash":    genResult.Hash,
		"exports": genresult.Relativize(projectPath, genResult.Exports),
		"assets":  genresult.Relativize(projectPath, genResult.Assets),
	}
}

// generatedClientOutput reports the project-relative directory of the generated
// TypeScript client when the client generator produced one. It reads the
// clientgen contract the generator writes (.gen/clientgen/config.json) and
// confirms the output directory exists and is non-empty — a project with no
// client generator, or one whose API exposes no services, produces no directory
// and reports nothing (so the generate step captures only .gen, as before).
func generatedClientOutput(projectPath string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(projectPath, ".gen", "clientgen", "config.json"))
	if err != nil {
		return "", false
	}
	var cfg struct {
		Targets []string `json:"targets"`
		TS      struct {
			Output string `json:"output"`
		} `json:"ts"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return "", false
	}
	if cfg.TS.Output == "" || !slices.Contains(cfg.Targets, "ts") {
		return "", false
	}
	dir := filepath.Join(projectPath, cfg.TS.Output)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", false
	}
	if entries, _ := os.ReadDir(dir); len(entries) == 0 {
		return "", false
	}
	return cfg.TS.Output, true
}

// doGenerate runs the full generate phase.
func doGenerate(ctx *pctx.Context, emit *jsonl.Emitter, projectPath string) (*build.GenerateResult, error) {
	mode := ctx.Params.String("mode")
	if mode == "" {
		mode = "build"
	}
	clear := ctx.Params.Bool("clear", false)

	// 1. Content hash
	emit.Progress(1, 5, "Computing content hash")
	contentHash := build.ComputeContentHash(projectPath, ctx.FilePatterns)
	if contentHash == "" {
		contentHash = "0"
	}

	// 2. Prepare .gen/
	genDir := filepath.Join(projectPath, ".gen")
	if err := build.EnsureRealDirectory(genDir, clear); err != nil {
		return nil, errs.Wrapf(err, errs.CodeGenerateFailed, "preparing .gen directory")
	}
	if err := build.ResetHTTPRoutes(projectPath); err != nil {
		return nil, errs.Wrapf(err, errs.CodeGenerateFailed, "resetting HTTP route inventory")
	}

	// 2b. Copy project generate assets
	if err := build.CopyProjectGenerateAssets(ctx.WorkspaceRoot, projectPath, genDir, emit); err != nil {
		return nil, errs.Wrapf(err, errs.CodeGenerateFailed, "copying generate assets")
	}

	// 3. Pre-build hooks (delegate to bun)
	emit.Progress(2, 5, "Running generation hooks")
	exports := make(map[string]string)
	assets := make(map[string]string)

	bunBin, err := preBuildBun(ctx.WorkspaceRoot, projectPath)
	if err != nil {
		return nil, err
	}
	// Clear the scratch fragments before the producers run and reconcile the
	// committed manifest once, after every one of them has returned: a
	// producer deleted from code must not leave a requirement behind, and a
	// sync taken mid-way would commit a strictly-degraded manifest.
	if err := infra.ClearGeneratedRequirementSidecars(projectPath); err != nil {
		return nil, errs.Wrapf(err, errs.CodeGenerateFailed, "clearing generated infra requirements")
	}
	hookResult, err := hooks.RunHooks(ctx.WorkspaceRoot, projectPath, ctx.Project.Name, hooks.HookPreBuild, mode, bunBin, ctx.Params.Bool("debug", false), nil)
	if err != nil {
		return nil, errs.Wrapf(err, errs.CodeGenerateFailed, "pre-build hooks")
	}
	// Every preBuild hook must report a complete route evaluation, and
	// discovery must have read every manifest. A run without both cannot tell
	// which routes the project serves. It fails here, with every cause the
	// hooks returned, before it aggregates the fragments, advertises the
	// aggregate, reconciles infra requirements, promotes a contract or writes a
	// generate result, so the committed route inventory and infra requirements
	// keep their bytes. The scratch fragments stay in .gen until the next run
	// resets them.
	if len(hookResult.Unavailable) > 0 {
		return nil, httpRoutesUnavailableError(hookResult.Unavailable)
	}
	for k, v := range hookResult.Exports {
		exports[k] = v
	}
	for k, v := range hookResult.Assets {
		assets[k] = v
	}
	// httpRoutesPath is the validated aggregate, or "" when no hook wrote a
	// fragment. A fragment with no route still yields an aggregate: a
	// known-empty inventory with routes [].
	httpRoutesPath, err := build.AggregateHTTPRoutes(projectPath)
	if err != nil {
		return nil, errs.Wrapf(err, errs.CodeGenerateFailed, "aggregating HTTP route inventory")
	}
	if httpRoutesPath != "" {
		assets[HTTPRoutesRelPath] = httpRoutesPath
	}
	// The converged producer set exists HERE, and only here. Every preBuild
	// hook has returned, and the @putnami/application hook activates the full
	// config registry (loader imports plus ConfigContributor blocks — see
	// bin/_activate-config.ts) before it writes .gen/infra/secrets.json, so
	// this sync sees the complete secret-name set alongside the database,
	// migration, storage and event fragments the other producers wrote.
	//
	// build-generate is the sole committer of infra/requirements.json;
	// config-extract-exec rewrites only the secrets fragment from the same
	// activated registry (bin/_activate-config.ts) and reproduces the bytes
	// committed here; it does not clear sidecars because it cannot regenerate
	// the other fragments. The capability manifest and design graph have one
	// author (the configExtract hook passes publishCapabilityManifest:false /
	// publishDesignGraph:false).
	syncGeneratedInfraRequirements(projectPath, emit)

	// 4. Update version.json with content hash
	emit.Progress(3, 5, "Updating version info")
	buildInfoExports, err := build.UpdateVersionContentHash(projectPath, contentHash)
	if err != nil {
		return nil, errs.Wrapf(err, errs.CodeGenerateFailed, "updating version info")
	}
	for k, v := range buildInfoExports {
		exports[k] = v
	}

	// 5. Bundled serve. The version-neutral activation view derived from
	// capabilities.json is authoritative; feature-evidence artifacts are not an
	// activation input. Never fall back to enumerating the ad-hoc generate export
	// map, nor to a manifest left on disk by an earlier build (see
	// capabilityManifestForBundledServe).
	capabilitiesPath, generateBundledServe, err := capabilityManifestForBundledServe(exports, assets)
	if err != nil {
		return nil, errs.Wrapf(err, errs.CodeGenerateFailed, "resolving bundled capability activation")
	}
	if generateBundledServe {
		capabilitiesPath, err = build.ReconcileCapabilityManifest(projectPath, capabilitiesPath, exports)
		if err != nil {
			return nil, errs.Wrapf(err, errs.CodeGenerateFailed, "reconciling capability manifest with generated loaders")
		}
		emit.Progress(4, 5, "Generating bundled serve")
		bundledPath, err := build.GenerateBundledServe(projectPath, capabilitiesPath, nil)
		if err != nil {
			return nil, errs.Wrapf(err, errs.CodeGenerateFailed, "generating bundled serve from capability manifest")
		}
		if bundledPath != "" {
			exports["bundled-serve"] = bundledPath
		}
	}

	// 5b. Promote the capability manifest into the tracked tree when the project
	// asked for it. The producer wrote it under .gen; `architecture validate`
	// reads DARC evidence from the committed copy and runs no build, so an
	// application whose contracts are enforced at run time has to commit what it
	// emitted or its evidence is invisible.
	if projectCommitsCapabilityManifest(ctx) {
		if _, err := promoteCapabilityManifest(projectPath, capabilitiesPath); err != nil {
			return nil, errs.Wrapf(err, errs.CodeGenerateFailed, "promoting capability manifest")
		}
	}

	// 5c. Commit the HTTP route inventory. It is committed by default, with no
	// option, so this task reads no project option its cache key does not hold.
	// Every run that reaches this point ran every preBuild hook, each of which
	// reported a complete route evaluation, and validated the aggregate; any
	// other run failed above, before this point, and left the committed copy as
	// it stands.
	if err := commitHTTPRoutes(projectPath, httpRoutesPath); err != nil {
		return nil, errs.Wrapf(err, errs.CodeGenerateFailed, "committing HTTP route inventory")
	}

	// 6. Write manifest
	emit.Progress(5, 5, "Writing manifest")
	genResult := &build.GenerateResult{
		Hash:    contentHash,
		Mode:    mode,
		Exports: exports,
		Assets:  assets,
	}

	// Serialize project-relative paths. Hooks and the generators above report
	// absolute ones, and that stays true in memory (the caller's compile step
	// resolves an entrypoint from this very struct) — but the manifest is
	// captured into the cache and restored into other checkouts, so its BYTES
	// must not name this worktree. The rule is shared with the Go
	// runner so the two languages keep one manifest shape.
	manifestData, err := json.MarshalIndent(&build.GenerateResult{
		Hash:    genResult.Hash,
		Mode:    genResult.Mode,
		Exports: genresult.Relativize(projectPath, genResult.Exports),
		Assets:  genresult.Relativize(projectPath, genResult.Assets),
	}, "", "  ")
	if err != nil {
		return nil, errs.Wrapf(err, errs.CodeGenerateFailed, "marshaling generate result")
	}
	if err := os.WriteFile(filepath.Join(genDir, "generate-result.json"), manifestData, 0644); err != nil {
		return nil, errs.Wrapf(err, errs.CodeGenerateFailed, "writing generate result")
	}

	// The scheduler captures and restores the .gen tree as the generate step's
	// cache output (it writes the "gen" resource), so a generate cache hit
	// rematerializes .gen directly.

	return genResult, nil
}

// capabilityManifestForBundledServe decides whether this build needs bundled
// activation and returns the manifest that describes it.
//
// No fallback to a manifest on disk: it is the previous build's activation contract.
func capabilityManifestForBundledServe(exports, assets map[string]string) (string, bool, error) {
	capabilitiesPath, hasManifest := assets["schema/capabilities.json"]
	loaders := make([]string, 0, len(exports))
	for name, path := range exports {
		if build.IsImportableServerLoader(name, path) {
			loaders = append(loaders, name)
		}
	}
	if len(loaders) > 0 && !hasManifest {
		sort.Strings(loaders)
		return "", false, fmt.Errorf(
			"generated server loaders require schema/capabilities.json: %s were generated but no pre-build hook reported the capability manifest "+
				"(the @putnami/application hook reports it from Application.build(); it reports nothing when it cannot load the project entry point)",
			strings.Join(loaders, ", "))
	}
	return capabilitiesPath, hasManifest, nil
}

func syncGeneratedInfraRequirements(projectPath string, emit *jsonl.Emitter) {
	diags, err := infra.SyncGeneratedRequirements(projectPath)
	if err != nil {
		emit.Warn(fmt.Sprintf("infra requirements: sync failed: %v", err))
	}
	for _, d := range diags {
		emit.Warn("infra requirements: " + d.String())
	}
}

// loadGenerateResult reads back the manifest a previous generate wrote (or a
// cache hit restored) and hands callers the same absolute paths doGenerate had
// in memory: the file stores them project-relative so it can be restored into
// any checkout, and this is where that is undone. Values already absolute — a
// manifest written before the project-relative rule — resolve to themselves.
func loadGenerateResult(projectPath string) *build.GenerateResult {
	data, err := os.ReadFile(filepath.Join(projectPath, ".gen", "generate-result.json"))
	if err != nil {
		return &build.GenerateResult{
			Exports: map[string]string{},
			Assets:  map[string]string{},
		}
	}
	var result build.GenerateResult
	if json.Unmarshal(data, &result) != nil {
		return &build.GenerateResult{
			Exports: map[string]string{},
			Assets:  map[string]string{},
		}
	}
	result.Exports = genresult.Resolve(projectPath, result.Exports)
	result.Assets = genresult.Resolve(projectPath, result.Assets)
	return &result
}

// CapabilityManifestRelPath is the canonical project-relative capability
// manifest path (mirrors go.putnami.dev/protocol/capabilities.CommittedPath;
// kept as a local literal for the same reason the Go extension keeps its own).
const CapabilityManifestRelPath = "schema/capabilities.json"

// projectCommitsCapabilityManifest reports whether the project asked for its
// capability manifest to be tracked, through `options.generate.capabilities`.
//
// It is opt-in, and deliberately not folded into `options.generate.schema`.
// `schema` defaults to true and covers the config and OpenAPI documents. A
// committed capability manifest is a reviewed artifact — `putnami architecture
// validate` reads it as DARC evidence — so a project takes it deliberately.
func projectCommitsCapabilityManifest(ctx *pctx.Context) bool {
	if ctx == nil {
		return false
	}
	raw, ok := ctx.Project.Options["generate"]
	if !ok {
		return false
	}
	var opts struct {
		Capabilities *bool `json:"capabilities"`
	}
	if json.Unmarshal(raw, &opts) != nil || opts.Capabilities == nil {
		return false
	}
	return *opts.Capabilities
}

// promoteCapabilityManifest copies the emitted capability manifest from .gen
// into the tracked tree.
//
// The producer stays the sole author: this reads what `@putnami/application`
// wrote and never composes a manifest. That split is the same one the Go
// extension keeps — the framework describes the application, the extension
// promotes the description — and it is what lets `architecture validate` read
// DARC evidence from a committed file without running a build.
func promoteCapabilityManifest(projectPath, emittedPath string) (string, error) {
	return promoteSchemaArtifact(projectPath, emittedPath, CapabilityManifestRelPath)
}

// HTTPRoutesRelPath is the project-relative path of the committed
// putnami.http-routes.v1 inventory, the same path the Go extension commits,
// and the asset key the aggregate is reported under.
const HTTPRoutesRelPath = "schema/http-routes.json"

// commitHTTPRoutes makes the committed route inventory equal what the preBuild
// hooks produced. The caller invokes it only after every hook returned with a
// complete route evaluation and the aggregate validated
// (hooks.Result.Unavailable is empty). emittedPath is that aggregate, promoted
// verbatim, including a known-empty one; it is "" when no hook wrote a
// fragment, and then a committed copy has no producer and is removed.
func commitHTTPRoutes(projectPath, emittedPath string) error {
	if emittedPath != "" {
		_, err := promoteSchemaArtifact(projectPath, emittedPath, HTTPRoutesRelPath)
		return err
	}
	committed := filepath.Join(projectPath, filepath.FromSlash(HTTPRoutesRelPath))
	if err := os.Remove(committed); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove committed %s: %w", HTTPRoutesRelPath, err)
	}
	return nil
}

// httpRoutesUnavailableError is the failure of a generate that cannot tell
// which routes the project serves: discovery could not read a manifest, or a
// preBuild hook did not report a complete route evaluation. causes are the
// entries of hooks.Result.Unavailable or hooks.Discovery.Unavailable. Solo and
// batch report this one error through generateDiagnostics, so both attribute
// the same code and text to the project.
func httpRoutesUnavailableError(causes []string) error {
	return errs.New(errs.CodeHTTPRoutesUnavailable,
		"cannot tell which routes the project serves: "+strings.Join(causes, "; ")+
			". Generate changed nothing in "+HTTPRoutesRelPath+"; resolve each cause, then generate again")
}

// preBuildBun resolves the Bun binary the preBuild hooks run with. A project
// whose hook discovery is complete and finds no preBuild hook runs no hook, so
// it needs no Bun: preBuildBun returns "" for it and its run proceeds, which
// removes a route inventory that no producer writes any more. Without Bun, a
// project with a preBuild hook, or whose hook discovery is incomplete, fails:
// the run cannot tell which routes its hooks would report.
func preBuildBun(workspaceRoot, projectPath string) (string, error) {
	bunBin, bunErr := resolveBunBin()
	if bunErr == nil {
		return bunBin, nil
	}
	discovery, err := hooks.Discover(workspaceRoot, projectPath, hooks.HookPreBuild)
	if err != nil {
		return "", errs.Wrapf(err, errs.CodeGenerateFailed, "pre-build hooks")
	}
	if len(discovery.Hooks) > 0 {
		return "", errs.Wrapf(bunErr, errs.CodeGenerateFailed,
			"pre-build hooks of %s cannot run without Bun", strings.Join(discovery.Hooks, ", "))
	}
	if len(discovery.Unavailable) > 0 {
		return "", httpRoutesUnavailableError(discovery.Unavailable)
	}
	return "", nil
}

// promoteSchemaArtifact copies a file a framework producer emitted under .gen
// to relPath in the tracked tree, and returns the committed path. It never
// composes content: the producer stays the artifact's sole author.
//
// An empty emittedPath, or one that does not exist, promotes nothing and
// leaves a committed copy as it stands. The write is atomic: the bytes go to a
// uniquely named temporary file in the destination directory, which is then
// renamed over the committed path, so neither a crash nor a concurrent writer
// leaves a truncated file. It is skipped when the bytes already match, so a
// warm build does not touch the file's mtime.
func promoteSchemaArtifact(projectPath, emittedPath, relPath string) (string, error) {
	if emittedPath == "" {
		return "", nil
	}
	emitted, err := os.ReadFile(emittedPath) //nolint:gosec // a path this build just produced under the project's .gen
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read emitted %s: %w", relPath, err)
	}
	committed := filepath.Join(projectPath, filepath.FromSlash(relPath))
	if current, err := os.ReadFile(committed); err == nil && string(current) == string(emitted) { //nolint:gosec // a project-relative committed artifact
		return committed, nil
	}
	dir := filepath.Dir(committed)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s directory: %w", filepath.ToSlash(filepath.Dir(relPath)), err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(committed)+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("stage %s: %w", relPath, err)
	}
	staged := tmp.Name()
	_, writeErr := tmp.Write(emitted)
	if writeErr == nil {
		// A committed schema artifact is world-readable like its siblings;
		// CreateTemp opens the file 0600.
		writeErr = tmp.Chmod(0o644)
	}
	if closeErr := tmp.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		_ = os.Remove(staged)
		return "", fmt.Errorf("stage %s: %w", relPath, writeErr)
	}
	if err := os.Rename(staged, committed); err != nil {
		_ = os.Remove(staged)
		return "", fmt.Errorf("publish %s: %w", relPath, err)
	}
	return committed, nil
}
