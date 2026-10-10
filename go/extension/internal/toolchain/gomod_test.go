package toolchain

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// THE MIGRATED PARSER CORPUS.
//
// These cases moved here from tooling/cli/internal/workspace/derive_go_deps_test.go.
// They pin the go.mod grammar the workspace probe consolidates on, and each one
// is a shape that, mis-parsed, produces a MISSING dependency edge — which is to
// say a build cache key that stops observing a dependency it links.

// TestParseGoMod_RequireForms covers both the single-line require form and the
// parenthesised block form, direct and `// indirect` requires. Indirect
// requires MUST be captured: they still link code that has to key the build.
func TestParseGoMod_RequireForms(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{
			name:    "single-line direct require",
			content: "module m\n\ngo 1.21\n\nrequire foo v1.2.3\n",
			want:    []string{"foo"},
		},
		{
			name:    "single-line indirect require is kept",
			content: "module m\n\nrequire bar v0.1.0 // indirect\n",
			want:    []string{"bar"},
		},
		{
			name: "block form, direct and indirect",
			content: "module m\n\nrequire (\n" +
				"\tfoo v1.2.3\n" +
				"\tbar v0.1.0 // indirect\n" +
				"\tbaz v2.0.0\n" +
				")\n",
			want: []string{"foo", "bar", "baz"},
		},
		{
			name: "two require blocks (direct block + indirect block)",
			content: "module m\n\nrequire (\n\tfoo v1\n)\n\n" +
				"require (\n\tbar v2 // indirect\n)\n",
			want: []string{"foo", "bar"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mod, err := ParseGoMod(tt.content)
			if err != nil {
				t.Fatalf("ParseGoMod: %v", err)
			}
			if !reflect.DeepEqual(mod.Requires, tt.want) {
				t.Errorf("Requires = %v, want %v", mod.Requires, tt.want)
			}
		})
	}
}

// TestParseGoMod_ReplaceForms covers replace parsing for both module-target and
// local-path-target replaces, single-line and block form. Local targets (`./`,
// `../`) must be flagged so the probe resolves them back to a workspace
// directory rather than looking them up as module paths.
func TestParseGoMod_ReplaceForms(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []GoModReplace
	}{
		{
			name:    "single-line local replace",
			content: "module m\n\nreplace foo => ../foo\n",
			want:    []GoModReplace{{Old: "foo", NewPath: "../foo", NewLocal: true}},
		},
		{
			name:    "single-line module replace",
			content: "module m\n\nreplace foo => example.com/fork/foo v1.0.0\n",
			want:    []GoModReplace{{Old: "foo", NewPath: "example.com/fork/foo", NewLocal: false}},
		},
		{
			name:    "replace with version on the left",
			content: "module m\n\nreplace foo v1.2.3 => ../foo\n",
			want:    []GoModReplace{{Old: "foo", NewPath: "../foo", NewLocal: true}},
		},
		{
			name: "block form, mixed local and module targets",
			content: "module m\n\nreplace (\n" +
				"\tfoo => ../foo\n" +
				"\tbar => example.com/fork/bar v2.0.0\n" +
				"\tbaz v1 => ./vendor/baz\n" +
				")\n",
			want: []GoModReplace{
				{Old: "foo", NewPath: "../foo", NewLocal: true},
				{Old: "bar", NewPath: "example.com/fork/bar", NewLocal: false},
				{Old: "baz", NewPath: "./vendor/baz", NewLocal: true},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mod, err := ParseGoMod(tt.content)
			if err != nil {
				t.Fatalf("ParseGoMod: %v", err)
			}
			if !reflect.DeepEqual(mod.Replaces, tt.want) {
				t.Errorf("Replaces = %v, want %v", mod.Replaces, tt.want)
			}
		})
	}
}

