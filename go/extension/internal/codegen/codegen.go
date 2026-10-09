// Package codegen runs the generate phase: parse the project's Go source,
// dispatch every registered visitor against it, and write artifacts to .gen/
// (always) and the project tree (for committed schemas).
//
// Visitor discovery happens at link time via blank imports of generator
// subpackages from the extension binary; see go/extension/cmd/putnami-go/main.go.
// This package owns only the orchestration — the contract lives in the SDK
// (go.putnami.dev/sdk/extension/codegen).
package codegen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/go/extension/internal/codegen/openapiutil"
	"go.putnami.dev/go/extension/internal/jobs/configextract"
	"go.putnami.dev/protocol/infra"
	sdkcodegen "go.putnami.dev/sdk/extension/codegen"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/genresult"
	"go.putnami.dev/sdk/extension/jsonl"
)

// schemaSubdir is the one subtree of .gen a generated contract is published
// under. build-generate CEDES it to build-describe, which declares and captures
// it (ADR 0005), so it is also the boundary this package mirrors across.
const schemaSubdir = "schema"

// generateStagingDir holds build-generate's own copy of everything it writes
// into the ceded .gen/schema subtree.
//
// Describe owns .gen/schema because it runs last and only its capture holds the
// converged contract. Generate still owes describe its static output: describe
// merges the two OpenAPI documents against a BEFORE snapshot of .gen/schema, and
// promotes the artifacts generate staged that the describe binary never rewrote.
// A generate CACHE HIT does not run generate, and generate's restore swaps a
// staged tree over .gen — which deletes the ceded subtree — so the hand-off has
// to have a copy somewhere generate still owns. This is that place.
//
// It is an INTERNAL hand-off between two tasks of this extension and nothing
// else ever reads it: .gen/<rel> stays the single path every consumer resolves
// (protocol ADR 0003).
const generateStagingDir = "generate-staging"

// generateStagingRoot is the generate-owned mirror of the ceded .gen/schema
// subtree, i.e. <project>/.gen/generate-staging/schema.
func generateStagingRoot(genDir string) string {
	return filepath.Join(genDir, generateStagingDir, schemaSubdir)
}

// isCededSchemaRel reports whether a generator's project-relative artifact path
// lands inside the ceded .gen/schema subtree, which is the only region the
// staging mirror covers.
func isCededSchemaRel(rel string) bool {
	return strings.HasPrefix(filepath.ToSlash(rel), schemaSubdir+"/")
}

// contractHasNoDurableHome reports whether writing rel would produce a generated
// contract this project cannot keep.
//
// A contract has exactly two durable homes: the tracked sidecar build-generate
// commits, and build-describe's cache entry for the ceded .gen/schema subtree.
// commitSchemas=false removes the first. A project with no describe phase does
// not have the second. Both conditions have to hold AND the artifact has to land
// in the ceded subtree — an artifact outside it stays in build-generate's own
// declared output, so the opt-out costs it nothing.
//
// The last term is why the answer is per-artifact rather than per-project:
// options.generate.schema is INHERITED, so a workspace-level putnami.workspace.json
// default reaches every project, and refusing on the option alone would fail the
// build of every library and CLI that generates no contract at all.
func contractHasNoDurableHome(rel string, commitSchemas, deferToDescribe bool) bool {
	return !commitSchemas && !deferToDescribe && isCededSchemaRel(rel)
}

// GenerateResult mirrors the TypeScript pipeline's generate-result.json so
// downstream tasks can locate generated files the same way regardless of
// language. Schemas lists every committed-by-default file the run produced.
//
// Every path it carries OUT of this process — into .gen/generate-result.json
// and into the job's result payload, both of which the cache stores — is
// project-relative and slash-separated; see go.putnami.dev/sdk/extension/
// genresult for why, and for the one rule both language runtimes apply.
type GenerateResult struct {
	Hash    string            `json:"hash"`
	Mode    string            `json:"mode"`
	Exports map[string]string `json:"exports"`
	Assets  map[string]string `json:"assets"`
	Schemas []string          `json:"schemas"`
}

