package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	proto "go.putnami.dev/protocol/extension"
)

// The no-dead-protocol invariant, made checkable for the one
// protocol struct whose fields are pure classification: CommandTraits.
//
// Every field on it is documented, serialized, and published in the extension
// JSON schema, which invites an extension author to declare it. A field nothing
// reads therefore promises behavior the framework does not deliver — silently,
// because declaring it is not an error. SideEffects sat in exactly that state
// through an entire epic and was found only when a later change needed the
// classification it had been typing all along.
//
// This guard is deliberately source-level rather than a compile-time trick: a
// trait is honored by a consumer READING it, and no Go type can express that.
// Parsing Go syntax keeps comments and string literals from masquerading as
// consumers while avoiding package loading across the workspace's modules.
const traitsDeclaringDir = "protocols/extension"

// TestCommandTraits_EveryFieldHasANonTestReadSite fails when a CommandTraits
// field is declared, serialized, and schema-advertised but read by nothing.
//
// Scope is the whole workspace, not this module: a trait honored by the Go
// extension rather than the CLI is still honored, and a guard that could only
// see one module would fail for a legitimate consumer living elsewhere.
//
// The declaring package is excluded. Traits are assigned there with composite
// literal keys (`SideEffects: ...`), which this does not count anyway, but a
// field the protocol only reads back to itself is still not a field any
// consumer honors.
func TestCommandTraits_EveryFieldHasANonTestReadSite(t *testing.T) {
	t.Parallel()
	root := workspaceRoot(t)
	production, _ := goSources(t, root)

	consumers := make([]string, 0, len(production))
	for _, rel := range production {
		if !strings.HasPrefix(rel, traitsDeclaringDir+"/") {
			consumers = append(consumers, rel)
		}
	}
	if len(consumers) == 0 {
		t.Fatal("excluded every source file — the walk root or the declaring dir is wrong")
	}

	traits := reflect.TypeFor[proto.CommandTraits]()
	if traits.NumField() == 0 {
		t.Fatal("CommandTraits has no fields — the protocol guard would be vacuous")
	}
	fields := make([]string, 0, traits.NumField())
	for i := range traits.NumField() {
		fields = append(fields, traits.Field(i).Name)
	}
	reads := countFieldReads(t, root, consumers, fields)
	for i := range traits.NumField() {
		field := traits.Field(i)
		read := reads[field.Name]
		if read.count > 0 {
			continue
		}
		t.Errorf("CommandTraits.%s (json:%q) has no non-test read site in the workspace.\n"+
			"    It is documented, serialized, and published in the extension JSON schema, so an\n"+
			"    extension author who declares it gets silent no-op behavior. Either honor it at\n"+
			"    the seam where its question is the real one, or retire it from the struct, the\n"+
			"    schema, and any CLI re-export.\n"+
			"    searched %d production files outside %s/\n%s",
			field.Name, field.Tag.Get("json"), len(consumers), traitsDeclaringDir, indentSites(read.sites))
	}
}

type fieldReads struct {
	count int
	sites []string
}

// countFieldReads counts selector reads of fields in parsed Go syntax. Every
// source file is parsed once regardless of how many protocol fields exist.
//
// Syntax is the whole point and not a detail: a raw source search also matches
// `proto.SideEffectsRegistry`, comments, and string literals. Any of those
// would report a dead field as live. A simple assignment is excluded too:
// populating a trait is not a consumer honoring it. Compound assignments still
// count because they read the previous value.
func countFieldReads(t *testing.T, root string, files, fields []string) map[string]fieldReads {
	t.Helper()
	const maxReportedSites = 12

	reads := make(map[string]fieldReads, len(fields))
	for _, field := range fields {
		if _, exists := reads[field]; exists {
			t.Fatalf("duplicate field %q in selector-read query", field)
		}
		reads[field] = fieldReads{}
	}
	for _, rel := range files {
		filename := filepath.Join(root, filepath.FromSlash(rel))
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filename, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}

		writeOnly := make(map[token.Pos]bool)
		ast.Inspect(file, func(node ast.Node) bool {
			assignment, ok := node.(*ast.AssignStmt)
			if !ok || assignment.Tok != token.ASSIGN {
				return true
			}
			for _, lhs := range assignment.Lhs {
				ast.Inspect(lhs, func(node ast.Node) bool {
					if selector, ok := node.(*ast.SelectorExpr); ok {
						writeOnly[selector.Sel.Pos()] = true
					}
					return true
				})
			}
			return true
		})

		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok || writeOnly[selector.Sel.Pos()] {
				return true
			}
			read, tracked := reads[selector.Sel.Name]
			if !tracked {
				return true
			}
			read.count++
			if len(read.sites) < maxReportedSites {
				read.sites = append(read.sites, rel+":"+strconv.Itoa(fset.Position(selector.Sel.Pos()).Line))
			}
			reads[selector.Sel.Name] = read
			return true
		})
	}
	return reads
}

// workspaceRoot walks up to the directory holding putnami.workspace.json.
// moduleRoot stops at this module's go.mod, which would scope the guard to the
// CLI and miss a consumer in another module.
func workspaceRoot(t *testing.T) string {
	t.Helper()
	dir := moduleRoot(t)
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no putnami.workspace.json above %s", moduleRoot(t))
		}
		dir = parent
	}
}

// TestCountFieldReads_RejectsAPrefixMatch pins the boundary rule above, so a
// later simplification to strings.Contains fails here instead of silently
// turning the guard into a rubber stamp.
func TestCountFieldReads_RejectsAPrefixMatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const src = "package p\n" +
		"var _ = proto.SideEffectsRegistry\n" +
		"var _ = other.SideEffectsCloud\n" +
		"var _ = \"object.SideEffects\"\n" +
		"// object.SideEffects is prose, not a read\n" +
		"func set(traits proto.CommandTraits) { traits.SideEffects = \"cloud\" }\n"
	if err := os.WriteFile(filepath.Join(dir, "prefix.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	reads := countFieldReads(t, dir, []string{"prefix.go"}, []string{"SideEffects", "Enabled"})
	if len(reads) != 2 {
		t.Fatalf("field result count = %d, want one non-vacuous result per requested field", len(reads))
	}
	if read := reads["SideEffects"]; read.count != 0 {
		t.Errorf("constant re-exports counted as reads of the field: %d\n%s", read.count, indentSites(read.sites))
	}

	const real = "package p\n" +
		"func f(tr proto.CommandTraits) bool { return tr.SideEffects != \"\" || tr.Enabled }\n"
	if err := os.WriteFile(filepath.Join(dir, "real.go"), []byte(real), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	reads = countFieldReads(t, dir, []string{"real.go"}, []string{"SideEffects", "Enabled"})
	for _, field := range []string{"SideEffects", "Enabled"} {
		read := reads[field]
		if read.count != 1 {
			t.Errorf("genuine %s selector read count = %d, want 1", field, read.count)
		}
		if want := "real.go:2"; len(read.sites) != 1 || read.sites[0] != want {
			t.Errorf("%s sites = %v, want [%s]", field, read.sites, want)
		}
	}
}
