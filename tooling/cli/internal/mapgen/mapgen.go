// Package mapgen builds the workspace orientation map in memory from artifacts
// that already exist on disk, as a MAP-REDUCE. It writes nothing: `putnami
// context map` prints the map.
//
// The map is a pure projection of committed inputs, so storing it bought
// nothing and cost a drift class: every reader rebuilds it from the working
// tree.
//
// The map stage produces one FRAGMENT per project, computed from PROJECT-SCOPED
// inputs only — putnami.json, schema/openapi.json, schema/config.jsonschema.json,
// README.md, and the names of the JSON files under schema/ — plus the
// workspace-resolved identity of that project (its id, path, name, type, tags,
// extensions, and declared dependency names).
//
// The reduce stage takes the LIVE project set (enumerated from the workspace
// manifest), the fragments, and the genuinely global inputs (the workspace name
// and the indexed documents — the workspace-root docs/**/*.md tree plus every
// non-project-root README.md), and renders the two documents with entries sorted
// by project path. It also derives the INTERCALL edges, which are a property of
// the whole live set (a URL-shaped config key only names a callee once it
// resolves against the other projects), so they belong here and not in a
// fragment. Deletions and renames are therefore
// STRUCTURAL: a project that is no longer in the live set contributes nothing,
// with no diffing logic. The reduce NEVER reads the previously rendered
// documents — the render is always a total function of the full fragment set,
// which is what makes "regenerate from scratch always agrees" hold (the issue
// explicitly rejects in-place patching, because patching makes the output a
// function of the previous output and reintroduces drift as a failure mode).
//
// Determinism is the point, so nothing here may leak an ambient value into the
// bytes: no timestamps, no absolute paths, no map iteration order, and no CLI
// version stamp. It is what makes two prints of the map the same bytes at a
// fixed tree. Every collection is sorted on a total key before it is
// emitted.
package mapgen

// SchemaVersion is the shape version of the map document. It is a CONSTANT, not
// a build stamp: bumping it is a deliberate format change. The document is
// printed by `putnami context map --output=json`, so the version is a wire
// contract a consumer may branch on — which is exactly why a CLI release must
// never move it.
const SchemaVersion = 1

// GeneratedBy names the producer in the document. Version-free on purpose (see
// SchemaVersion).
const GeneratedBy = "putnami context map"

// Regenerate is the command a reader runs to print the map again.
const Regenerate = "putnami context map"

// ProjectInputFiles are the four project-scoped FILE inputs a fragment is
// computed from.
var ProjectInputFiles = []string{
	"README.md",
	"putnami.json",
	"schema/config.jsonschema.json",
	"schema/openapi.json",
}

// SchemaDir is the project-relative directory whose committed JSON artifacts a
// fragment LISTS (ProjectEntry.Schemas). It is a fifth input of a different
// kind: the fragment emits the file NAMES, never their content.
const SchemaDir = "schema"

// RepoMap is the reduced orientation map — the document `putnami context map`
// prints, as JSON or Markdown.
type RepoMap struct {
	// SchemaVersion is the document shape version (SchemaVersion).
	SchemaVersion int `json:"schemaVersion"`
	// Workspace is the workspace name from putnami.workspace.json.
	Workspace string `json:"workspace,omitempty"`
	// GeneratedBy names the producer; Regenerate names the refresh command.
	GeneratedBy string `json:"generatedBy"`
	Regenerate  string `json:"regenerate"`
	// Projects are the live workspace projects, sorted by Path.
	Projects []ProjectEntry `json:"projects"`
	// Docs indexes the workspace-root docs tree, sorted by Path. Empty when the
	// workspace has no docs/ directory.
	Docs []DocEntry `json:"docs,omitempty"`
}

// ProjectEntry is one project's reduced contribution: everything the fragment
// carried, plus the two fields only the reduce can know (resolved dependency ids
// and the reverse dependents edge).
type ProjectEntry struct {
	// ID is the canonical workspace project id ("/tooling/cli").
	ID string `json:"id"`
	// Path is the workspace-relative project path, the sort key of the map.
	Path string `json:"path"`
	// Name is the project's resolved name.
	Name string `json:"name"`
	// Type is "library"/"application"/… as declared; empty when unset.
	Type string `json:"type,omitempty"`
	// Description is the authored putnami.json description.
	Description string `json:"description,omitempty"`
	// Summary is the first prose line of the project README.
	Summary string `json:"summary,omitempty"`
	// Readme is the workspace-relative README path, when the project has one.
	Readme string `json:"readme,omitempty"`
	// Tags and Extensions are the project's sorted, deduped declarations.
	Tags       []string `json:"tags,omitempty"`
	Extensions []string `json:"extensions,omitempty"`
	// DependsOn are the workspace project ids this project depends on, resolved
	// from the declared dependency NAMES against the live project set (a name
	// that resolves to no live project — an external dependency — is dropped).
	DependsOn []string `json:"dependsOn,omitempty"`
	// Dependents is the reverse edge, computed by the reduce over DependsOn.
	Dependents []string `json:"dependents,omitempty"`
	// Intercalls are the outbound RUNTIME edges derived from this project's
	// URL-shaped config keys, resolved against the live project set, sorted by To.
	Intercalls []Intercall `json:"intercalls,omitempty"`
	// CalledBy is the reverse intercall edge, computed by the reduce exactly like
	// Dependents — it is what makes "who calls X at runtime" a lookup.
	CalledBy []string `json:"calledBy,omitempty"`
	// Endpoints is the endpoint table from the committed schema/openapi.json.
	Endpoints []Endpoint `json:"endpoints,omitempty"`
	// ConfigKeys are the flattened keys of the committed
	// schema/config.jsonschema.json.
	ConfigKeys []ConfigKey `json:"configKeys,omitempty"`
	// Schemas are the project-relative paths of the JSON artifacts committed
	// directly under SchemaDir, sorted.
	Schemas []string `json:"schemas,omitempty"`
}

// Intercall is one outbound runtime edge: this project addresses another
// workspace project through configuration.
//
// It carries the EVIDENCE and nothing else. The retired repo-local generator
// this generalizes also emitted a protocol, a purpose, and a credential per
// edge; every one of those was a hardcoded guess keyed on substrings, and a
// guess rendered as a fact is worse than an absence. A reader gets the target
// and the config key(s) that imply it, and judges the edge itself.
type Intercall struct {
	// To is the callee's workspace project id.
	To string `json:"to"`
	// ConfigKeys are the URL-shaped keys that resolved to To, sorted.
	ConfigKeys []string `json:"configKeys"`
}

// Endpoint is one operation from a committed schema/openapi.json.
type Endpoint struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	OperationID string `json:"operationId,omitempty"`
	Summary     string `json:"summary,omitempty"`
}

// ConfigKey is one flattened key from a committed schema/config.jsonschema.json.
type ConfigKey struct {
	Key  string `json:"key"`
	Type string `json:"type,omitempty"`
}

// DocEntry is one indexed workspace document.
type DocEntry struct {
	Path  string `json:"path"`
	Title string `json:"title,omitempty"`
}
