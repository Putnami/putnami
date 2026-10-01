package mapgen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/workspace"
)

// AbsentDigest is the typed absence recorded for a declared input the project
// does not have. It is a VALUE in the digest, not an omission, so "the README
// was deleted" and "the README never existed" are different inputs sets and the
// second cannot be mistaken for the first.
const AbsentDigest = "absent"

// Fragment is one project's map contribution plus the provenance needed to
// validate it: the digest of the inputs it was computed from.
type Fragment struct {
	// FragmentVersion is the shape version (FragmentVersion). A mismatch makes
	// the fragment unusable regardless of its digest.
	FragmentVersion int `json:"fragmentVersion"`
	// Inputs records what this fragment was computed from.
	Inputs FragmentInputs `json:"inputs"`
	// Project is the project-scoped half of a map entry.
	Project FragmentProject `json:"project"`
}

// FragmentInputs is the fragment's cache key, spelled out so a reader can audit
// it rather than trust it.
type FragmentInputs struct {
	// Digest is the single value the reduce compares: sha256 over the identity
	// digest and every file entry, in canonical order.
	Digest string `json:"digest"`
	// Identity is the digest of the workspace-resolved project identity the
	// fragment consumed (id, path, name, type, description, tags, extensions,
	// declared dependency names). It is an INPUT because those values reach the
	// fragment from the loaded workspace (the language provider's probe view),
	// not only from the files below — without it, a renamed TypeScript package or
	// a new dependency edge could leave a matching file digest on a stale entry.
	Identity string `json:"identity"`
	// Files are the project-scoped inputs, sorted by path, each with a content
	// digest or AbsentDigest: the four ProjectInputFiles plus the SchemaDirInput
	// listing entry.
	Files []InputFile `json:"files"`
}

// InputFile is one declared project-scoped input and its content address.
type InputFile struct {
	// Path is the project-relative, slash-formed input path. A trailing slash
	// marks a DIRECTORY LISTING input (SchemaDirInput).
	Path string `json:"path"`
	// Digest is "sha256:<64hex>" for a present file (or, for a listing input, over
	// its sorted name list), AbsentDigest otherwise.
	Digest string `json:"digest"`
}

// FragmentProject is everything about a project that is derivable WITHOUT
// looking at any other project. The reduce turns it into a ProjectEntry by
// resolving Dependencies against the live set and adding the reverse edge.
type FragmentProject struct {
	ID          string   `json:"id"`
	Path        string   `json:"path"`
	Name        string   `json:"name"`
	Type        string   `json:"type,omitempty"`
	Description string   `json:"description,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Readme      string   `json:"readme,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Extensions  []string `json:"extensions,omitempty"`
	// Dependencies are the declared dependency NAMES, not ids: resolving a name
	// to a project is a workspace-wide question and belongs to the reduce.
	Dependencies []string    `json:"dependencies,omitempty"`
	Endpoints    []Endpoint  `json:"endpoints,omitempty"`
	ConfigKeys   []ConfigKey `json:"configKeys,omitempty"`
	// Schemas are the project-relative paths of the JSON artifacts committed
	// directly under SchemaDir, sorted. Covered by the SchemaDirInput listing
	// digest, so adding or removing one invalidates this fragment.
	Schemas []string `json:"schemas,omitempty"`
}

// FragmentPath returns the workspace-relative path of a project's fragment.
func FragmentPath(projectPath string) string {
	return path3(FragmentDir, filepath.ToSlash(projectPath), FragmentFilename)
}

// path3 joins slash-formed path segments without touching the OS separator.
func path3(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.Trim(filepath.ToSlash(p), "/"); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "/")
}

// readInputs reads the declared project-scoped inputs once, returning the digest
// record, the file contents keyed by project-relative path (absent files are
// simply missing from the content map), and the schema listing.
//
// Reading is unconditional even when the caller only wants to VALIDATE a
// persisted fragment: a digest that is not computed from the bytes on disk is
// not a digest. What validation saves is the parsing and scanning — the openapi
// walk, the JSON-Schema flatten, the README scan — not the reads.
func readInputs(projDir string) (FragmentInputs, map[string][]byte, []string, error) {
	files := make([]InputFile, 0, len(ProjectInputFiles)+1)
	contents := make(map[string][]byte, len(ProjectInputFiles))

	rels := append([]string(nil), ProjectInputFiles...)
	sort.Strings(rels)
	for _, rel := range rels {
		data, err := os.ReadFile(filepath.Join(projDir, filepath.FromSlash(rel)))
		if err != nil {
			if os.IsNotExist(err) {
				files = append(files, InputFile{Path: rel, Digest: AbsentDigest})
				continue
			}
			return FragmentInputs{}, nil, nil, fmt.Errorf("read map input %s: %w", rel, err)
		}
		contents[rel] = data
		files = append(files, InputFile{Path: rel, Digest: digestBytes(data)})
	}

	schemas, listing, err := readSchemaListing(projDir)
	if err != nil {
		return FragmentInputs{}, nil, nil, err
	}
	files = append(files, listing)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return FragmentInputs{Files: files}, contents, schemas, nil
}