// Run is the entry point for the build-generate job. It is a regular
// cli.JobFunc so the existing dispatcher in cmd/putnami-go can call it.
func Run(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	mode := ctx.Params.String("mode")
	if mode == "" {
		mode = "build"
	}
	clear := ctx.Params.Bool("clear", false)

	emit.PhaseStart("generate")
	res, err := run(ctx, emit, mode, clear)
	if err != nil {
		emit.PhaseEnd("generate", "failed")
		return "FAILED", nil, err
	}
	emit.PhaseEnd("generate", "success")

	// res is the manifest's on-disk form (writeManifest returns it), so the
	// payload and the file agree value for value. The payload is not a debug
	// echo: the scheduler stores it in the same cache entry as the captured
	// .gen tree, and `cache verify` digests it — an absolute path here makes
	// two equivalent live runs in two worktrees disagree just as surely as one
	// in the file.
	return "OK", map[string]any{
		"hash":    res.Hash,
		"exports": res.Exports,
		"assets":  res.Assets,
		"schemas": res.Schemas,
	}, nil
}

// run does the actual work. Split out so it can return an error and let Run
// handle the JSONL phase emission in one place.
func run(ctx *pctx.Context, emit *jsonl.Emitter, mode string, clear bool) (*GenerateResult, error) {
	projectPath := ctx.Project.FullPath
	if projectPath == "" {
		projectPath = filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)
	}

	visitors := sdkcodegen.Visitors()

	emit.Progress(1, 4, "Preparing .gen directory")
	genDir := filepath.Join(projectPath, ".gen")
	if err := ensureGenDir(genDir, clear); err != nil {
		return nil, fmt.Errorf("preparing .gen directory: %w", err)
	}
	// The staging mirror is rebuilt from scratch on every run, exactly like the
	// infra sidecars below.
	if err := os.RemoveAll(filepath.Join(genDir, generateStagingDir)); err != nil {
		return nil, fmt.Errorf("clearing staged schema mirror: %w", err)
	}
	if err := infra.ClearGeneratedRequirementSidecars(projectPath); err != nil {
		return nil, fmt.Errorf("clearing generated infra requirements: %w", err)
	}

	commitSchemas := configextract.ProjectWantsCommittedSchemas(ctx)

	// When a describe phase will run for this project, it becomes the sole
	// committer of the converged schema sidecars: build-generate stages
	// its static output to .gen/ and leaves the tracked tree untouched, so a
	// skipped / canceled / cached describe can't strand a strictly-degraded
	// intermediate there (the OpenAPI stub, a deleted requirements.json).
	// Library/CLI projects have no describe phase, so build-generate keeps
	// committing directly.
	deferToDescribe := describeWillCommit(projectPath)
	commitSchemasNow := commitSchemas && !deferToDescribe

	// A generated contract needs a durable home, and there are exactly two: the
	// tracked sidecar this task commits, or build-describe's cache entry for the
	// ceded .gen/schema subtree. A project with no describe phase has no second
	// one — nothing restores .gen/schema for it — so suppressing the tracked
	// write would leave the contract alive only until the next cache hit. Refuse
	// that combination instead of losing the file quietly.
	//
	// The refusal is raised only when a contract is actually about to be written.
	// The option is inherited — a workspace-level putnami.workspace.json default reaches
	// every project — so refusing on the option alone would fail the build of
	// every library and CLI that has no contract to lose.
	refuseSuppressedContract := func(rel string) error {
		if !contractHasNoDurableHome(rel, commitSchemas, deferToDescribe) {
			return nil
		}
		message := fmt.Sprintf(
			"options.generate.schema=false is not supported for %s, which generates %s: the project has no "+
				"describe phase (no main package importing go.putnami.dev/app), so build-generate is the sole "+
				"producer of its generated contracts and the suppressed schema/ sidecars would be their only "+
				"durable copy. The build-time .gen/schema tree is owned by build-describe, which never runs "+
				"here, so a cached build restores nothing there. Drop the option, or give the project an "+
				"application entrypoint.",
			ctx.Project.Name, filepath.ToSlash(rel))
		emit.DiagnosticWithCode("error", message, "putnami.json", 0, 0, "go.generate.schema_has_no_durable_home")
		return errors.New(message)
	}

	// syncRequirements reconciles the committed infra/requirements.json from the
	// .gen/infra/*.json fragments produced so far. It is a no-op while deferring:
	// build-generate sees only the static fragments (DB/storage/event resources
	// are runtime-only), so syncing now would delete or downgrade the committed
	// file — describe runs the final, converged sync after its runtime fragments
	// land. The .gen/infra fragments themselves are still written either way.
	syncRequirements := func() {
		if deferToDescribe {
			return
		}
		syncGeneratedInfraRequirements(projectPath, emit)
	}

	// The config schema now follows the same describe-sole-committer rule as the
	// other converged sidecars: it uses commitSchemasNow,
	// not commitSchemas. When a describe phase will run (deferToDescribe), build-
	// generate stages the workload's own blocks to .gen/config-schema.json and does
	// NOT touch the committed schema/config.json — describe re-extracts the own
	// blocks, unions the dependency/plugin blocks, and writes the committed file
	// exactly once. Before this fix, build-generate committed own-only blocks while
	// describe committed own+plugin blocks with no ordering between them, so under
	// `--impacted` a late generate could clobber describe's merge and silently drop
	// a plugin-contributed block (e.g. the `events` block). Library/CLI projects
	// have no describe phase, so build-generate keeps committing directly.
	configSchemaOutput := configextract.DefaultOutputPath
	if !commitSchemasNow {
		configSchemaOutput = configextract.FallbackOutputPath
	}

	var schemas []string

	// Config schema artifacts are part of the normal Go generation contract:
	// deployable workloads that use config.Config[T] need the manifest before
	// Cloud publish/deploy can bootstrap runtime config. The same extraction
	// refreshes infra secret requirements from sensitive fields.
	if artifact, ok, err := configextract.WriteArtifacts(
		projectPath,
		ctx.Project.Name,
		configextract.SchemaVersion(projectPath),
		configSchemaOutput,
	); err != nil {
		syncRequirements()
		return nil, fmt.Errorf("config schema: %w", err)
	} else if ok {
		schemas = append(schemas, relSchemaPath(projectPath, artifact.SchemaPath), relSchemaPath(projectPath, artifact.JSONSchemaPath))
	}
	syncRequirements()

	if len(visitors) == 0 {
		return writeManifest(projectPath, mode, "", &GenerateResult{
			Hash:    "",
			Mode:    mode,
			Exports: map[string]string{},
			Assets:  map[string]string{},
			Schemas: schemas,
		})
	}

	emit.Progress(2, 4, "Parsing Go sources")
	fset, files, hash, err := parseProject(projectPath)
	if err != nil {
		return nil, fmt.Errorf("parsing project: %w", err)
	}

	gen := &sdkcodegen.Generation{
		ProjectRoot: projectPath,
		GenDir:      genDir,
		Mode:        mode,
		Project: sdkcodegen.ProjectInfo{
			Name:            ctx.Project.Name,
			Version:         stableVersion(ctx),
			DeclaredVersion: configextract.DeclaredProjectVersion(projectPath),
			Module:          resolveModulePath(projectPath),
			Options:         ctx.Project.Options,
		},
		Fset:  fset,
		Files: files,
		Emit:  emit,
	}

	emit.Progress(3, 4, "Running visitors")
	exports := map[string]string{}
	assets := map[string]string{}

	for _, v := range visitors {
		emit.Log("debug", "generator: "+v.Name())
		result, err := v.Visit(gen)
		if err != nil {
			return nil, fmt.Errorf("generator %s: %w", v.Name(), err)
		}
		if result.IsEmpty() {
			continue
		}

		for _, sf := range result.SchemaFiles {
			if err := refuseSuppressedContract(sf.RelPath); err != nil {
				return nil, err
			}
			written, genPath, err := writeSchemaFile(projectPath, genDir, sf, commitSchemasNow)
			if err != nil {
				return nil, fmt.Errorf("generator %s: writing %s: %w", v.Name(), sf.RelPath, err)
			}
			schemas = append(schemas, written...)
			// Expose the canonical .gen copy (which always exists) under
			// <visitorName>-spec so downstream tasks have a single stable
			// location regardless of the project's commit opt-out. Absolute
			// here, in-process; writeManifest relativizes it on the way out.
			exports[v.Name()+"-spec"] = genPath
		}
		maps.Copy(exports, result.Exports)
		maps.Copy(assets, result.Assets)
		emit.Log("info", fmt.Sprintf("%s: %d schema(s), %d export(s)", v.Name(), len(result.SchemaFiles), len(result.Exports)))
	}

	syncRequirements()

	emit.Progress(4, 4, "Writing manifest")
	return writeManifest(projectPath, mode, hash, &GenerateResult{
		Hash:    hash,
		Mode:    mode,
		Exports: exports,
		Assets:  assets,
		Schemas: schemas,
	})
}

