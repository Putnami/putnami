package store

import (
	"os"
	"path/filepath"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

// configCase is one project-config body and what it changes, for the tables
// below. A named type keeps each row on one line, where the body is readable
// as the file a user would author.
type configCase struct {
	what string
	body string
}

// TestProjectConfigScope_KeepsWhatTheTaskReads pins which bytes of a project
// config reach one task's cache key.
//
// A project config is addressed to several extensions at once. The layers this
// extension owns decide what its tasks do and must move the key; another
// extension's layers cannot, and hashing them re-runs a task for bytes it never
// reads. Everything else stays: identity fields decide what the task builds,
// and a namespace the hasher cannot attribute to an extension — a command layer
// or a bare name an extension reads for itself — is kept rather than guessed
// away.
func TestProjectConfigScope_KeepsWhatTheTaskReads(t *testing.T) {
	t.Parallel()
	for _, filename := range []string{wsproto.ConfigFilename, wsproto.LegacyConfigFilename} {
		t.Run(filename, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, filename)
			scope := ProjectConfigScope{ExtensionLayers: []string{"@putnami/go", "/go/extension"}}
			write := func(body string) string {
				t.Helper()
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				hash, err := hashFiles(dir, []string{filename}, scope)
				if err != nil {
					t.Fatal(err)
				}
				return hash
			}

			base := write(`{"name":"proj","tags":["go"],"options":{"@putnami/go":{"race":true}}}`)

			moves := []configCase{
				{"project identity", `{"name":"renamed","tags":["go"],"options":{"@putnami/go":{"race":true}}}`},
				{"project tags", `{"name":"proj","tags":["go","service"],"options":{"@putnami/go":{"race":true}}}`},
				{"own extension layer", `{"name":"proj","tags":["go"],"options":{"@putnami/go":{"race":false}}}`},
				{"own extension command layer", `{"name":"proj","tags":["go"],"options":{"@putnami/go":{"race":true},"@putnami/go:test":{"short":true}}}`},
				{"own extension path reference", `{"name":"proj","tags":["go"],"options":{"@putnami/go":{"race":true},"/go/extension":{"filePatterns":["doc/**"]}}}`},
				{"command layer", `{"name":"proj","tags":["go"],"options":{"@putnami/go":{"race":true},"publish":{"archives":false}}}`},
				{"unattributable namespace", `{"name":"proj","tags":["go"],"options":{"@putnami/go":{"race":true},"sdd":{"verification":{"specs":"enforce"}}}}`},
			}
			for _, tc := range moves {
				if got := write(tc.body); got == base {
					t.Errorf("%s left the digest unmoved; the task reads it", tc.what)
				}
			}

			// Back to the baseline body, then the layers that must NOT move it.
			if got := write(`{"name":"proj","tags":["go"],"options":{"@putnami/go":{"race":true}}}`); got != base {
				t.Fatalf("rewriting the baseline body produced %s, want %s", got, base)
			}
			stays := []configCase{
				{"another extension's layer", `{"name":"proj","tags":["go"],"options":{"@putnami/go":{"race":true},"@putnami/cloud":{"region":"eu"}}}`},
				{"another extension's command layer", `{"name":"proj","tags":["go"],"options":{"@putnami/go":{"race":true},"@putnami/cloud:publish-config":{"env":"prod"}}}`},
				{"another extension's path reference", `{"name":"proj","tags":["go"],"options":{"@putnami/go":{"race":true},"/typescript/extension":{"filePatterns":["src/**"]}}}`},
				{"execution-only task tuning", `{"name":"proj","tags":["go"],"options":{"@putnami/go":{"race":true}},"tasks":{"lint":{"cpuWeight":4}}}`},
			}
			for _, tc := range stays {
				if got := write(tc.body); got != base {
					t.Errorf("%s moved the digest: %s != %s", tc.what, got, base)
				}
			}
		})
	}
}