// readSchemaListing lists the JSON artifacts committed directly under
// <project>/schema/ (non-recursive: every such directory in this tree is flat)
// and returns them as sorted project-relative paths, together with the input
// entry that covers them.
//
// The digest is over the sorted NAME LIST, never the file bytes: names are all
// the fragment emits, so a content edit to an unparsed schema must leave the
// fragment valid — while adding or removing a schema must invalidate it, which
// is precisely what a listing digest does and what four file digests cannot.
//
// A missing (or non-directory) schema/ is a typed ABSENCE for the same reason an
// absent file is: "the directory was emptied" and "there never was one" must not
// hash alike.
func readSchemaListing(projDir string) ([]string, InputFile, error) {
	dir := filepath.Join(projDir, filepath.FromSlash(SchemaDir))
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, InputFile{Path: SchemaDirInput, Digest: AbsentDigest}, nil //nolint:nilerr // no schema/ directory is an absence, not a failure
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, InputFile{}, fmt.Errorf("read map input %s: %w", SchemaDirInput, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var listing strings.Builder
	rels := make([]string, 0, len(names))
	for _, name := range names {
		listing.WriteString(name)
		listing.WriteString("\n")
		rels = append(rels, SchemaDir+"/"+name)
	}
	if len(rels) == 0 {
		rels = nil
	}
	return rels, InputFile{Path: SchemaDirInput, Digest: digestBytes([]byte(listing.String()))}, nil
}

// identityView is the normalized projection of the workspace-resolved project
// identity the fragment consumes. It is serialized canonically and hashed into
// FragmentInputs.Identity; the field set here IS the contract, so adding a field
// to FragmentProject that comes from the workspace (rather than from a file)
// requires adding it here too or the digest stops covering the fragment.
type identityView struct {
	ID           string   `json:"id"`
	Path         string   `json:"path"`
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	Description  string   `json:"description"`
	Tags         []string `json:"tags"`
	Extensions   []string `json:"extensions"`
	Dependencies []string `json:"dependencies"`
}

// projectIdentity normalizes a workspace project into the identity view: sorted,
// deduped, slash-formed, with the authored description folded in.
func projectIdentity(p *workspace.Project) identityView {
	description := ""
	if p.Config != nil {
		description = strings.TrimSpace(p.Config.Description)
	}
	return identityView{
		ID:           p.ID,
		Path:         filepath.ToSlash(p.Path),
		Name:         p.Name,
		Type:         p.Type,
		Description:  description,
		Tags:         sortedSet(p.Tags),
		Extensions:   sortedSet(p.Extensions),
		Dependencies: sortedSet(p.Dependencies),
	}
}

// digestIdentity hashes the canonical JSON of the identity view. json.Marshal of
// a struct emits fields in declaration order, and every slice is already sorted,
// so the bytes are stable.
func digestIdentity(view identityView) (string, error) {
	data, err := json.Marshal(view)
	if err != nil {
		return "", fmt.Errorf("serialize project identity: %w", err)
	}
	return digestBytes(data), nil
}

// digestInputs folds the identity digest and every file entry into the single
// value the reduce compares. The serialization is explicit (length-free but
// NUL-separated and newline-terminated per record, over a sorted file list) so
// two different input sets cannot collide by concatenation.
func digestInputs(in FragmentInputs) string {
	var sb strings.Builder
	sb.WriteString("identity\x00")
	sb.WriteString(in.Identity)
	sb.WriteString("\n")
	for _, f := range in.Files {
		sb.WriteString(f.Path)
		sb.WriteString("\x00")
		sb.WriteString(f.Digest)
		sb.WriteString("\n")
	}
	return digestBytes([]byte(sb.String()))
}

// BuildFragment computes a project's fragment from its four declared inputs plus
// its workspace-resolved identity. It is a pure function of those inputs: no
// timestamps, no absolute paths, no CLI version.
func BuildFragment(wsRoot string, p *workspace.Project) (*Fragment, error) {
	inputs, contents, schemas, view, err := digestFragmentInputs(wsRoot, p)
	if err != nil {
		return nil, err
	}
	return assembleFragment(p, view, inputs, contents, schemas)
}

// digestFragmentInputs is the CHEAP half of a fragment build: it reads the
// declared input files and computes the inputs digest, without parsing any of
// them. It exists so ResolveFragment can decide a cache hit from the digest
// alone — the digest is a pure function of the raw bytes plus the resolved
// identity, so it never needs the parsed forms.
func digestFragmentInputs(wsRoot string, p *workspace.Project) (FragmentInputs, map[string][]byte, []string, identityView, error) {
	projDir := filepath.Join(wsRoot, filepath.FromSlash(p.Path))
	inputs, contents, schemas, err := readInputs(projDir)
	if err != nil {
		return FragmentInputs{}, nil, nil, identityView{}, err
	}
	view := projectIdentity(p)
	identity, err := digestIdentity(view)
	if err != nil {
		return FragmentInputs{}, nil, nil, identityView{}, err
	}
	inputs.Identity = identity
	inputs.Digest = digestInputs(inputs)
	return inputs, contents, schemas, view, nil
}

// assembleFragment is the EXPENSIVE half: it parses the README summary, the
// OpenAPI document, and the config schema out of the already-read contents.
// ResolveFragment runs it only on a cache miss.
func assembleFragment(p *workspace.Project, view identityView, inputs FragmentInputs, contents map[string][]byte, schemas []string) (*Fragment, error) {
	projPath := filepath.ToSlash(p.Path)

	project := FragmentProject{
		ID:           view.ID,
		Path:         view.Path,
		Name:         view.Name,
		Type:         view.Type,
		Description:  view.Description,
		Tags:         view.Tags,
		Extensions:   view.Extensions,
		Dependencies: view.Dependencies,
		Schemas:      schemas,
	}
	if readme, ok := contents["README.md"]; ok {
		project.Readme = path3(projPath, "README.md")
		project.Summary = readmeSummary(readme)
	}
	if openapi, ok := contents["schema/openapi.json"]; ok {
		endpoints, err := parseOpenAPI(openapi)
		if err != nil {
			return nil, fmt.Errorf("parse %s/schema/openapi.json: %w", projPath, err)
		}
		project.Endpoints = endpoints
	}
	if schema, ok := contents["schema/config.jsonschema.json"]; ok {
		keys, err := parseConfigSchema(schema)
		if err != nil {
			return nil, fmt.Errorf("parse %s/schema/config.jsonschema.json: %w", projPath, err)
		}
		project.ConfigKeys = keys
	}

	return &Fragment{FragmentVersion: FragmentVersion, Inputs: inputs, Project: project}, nil
}

// CanonicalFragment serializes a fragment to its on-disk bytes: two-space
// indented JSON with a trailing newline, the same canonical form the other
// committed CLI artifacts use.
func CanonicalFragment(f *Fragment) ([]byte, error) {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("serialize map fragment: %w", err)
	}
	return append(data, '\n'), nil
}