func relSchemaPath(projectPath, absPath string) string {
	rel, err := filepath.Rel(projectPath, absPath)
	if err != nil {
		return absPath
	}
	return filepath.ToSlash(rel)
}

// parseProject walks projectPath, parsing every non-test .go file and
// computing a SHA-256 hash of their contents. Vendored, generated, and
// hidden directories are excluded so they don't perturb the hash or feed
// visitors stale source.
func parseProject(projectPath string) (*token.FileSet, map[string]*ast.File, string, error) {
	fset := token.NewFileSet()
	files := make(map[string]*ast.File)

	var paths []string
	err := filepath.WalkDir(projectPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			if base == "vendor" || base == ".gen" || base == "testdata" || strings.HasPrefix(base, ".") && path != projectPath {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return nil, nil, "", err
	}

	sort.Strings(paths)

	h := sha256.New()
	for _, p := range paths {
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			// Bad source is the user's problem; record nothing for that
			// file and let go build surface the real diagnostic.
			continue
		}
		files[p] = f
		if data, err := os.ReadFile(p); err == nil {
			h.Write(data)
		}
	}

	return fset, files, hex.EncodeToString(h.Sum(nil))[:12], nil
}

// ensureGenDir prepares the .gen directory. When clear is set, the existing
// contents are removed so stale generators don't leave files behind.
func ensureGenDir(genDir string, clear bool) error {
	if clear {
		if err := os.RemoveAll(genDir); err != nil {
			return err
		}
	}
	return os.MkdirAll(genDir, 0o755)
}