// TestProjectConfigScope_EmptyScopeKeepsTheWholeFile pins the fail-closed
// default. A caller with no task in hand — `cache verify`, any hasher that does
// not know whose key it is building — keeps every option layer, so the digest it
// produces is the WIDER of the two and can only cost a miss.
func TestProjectConfigScope_EmptyScopeKeepsTheWholeFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, wsproto.ConfigFilename)
	write := func(body string) (scoped, unscoped string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		scopedHash, err := hashFiles(dir, []string{wsproto.ConfigFilename},
			ProjectConfigScope{ExtensionLayers: []string{"@putnami/go"}})
		if err != nil {
			t.Fatal(err)
		}
		unscopedHash, err := hashFiles(dir, []string{wsproto.ConfigFilename}, ProjectConfigScope{})
		if err != nil {
			t.Fatal(err)
		}
		return scopedHash, unscopedHash
	}

	_, baseUnscoped := write(`{"name":"proj","options":{"@putnami/cloud":{"region":"eu"}}}`)
	scoped, unscoped := write(`{"name":"proj","options":{"@putnami/cloud":{"region":"us"}}}`)
	if unscoped == baseUnscoped {
		t.Error("the unscoped digest ignored an option edit; it must keep the whole file")
	}
	if scoped == unscoped {
		t.Error("the scoped and unscoped digests agree on a config whose only options are foreign; " +
			"one of them is not reading what it claims")
	}

	// A config whose only options address other extensions says the same thing,
	// for this task, as one that declares none.
	if err := os.WriteFile(path, []byte(`{"name":"proj"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	bare, err := hashFiles(dir, []string{wsproto.ConfigFilename},
		ProjectConfigScope{ExtensionLayers: []string{"@putnami/go"}})
	if err != nil {
		t.Fatal(err)
	}
	if bare != scoped {
		t.Error("a config carrying only other extensions' options keyed differently from one carrying none")
	}
}

// TestProjectConfigScope_MalformedConfigIsHashedRaw pins the shape the hasher
// refuses to reinterpret: a config it cannot decode, and an `options` member
// that is not an object, are hashed as bytes. Scoping a shape the loader itself
// would reject would drop bytes on a guess.
func TestProjectConfigScope_MalformedConfigIsHashedRaw(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, wsproto.ConfigFilename)
	scope := ProjectConfigScope{ExtensionLayers: []string{"@putnami/go"}}
	write := func(body string) string {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		hash, err := hashFiles(dir, []string{wsproto.ConfigFilename}, scope)
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}

	broken := write(`{"name":"proj",`)
	if edited := write(`{"name":"other",`); edited == broken {
		t.Error("an edit to an undecodable config left the digest unmoved")
	}
	list := write(`{"name":"proj","options":["@putnami/cloud"]}`)
	if edited := write(`{"name":"proj","options":["@putnami/typescript"]}`); edited == list {
		t.Error("an edit to a non-object options member left the digest unmoved")
	}
}

// TestProjectConfigScope_MemoSeparatesScopes pins that the per-session file-hash
// memo keys on the scope. Two tasks of two extensions hash the same file with
// two projections; one memo entry for both would serve the first task's digest
// as the second's identity.
func TestProjectConfigScope_MemoSeparatesScopes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `{"name":"proj","options":{"@putnami/go":{"race":true},"@putnami/typescript":{"minify":true}}}`
	if err := os.WriteFile(filepath.Join(dir, wsproto.ConfigFilename), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cm := NewCacheManager(NewLocalStore(t.TempDir()))
	patterns := []string{wsproto.ConfigFilename}

	goHash, err := cm.HashFiles(dir, patterns, ProjectConfigScope{ExtensionLayers: []string{"@putnami/go"}})
	if err != nil {
		t.Fatal(err)
	}
	tsHash, err := cm.HashFiles(dir, patterns, ProjectConfigScope{ExtensionLayers: []string{"@putnami/typescript"}})
	if err != nil {
		t.Fatal(err)
	}
	if goHash == tsHash {
		t.Error("two extensions' scopes produced one digest for a config that addresses them differently")
	}

	// The memo must still be a memo: the same scope, spelled in another order,
	// is the same value.
	repeat, err := cm.HashFiles(dir, patterns, ProjectConfigScope{ExtensionLayers: []string{"@putnami/go"}})
	if err != nil {
		t.Fatal(err)
	}
	if repeat != goHash {
		t.Errorf("the same scope produced two digests: %s != %s", repeat, goHash)
	}
}

// TestProjectConfigScope_DeclaredNamespaces pins the half a manifest decides.
//
// A bare `options.<name>` block is dropped only when an extension DECLARED it
// (`optionNamespaces`) and the hashing task belongs to a different one. A
// namespace nobody declares, and one this extension declares too, stays: the
// first is unattributed and the second is a real input.
func TestProjectConfigScope_DeclaredNamespaces(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, wsproto.ConfigFilename)
	scope := ProjectConfigScope{
		ExtensionLayers:   []string{"@putnami/go"},
		ForeignNamespaces: []string{"sdd"},
	}
	write := func(body string) string {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		hash, err := hashFiles(dir, []string{wsproto.ConfigFilename}, scope)
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}

	base := write(`{"name":"proj","options":{"sdd":{"specs":"enforce"},"agent-artifact":{"keep":1}}}`)
	if got := write(`{"name":"proj","options":{"sdd":{"specs":"report"},"agent-artifact":{"keep":1}}}`); got != base {
		t.Error("a namespace another extension declared moved the key; this task cannot read it")
	}
	if got := write(`{"name":"proj","options":{"sdd":{"specs":"enforce"},"agent-artifact":{"keep":2}}}`); got == base {
		t.Error("an undeclared namespace left the key unmoved; nothing says who reads it")
	}

	// The same namespace, declared by THIS extension too, is an input of both
	// and belongs in both keys — the resolver leaves it out of ForeignNamespaces.
	shared := ProjectConfigScope{ExtensionLayers: []string{"@putnami/go"}}
	body := `{"name":"proj","options":{"sdd":{"specs":"enforce"}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := hashFiles(dir, []string{wsproto.ConfigFilename}, shared)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"name":"proj","options":{"sdd":{"specs":"report"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := hashFiles(dir, []string{wsproto.ConfigFilename}, shared)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Error("a namespace this extension also declares was dropped; it is an input of both")
	}
}

// A project config a job reads ACROSS its project root — the CLI's own test
// declares `../../**/putnami.json` — is read as a whole document, so it is
// hashed whole. The scope narrows only the configs of the project being hashed.
func TestHashFiles_AForeignProjectConfigIsHashedWhole(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	scope := ProjectConfigScope{ExtensionLayers: []string{"@putnami/go"}}
	patterns := []string{"putnami.json", "../other/putnami.json"}
	digest := func() string {
		t.Helper()
		got, err := hashFiles(filepath.Join(root, "proj"), patterns, scope)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	write("proj/putnami.json", `{"name":"proj","options":{"@putnami/typescript":{"x":1}}}`)
	write("other/putnami.json", `{"name":"other","options":{"@putnami/typescript":{"x":1}}}`)
	base := digest()

	write("proj/putnami.json", `{"name":"proj","options":{"@putnami/typescript":{"x":2}}}`)
	if got := digest(); got != base {
		t.Errorf("another extension's block in the hashed project's own config moved the key")
	}
	write("other/putnami.json", `{"name":"other","options":{"@putnami/typescript":{"x":2}}}`)
	if got := digest(); got == base {
		t.Errorf("a config read across the project root was hashed through the job's scope")
	}
}

// TestProjectConfigScope_VerbatimReadsEveryByte pins the reading mode of a
// task that rewrites its sources. Such a task reads a config as text, so every
// byte is an input: layout, the tasks block, and another extension's options
// all move its digest. The same edits leave a projected scope's digest alone,
// and the memo keeps the two readings apart.
func TestProjectConfigScope_VerbatimReadsEveryByte(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, wsproto.ConfigFilename)
	verbatim := ProjectConfigScope{Verbatim: true}
	projected := ProjectConfigScope{ExtensionLayers: []string{"@putnami/go"}}
	write := func(body string) (verbatimHash, projectedHash string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		verbatimHash, err := hashFiles(dir, []string{wsproto.ConfigFilename}, verbatim)
		if err != nil {
			t.Fatal(err)
		}
		projectedHash, err = hashFiles(dir, []string{wsproto.ConfigFilename}, projected)
		if err != nil {
			t.Fatal(err)
		}
		return verbatimHash, projectedHash
	}

	baseVerbatim, baseProjected := write("{\n  \"name\": \"proj\",\n  \"tags\": [\"go\"]\n}\n")
	for _, tc := range []configCase{
		{"layout only", "{\n  \"name\": \"proj\",\n  \"tags\": [\n    \"go\"\n  ]\n}\n"},
		{"execution-only task tuning", "{\n  \"name\": \"proj\",\n  \"tags\": [\"go\"],\n  \"tasks\": {\"lint\": {\"cpuWeight\": 4}}\n}\n"},
		{"another extension's layer", "{\n  \"name\": \"proj\",\n  \"tags\": [\"go\"],\n  \"options\": {\"@putnami/cloud\": {\"region\": \"eu\"}}\n}\n"},
	} {
		gotVerbatim, gotProjected := write(tc.body)
		if gotVerbatim == baseVerbatim {
			t.Errorf("%s left the verbatim digest unmoved; a source rewriter reads those bytes", tc.what)
		}
		if gotProjected != baseProjected {
			t.Errorf("%s moved the projected digest; a configuration reader ignores it", tc.what)
		}
	}

	// One file, two readings, one memo: the verbatim reading must not answer
	// for the projected one or the reverse.
	cm := NewCacheManager(NewLocalStore(t.TempDir()))
	patterns := []string{wsproto.ConfigFilename}
	memoVerbatim, err := cm.HashFiles(dir, patterns, verbatim)
	if err != nil {
		t.Fatal(err)
	}
	memoWhole, err := cm.HashFiles(dir, patterns, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if memoVerbatim == memoWhole {
		t.Error("the memo served one digest for the verbatim and the projected reading of the same file")
	}
}

// A config whose bytes are exactly the decoded view's canonical JSON — compact,
// sorted keys, no trailing newline — still keys differently when read verbatim.
// Otherwise an entry recorded under the decoded reading would answer for the
// verbatim one, and a formatter would be skipped for a file it rewrites.
func TestProjectConfigScope_VerbatimIsNotTheDecodedDigest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, wsproto.ConfigFilename),
		[]byte(`{"name":"proj","tags":["ts"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	key := func(patterns []string, scope ProjectConfigScope) string {
		t.Helper()
		k := BuildCacheKey("@test/ext", "1.0.0", "", "", "lint-format", "", "proj", "", "", nil, nil,
			dir, "", CacheKeyPolicy{Files: patterns, ConfigScope: scope}, nil)
		got, err := k.ComputeHashUsing(NewCacheManager(nil))
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	decoded := key([]string{wsproto.ConfigFilename}, ProjectConfigScope{})
	if key([]string{wsproto.ConfigFilename}, ProjectConfigScope{Verbatim: true}) == decoded {
		t.Error("the verbatim scope keyed a canonical config like the decoded view")
	}
	if key([]string{"*.json"}, ProjectConfigScope{}) == decoded {
		t.Error("a glob pattern keyed a canonical config like the decoded view")
	}
}

// The pattern that selects a project config decides how it is read. A glob in
// the last segment selects files by type or by directory, so the task reads
// the config as text and every byte is in the digest. A pattern that names the
// file keeps the decoded view. A config both kinds select is read as text,
// whatever the pattern order.
func TestHashFiles_GlobPatternReadsProjectConfigBytes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "proj")
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifests := []string{filepath.Join(dir, wsproto.ConfigFilename), filepath.Join(dir, "nested", wsproto.ConfigFilename)}
	write := func(body string) {
		t.Helper()
		for _, path := range manifests {
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	digest := func(patterns ...string) string {
		t.Helper()
		got, err := hashFiles(dir, patterns, ProjectConfigScope{ExtensionLayers: []string{"@putnami/typescript"}})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	cases := []struct {
		name     string
		patterns []string
		raw      bool
	}{
		{"extension glob", []string{"**/*.json"}, true},
		{"single-level glob", []string{"*.json", "nested/*.json"}, true},
		{"directory sweep", []string{"**"}, true},
		{"named file", []string{wsproto.ConfigFilename, "nested/" + wsproto.ConfigFilename}, false},
		{"named file at any depth", []string{"**/" + wsproto.ConfigFilename}, false},
		{"named and glob", []string{"**/" + wsproto.ConfigFilename, "**/*.json"}, true},
		{"glob and named", []string{"**/*.json", "**/" + wsproto.ConfigFilename}, true},
	}

	base := "{\n  \"name\": \"proj\",\n  \"tags\": [\"ts\"]\n}\n"
	edits := []configCase{
		{"a layout-only edit", "{\n  \"name\": \"proj\",\n  \"tags\": [\n    \"ts\"\n  ]\n}\n"},
		{"an execution-only tasks block", "{\n  \"name\": \"proj\",\n  \"tags\": [\"ts\"],\n  \"tasks\": {\"lint\": {\"cpuWeight\": 4}}\n}\n"},
		{"another extension's layer", "{\n  \"name\": \"proj\",\n  \"tags\": [\"ts\"],\n  \"options\": {\"@putnami/cloud\": {\"region\": \"eu\"}}\n}\n"},
	}
	before := make([]string, len(cases))
	write(base)
	for i, tc := range cases {
		before[i] = digest(tc.patterns...)
	}
	for _, edit := range edits {
		write(edit.body)
		for i, tc := range cases {
			moved := digest(tc.patterns...) != before[i]
			if moved != tc.raw {
				t.Errorf("%s: %s moved the digest = %v, want %v", tc.name, edit.what, moved, tc.raw)
			}
		}
	}
}

// A config read across the project root, and one a workspace-relative key
// pattern selects, keep the verbatim reading. Otherwise a source rewriter
// keyed on such a config would key and detect it through the projection, and
// a layout-only rewrite of it would look clean.
func TestProjectConfigScope_VerbatimReachesConfigsOutsideTheProject(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("proj/index.ts", "const a = 1;\n")
	write("other/putnami.json", "{\"name\": \"other\", \"tags\": [\"go\"]}\n")

	across := func(scope ProjectConfigScope) string {
		t.Helper()
		got, err := hashFiles(filepath.Join(root, "proj"), []string{"index.ts", "../other/putnami.json"}, scope)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	key := func(scope ProjectConfigScope) string {
		t.Helper()
		k := BuildCacheKey("@test/ext", "1.0.0", "", "", "lint-format", "", "proj", "", "", nil, nil,
			filepath.Join(root, "proj"), root,
			CacheKeyPolicy{Files: []string{"index.ts"}, WorkspaceFiles: []string{"other/putnami.json"}, ConfigScope: scope},
			nil)
		got, err := k.ComputeHashUsing(NewCacheManager(nil))
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	verbatim := ProjectConfigScope{Verbatim: true}
	acrossVerbatim, acrossWhole := across(verbatim), across(ProjectConfigScope{})
	keyVerbatim, keyWhole := key(verbatim), key(ProjectConfigScope{})

	write("other/putnami.json", "{\n  \"name\": \"other\",\n  \"tags\": [\"go\"]\n}\n")
	if across(verbatim) == acrossVerbatim {
		t.Error("a layout-only edit of a config read across the project root left the verbatim digest unmoved")
	}
	if across(ProjectConfigScope{}) != acrossWhole {
		t.Error("a layout-only edit of a config read across the project root moved the projected digest")
	}
	if key(verbatim) == keyVerbatim {
		t.Error("a layout-only edit of a workspace-keyed config left a verbatim key unmoved")
	}
	if key(ProjectConfigScope{}) != keyWhole {
		t.Error("a layout-only edit of a workspace-keyed config moved a projected key")
	}
}
