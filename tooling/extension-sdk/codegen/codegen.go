// Package codegen defines the build-time code generation contract used by the
// Putnami generate phase.
//
// A visitor walks the parsed AST of a project's Go source, decides whether it
// applies (typically by checking whether the relevant framework is imported),
// and returns artifacts to write under .gen/ and (optionally) committed paths
// like schema/.
//
// Visitors are registered once per process, usually from an init() in a
// codegen subpackage that the extension binary blank-imports. The extension's
// build-generate job loads the project once and dispatches every registered
// visitor against the same parsed source set, so adding a new generator costs
// one Register call rather than a new subprocess.
//
//	package mygen
//
//	import "go.putnami.dev/sdk/extension/codegen"
//
//	func init() {
//	    codegen.Register(&visitor{})
//	}
//
//	type visitor struct{}
//
//	func (v *visitor) Name() string { return "mygen" }
//	func (v *visitor) Visit(g *codegen.Generation) (*codegen.Result, error) {
//	    // walk g.Files, return Result{...}
//	}
package codegen

import (
	"encoding/json"
	"go/ast"
	"go/token"
	"sync"
)

// Visitor produces generated artifacts from a project's parsed Go source.
//
// Implementations should self-gate: if the relevant framework isn't imported
// by the project, return an empty Result without an error. The runner skips
// visitors whose Result is empty so unused generators have zero footprint.
type Visitor interface {
	// Name is a stable identifier used in logs, progress events, and as a
	// disambiguator if multiple visitors emit overlapping artifacts.
	Name() string

	// Visit inspects g.Files and returns the artifacts to write. The runner
	// is responsible for writing files; visitors only need to populate Result.
	Visit(g *Generation) (*Result, error)
}

// Generation is the read-only input handed to each visitor for a single run.
type Generation struct {
	// ProjectRoot is the absolute path to the project being generated for.
	ProjectRoot string

	// GenDir is <ProjectRoot>/.gen, guaranteed to exist before visitors run.
	GenDir string

	// Mode is "build", "serve", or "test" — the lifecycle phase that triggered
	// generation. Visitors can use it to skip expensive work in serve mode.
	Mode string

	// Project carries orchestrator-supplied project metadata.
	Project ProjectInfo

	// Fset and Files contain the parsed (untyped) project source. Files maps
	// absolute path -> *ast.File. Test files are excluded.
	Fset  *token.FileSet
	Files map[string]*ast.File

	// Emit lets visitors surface progress and log events to the orchestrator.
	// Use it instead of fmt.Println so output flows through the JSONL channel.
	Emit ProgressEmitter
}

// Result is what a Visitor returns after processing a Generation.
//
// SchemaFiles are the primary output — files that have a stable build-only
// path under .gen/ and an optional committed copy in the project tree (e.g.,
// schema/openapi.json). The runner writes both copies, respects the project's
// opt-out preference for the committed copy, and adds an entry to the
// manifest's `exports` keyed by `<visitorName>-spec` per schema file so
// visitors don't have to think about which copy actually exists.
//
// Exports and Assets are merged into .gen/generate-result.json. They are for
// non-schema artifacts (e.g. generated Go source another build step imports).
// Visitors should NOT add their own entries for SchemaFiles — let the runner
// do it so the path always points at a file that exists on disk.
//
// A visitor reports ABSOLUTE paths here; that is the in-process contract and it
// does not change. What the runner SERIALIZES is project-relative, because the
// manifest is a cached artifact that gets restored into other checkouts — see
// go.putnami.dev/sdk/extension/genresult.
type Result struct {
	// SchemaFiles are committed-by-default generated specs (openapi.json,
	// proto schemas, config schemas, etc.). Always written under .gen/<RelPath>;
	// also written to <ProjectRoot>/<RelPath> unless the project opts out.
	SchemaFiles []SchemaFile

	// Exports maps a logical key to an absolute path of a generated artifact
	// (typically a Go file) that the build/serve pipeline imports. Schema
	// files are tracked separately by the runner — don't list them here.
	Exports map[string]string

	// Assets behaves like Exports but for non-code artifacts (JSON, .gz, etc.).
	Assets map[string]string
}

// IsEmpty reports whether the visitor produced nothing — used by the runner
// to skip writing artifacts and logging "generated 0 files".
func (r *Result) IsEmpty() bool {
	if r == nil {
		return true
	}
	return len(r.SchemaFiles) == 0 && len(r.Exports) == 0 && len(r.Assets) == 0
}

// SchemaFile is a generated spec with a project-relative path. The runner
// writes it to .gen/<RelPath> and (unless opted out) to <ProjectRoot>/<RelPath>.
type SchemaFile struct {
	// RelPath is the path inside the project (e.g., "schema/openapi.json").
	RelPath string

	// Content is the file body, typically JSON-encoded.
	Content []byte
}

// ProjectInfo is the slice of orchestrator context useful to visitors. The
// runner populates this from the job's pctx.Context so visitors don't need
// to depend on the full SDK context type.
type ProjectInfo struct {
	// Name is the project name from putnami.json (e.g., "go.putnami.dev/examples/tasks-api").
	Name string

	// Version is the resolved workspace version, or "" when unknown.
	//
	// It is WORKSPACE-derived state: every project in the workspace sees the same
	// value, so a workspace version bump changes it for all of them. Never stamp
	// it into an artifact the project COMMITS — that turns one workspace bump into
	// a re-stamp of every tracked generated file. Use DeclaredVersion for
	// committed content and keep this for ephemeral, per-run output.
	Version string

	// DeclaredVersion is the version the project declares for ITSELF in its own
	// putnami.json ("version"), or "" when it declares none and inherits the scope
	// or workspace version.
	//
	// This is the only version a committed generated artifact may carry: it is a
	// declared input of the project that owns the artifact, so the artifact's
	// content depends on that project alone.
	DeclaredVersion string

	// Module is the Go module path resolved from go.mod.
	Module string

	// Options carries the raw project.options map from putnami.json so
	// visitors can read their own option keys.
	Options map[string]json.RawMessage
}

// ProgressEmitter is the small subset of the JSONL emitter visitors need.
// Keeping it minimal lets visitors stay decoupled from the full emitter type
// and makes unit tests trivial to fake.
type ProgressEmitter interface {
	Log(level, message string)
	Progress(current, total int, message string)
}

// --- Registry ---

var (
	registryMu sync.RWMutex
	registry   []Visitor
)

// Register adds a visitor to the global registry. Call from init() in the
// visitor's package.
func Register(v Visitor) {
	if v == nil {
		return
	}
	registryMu.Lock()
	registry = append(registry, v)
	registryMu.Unlock()
}

// Visitors returns the registered visitors in registration order.
func Visitors() []Visitor {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Visitor, len(registry))
	copy(out, registry)
	return out
}

// resetForTest clears the registry. Intended for tests in this package.
func resetForTest() {
	registryMu.Lock()
	registry = nil
	registryMu.Unlock()
}