// writeSchemaFile writes a SchemaFile to .gen/<RelPath> and, when the project
// hasn't opted out, also to <ProjectRoot>/<RelPath>.
//
// Returns:
//   - written: project-relative paths of every file written, for the manifest's
//     `schemas` listing.
//   - genPath: absolute path of the canonical .gen/ copy (always exists).
//
// The runner uses genPath to populate `<visitorName>-spec` in `exports` so
// downstream tasks have a single stable location even when the committed
// copy is suppressed via options.generate.schema=false. The manifest stores
// that entry project-relative (writeManifest); genPath is absolute because it
// is also the path this process writes to.
func writeSchemaFile(projectPath, genDir string, sf sdkcodegen.SchemaFile, commit bool) (written []string, genPath string, err error) {
	rel := filepath.Clean(sf.RelPath)
	if rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return nil, "", fmt.Errorf("invalid schema relpath: %q", sf.RelPath)
	}
	content := sf.Content
	if openapiutil.IsOpenAPIPath(rel) {
		content, err = openapiutil.Canonicalize(content)
		if err != nil {
			return nil, "", err
		}
	}

	genPath = filepath.Join(genDir, rel)
	if err := os.MkdirAll(filepath.Dir(genPath), 0o755); err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(genPath, content, 0o644); err != nil {
		return nil, "", err
	}
	// .gen/schema is ceded to build-describe, so these bytes are NOT in this
	// task's cache entry. Keep a copy where this task still owns it, so describe
	// sees the same pre-merge tree whether generate ran or was restored. The
	// mirror is never reported in `written` or `exports`: it is not a second
	// location for a consumer, it is this task's half of a hand-off.
	if isCededSchemaRel(rel) {
		mirror := filepath.Join(genDir, generateStagingDir, rel)
		if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
			return nil, "", err
		}
		if err := os.WriteFile(mirror, content, 0o644); err != nil {
			return nil, "", err
		}
	}

	written = []string{filepath.Join(".gen", rel)}

	if commit {
		commitPath := filepath.Join(projectPath, rel)
		if err := os.MkdirAll(filepath.Dir(commitPath), 0o755); err != nil {
			return nil, "", err
		}
		if err := os.WriteFile(commitPath, content, 0o644); err != nil {
			return nil, "", err
		}
		written = append(written, rel)
	}

	return written, genPath, nil
}