// ResolveFragment returns the fragment for p and whether it was REUSED from disk.
//
// A persisted fragment is reused only when it parses, carries the current
// FragmentVersion, and its recorded inputs digest still equals the digest of the
// inputs on disk right now. Anything else — missing, unreadable, malformed, old
// version, stale digest — rebuilds in memory. Drift is therefore not something
// the caller can observe after the fact: a fragment that does not match its
// inputs is never used.
//
// The digest is computed and compared BEFORE any parsing happens: a hit costs
// the input reads and the hash, never the README summary, OpenAPI, or config
// schema parse — which is what makes the warm path O(changed) instead of
// O(projects).
func ResolveFragment(wsRoot string, p *workspace.Project) (*Fragment, bool, error) {
	inputs, contents, schemas, view, err := digestFragmentInputs(wsRoot, p)
	if err != nil {
		return nil, false, err
	}
	stored := readStoredFragment(filepath.Join(wsRoot, filepath.FromSlash(FragmentPath(p.Path))))
	if stored != nil && stored.FragmentVersion == FragmentVersion &&
		stored.Inputs.Digest != "" && stored.Inputs.Digest == inputs.Digest {
		return stored, true, nil
	}
	fresh, err := assembleFragment(p, view, inputs, contents, schemas)
	if err != nil {
		return nil, false, err
	}
	return fresh, false, nil
}

// readStoredFragment reads and parses a persisted fragment, or returns nil.
//
// It deliberately reports NO error: a fragment is a cache, so a missing,
// unreadable, or malformed entry is a miss to be recomputed, never a condition
// that fails the build that happened to notice it.
func readStoredFragment(abs string) *Fragment {
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil
	}
	var f Fragment
	if err := json.Unmarshal(data, &f); err != nil {
		return nil
	}
	return &f
}

// digestBytes returns the canonical "sha256:<64hex>" content address of data.
func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// sortedSet returns the sorted, deduped, blank-free copy of items, or nil when
// nothing survives — the one normalization every emitted string list uses, so
// declaration order can never reach the output bytes.
func sortedSet(items []string) []string {
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" || seen[it] {
			continue
		}
		seen[it] = true
		out = append(out, it)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}