// TestParseGoMod_ModuleAndMissing pins the module line extraction, the `go`
// directive, and the error contract for a go.mod with no module line — which
// the probe surfaces as a diagnostic rather than as a phantom project.
func TestParseGoMod_ModuleAndMissing(t *testing.T) {
	mod, err := ParseGoMod("module go.putnami.dev/x\n\ngo 1.21\n")
	if err != nil {
		t.Fatalf("ParseGoMod: %v", err)
	}
	if mod.Module != "go.putnami.dev/x" {
		t.Errorf("Module = %q, want %q", mod.Module, "go.putnami.dev/x")
	}
	if mod.GoVersion != "1.21" {
		t.Errorf("GoVersion = %q, want 1.21", mod.GoVersion)
	}

	if _, err := ParseGoMod("go 1.21\n"); err == nil {
		t.Error("expected error for go.mod without a module line")
	}
}

// A comment that would otherwise be read as a directive must not become one,
// and a commented-out require must not produce an edge that keys a build on a
// dependency the module does not have.
func TestParseGoMod_CommentsAreStripped(t *testing.T) {
	mod, err := ParseGoMod("module m\n\n// require commented v1\nrequire real v1 // indirect\n")
	if err != nil {
		t.Fatalf("ParseGoMod: %v", err)
	}
	if !reflect.DeepEqual(mod.Requires, []string{"real"}) {
		t.Errorf("Requires = %v, want [real]", mod.Requires)
	}
}

// A BLOCK THAT NEVER CLOSES MUST NOT EAT THE REST OF THE FILE.
//
// This parser is tolerant of a half-written go.mod by design, and the
// two shapes below are the ones that produce a require block with no `)`: the
// paren consumed on the opener line, and a block still being typed. Before the
// recovery, everything after such a block was read as a block entry — so the
// `replace` directives were invisible, ReplacedModules() came back empty, and
// the workspace-sync codemod re-appended the same directive on every run
// instead of converging. Recognizing a top-level directive as the end of the
// phantom block is what keeps that task idempotent.
func TestParseGoMod_UnterminatedBlockDoesNotSwallowLaterDirectives(t *testing.T) {
	tests := []struct {
		name         string
		content      string
		wantRequires []string
		wantReplaces []GoModReplace
	}{
		{
			name:         "require block whose paren closed on the opener line",
			content:      "module m\n\nrequire ( foo v1 )\n\nreplace foo => ../foo\n",
			wantRequires: nil,
			wantReplaces: []GoModReplace{{Old: "foo", NewPath: "../foo", NewLocal: true}},
		},
		{
			name:         "require block with no closing paren at all",
			content:      "module m\n\nrequire (\n\tfoo v1\n\nreplace foo => ../foo\n",
			wantRequires: []string{"foo"},
			wantReplaces: []GoModReplace{{Old: "foo", NewPath: "../foo", NewLocal: true}},
		},
		{
			name: "an unterminated block still yields the module line and later requires",
			content: "module m\n\nreplace (\n\tfoo => ../foo\n\n" +
				"require bar v2\n\nreplace baz => ../baz\n",
			wantRequires: []string{"bar"},
			wantReplaces: []GoModReplace{
				{Old: "foo", NewPath: "../foo", NewLocal: true},
				{Old: "baz", NewPath: "../baz", NewLocal: true},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mod, err := ParseGoMod(tt.content)
			if err != nil {
				t.Fatalf("ParseGoMod: %v", err)
			}
			if mod.Module != "m" {
				t.Errorf("Module = %q, want m", mod.Module)
			}
			if !reflect.DeepEqual(mod.Requires, tt.wantRequires) {
				t.Errorf("Requires = %v, want %v", mod.Requires, tt.wantRequires)
			}
			if !reflect.DeepEqual(mod.Replaces, tt.wantReplaces) {
				t.Errorf("Replaces = %v, want %v", mod.Replaces, tt.wantReplaces)
			}
		})
	}
}