// writeManifest writes .gen/generate-result.json and returns the manifest in
// the exact form it serialized, so callers surface the same values in JSONL
// data payloads as the file carries. When result is nil (no visitors
// registered) we still write a manifest so downstream cache keys remain stable.
//
// The visitor contract hands the runner ABSOLUTE paths (see
// sdk/extension/codegen.Result) and that stays true in-process; the conversion
// to project-relative happens here, once, at the only boundary where a path
// leaves this process for a cached artifact.
func writeManifest(projectPath, mode, hash string, result *GenerateResult) (*GenerateResult, error) {
	if result == nil {
		result = &GenerateResult{
			Hash:    hash,
			Mode:    mode,
			Exports: map[string]string{},
			Assets:  map[string]string{},
		}
	}
	relative := &GenerateResult{
		Hash:    result.Hash,
		Mode:    result.Mode,
		Exports: genresult.Relativize(projectPath, result.Exports),
		Assets:  genresult.Relativize(projectPath, result.Assets),
		Schemas: genresult.RelativizeList(projectPath, result.Schemas),
	}

	genDir := filepath.Join(projectPath, ".gen")
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(relative, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(genDir, "generate-result.json"), data, 0o644); err != nil {
		return nil, err
	}
	return relative, nil
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

// resolveModulePath reads the first `module ...` directive from the project's
// go.mod. Empty when the project has no go.mod (e.g., workspace-level tasks).
func resolveModulePath(projectPath string) string {
	data, err := os.ReadFile(filepath.Join(projectPath, "go.mod"))
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "module"))
		}
	}
	return ""
}

// stableVersion returns the workspace's stable base version — the part
// without git SHA / dirty markers — so committed schemas don't churn on
// every commit. ctx.Version.Full is intentionally avoided here; it's the
// right value at runtime but the wrong value baked into a tracked file.
//
// It is still WORKSPACE-wide state: a workspace bump moves it for every
// project. Committed artifacts must therefore use
// configextract.DeclaredProjectVersion; this value is for ephemeral, per-run
// output only.
//
// Order: ctx.Version.Base → ctx.Workspace.Version → "0.0.0".
func stableVersion(ctx *pctx.Context) string {
	if ctx.Version != nil && ctx.Version.Base != "" {
		return ctx.Version.Base
	}
	if ctx.Workspace.Version != "" {
		return ctx.Workspace.Version
	}
	return "0.0.0"
}