// TestParseGoMod_IgnoreForms covers the `ignore` directive (Go 1.25) in its
// single-line, block and quoted forms, and a require block it ends.
func TestParseGoMod_IgnoreForms(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{
			name:    "single-line ignore",
			content: "module m\n\nignore ./tools\n",
			want:    []string{"./tools"},
		},
		{
			name:    "block form with a comment",
			content: "module m\n\nignore (\n\tstatic // assets\n\tcontent/html\n\t./third_party/js\n)\n",
			want:    []string{"static", "content/html", "./third_party/js"},
		},
		{
			name:    "a double-quoted path is unquoted",
			content: "module m\n\nignore \"./with space\"\n",
			want:    []string{"./with space"},
		},
		{
			name:    "an entry the go command rejects is dropped",
			content: "module m\n\nignore a b\nignore `raw`\nignore it's\nignore c\n",
			want:    []string{"c"},
		},
		{
			name:    "an empty block opens nothing",
			content: "module m\n\nignore ()\n\ntool (\n\texample.com/cmd/x\n)\n\nignore c\n",
			want:    []string{"c"},
		},
		{
			name:    "a tab or a paren follows the verb",
			content: "module m\n\nignore\t./tools\nignore(\n\tstatic\n)\n",
			want:    []string{"./tools", "static"},
		},
		{
			name:    "an ignore ends a require block that never closed",
			content: "module m\n\nrequire (\n\tfoo v1\n\nignore ./tools\n",
			want:    []string{"./tools"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mod, err := ParseGoMod(tt.content)
			if err != nil {
				t.Fatalf("ParseGoMod: %v", err)
			}
			if !reflect.DeepEqual(mod.Ignores, tt.want) {
				t.Errorf("Ignores = %q, want %q", mod.Ignores, tt.want)
			}
		})
	}
}

// ReplacedModules is the closure computation's oracle for "already satisfied";
// both directive forms must feed it.
func TestGoModFile_ReplacedModules(t *testing.T) {
	mod, err := ParseGoMod("module m\n\nreplace a => ../a\n\nreplace (\n\tb => ../b\n)\n")
	if err != nil {
		t.Fatalf("ParseGoMod: %v", err)
	}
	replaced := mod.ReplacedModules()
	if !replaced["a"] || !replaced["b"] || len(replaced) != 2 {
		t.Errorf("ReplacedModules = %v, want {a,b}", replaced)
	}
}

// ReadGoMod returns (nil, nil) for an absent file: "this directory is not a Go
// module" is an ordinary answer, not a failure.
func TestReadGoMod_AbsentFileIsNotAnError(t *testing.T) {
	mod, err := ReadGoMod(filepath.Join(t.TempDir(), "go.mod"))
	if mod != nil || err != nil {
		t.Fatalf("ReadGoMod(absent) = (%v, %v), want (nil, nil)", mod, err)
	}
}

func TestParseGoWorkUses_BothFormsAndComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.work")
	content := "go 1.26\n\nuse (\n\t./a\n\t./nested/b // trailing comment\n)\n\nuse ./c\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	rels, err := ParseGoWorkUses(path)
	if err != nil {
		t.Fatalf("ParseGoWorkUses: %v", err)
	}
	if !reflect.DeepEqual(rels, []string{"a", "nested/b", "c"}) {
		t.Errorf("uses = %v, want [a nested/b c] (cleaned, comments stripped)", rels)
	}

	missing, err := ParseGoWorkUses(filepath.Join(dir, "absent", "go.work"))
	if err != nil || len(missing) != 0 {
		t.Fatalf("ParseGoWorkUses(absent) = (%v, %v), want (nil, nil)", missing, err)
	}
}

func TestIsLocalReplaceTarget(t *testing.T) {
	for _, target := range []string{"./x", "../x", ".", ".."} {
		if !isLocalReplaceTarget(target) {
			t.Errorf("%q should be a local target", target)
		}
	}
	for _, target := range []string{"example.com/x", "go.putnami.dev/protocol/config"} {
		if isLocalReplaceTarget(target) {
			t.Errorf("%q should be a module target", target)
		}
	}
}
