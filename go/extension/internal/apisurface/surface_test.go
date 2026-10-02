package apisurface

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const (
	testModule = "go.example.com/lib"

	feature     = "go/go-project-toolchain"
	requirement = "api-compatibility-marker"
)

func files(sources map[string]string) []File {
	out := make([]File, 0, len(sources))
	for path, data := range sources {
		out = append(out, File{Path: path, Data: []byte(data)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func read(t *testing.T, sources map[string]string) *Surface {
	t.Helper()
	surface, err := Read(files(sources), "fallback.example/project")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return surface
}

// lib is a one-package module whose only file is source.
func lib(source string) map[string]string {
	return map[string]string{
		"go.mod":  "module " + testModule + "\n\ngo 1.25\n",
		"lib.go":  "package lib\n\n" + source,
		"doc.txt": "not Go",
	}
}

func messages(changes []Change) []string {
	out := make([]string, 0, len(changes))
	for _, change := range changes {
		out = append(out, change.Message)
	}
	return out
}

func incompatible(t *testing.T, old, current string) []string {
	t.Helper()
	return messages(Incompatible(read(t, lib(old)), read(t, lib(current))))
}

func assertMessages(t *testing.T, got []string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("changes = %q\nwant      %q", got, want)
	}
}

func TestARemovedFunctionIsIncompatibleAndNamesPackageAndSymbol(t *testing.T) {
	t.Parallel()
	changes := Incompatible(
		read(t, lib("func Greet(name string) string { return name }\nfunc Keep() {}\n")),
		read(t, lib("func Keep() {}\n")),
	)
	if len(changes) != 1 {
		t.Fatalf("changes = %+v, want one", changes)
	}
	change := changes[0]
	if change.Package != testModule || change.Symbol != "func Greet" || change.Message != "func Greet removed" {
		t.Fatalf("change = %+v", change)
	}
	if change.Pos != (Position{}) {
		t.Fatalf("a removed symbol has no position, got %+v", change.Pos)
	}
}

func TestAdditionsAreCompatible(t *testing.T) {
	t.Parallel()
	assertMessages(t, incompatible(t,
		"type Client struct{ URL string }\nfunc (c Client) Get() error { return nil }\n",
		"type Client struct{ URL string; Timeout int }\nfunc (c Client) Get() error { return nil }\n"+
			"func (c Client) Put() error { return nil }\nfunc New() Client { return Client{} }\n"+
			"var Default = New()\nconst Version = \"1\"\ntype Option func(*Client)\n",
	))
}

func TestRenamedParametersAreCompatible(t *testing.T) {
	t.Parallel()
	assertMessages(t, incompatible(t,
		"func Greet(name string, times int) (out string, err error) { return }\n"+
			"func Each(fn func(index int, value string) bool) {}\n",
		"func Greet(who string, n int) (string, error) { return \"\", nil }\n"+
			"func Each(visit func(i int, v string) bool) {}\n",
	))
}

func TestGroupedParametersReadAsTheirTypes(t *testing.T) {
	t.Parallel()
	assertMessages(t, incompatible(t,
		"func Add(a, b int) int { return a + b }\n",
		"func Add(a int, b int) int { return a + b }\n",
	))
	assertMessages(t, incompatible(t,
		"func Add(a, b int) int { return a + b }\n",
		"func Add(a int) int { return a }\n",
	), "func Add changed from func(int, int) int to func(int) int")
}

func TestASignatureChangeIsIncompatible(t *testing.T) {
	t.Parallel()
	assertMessages(t, incompatible(t,
		"func Greet(name string) string { return name }\nfunc Many(values ...string) {}\n",
		"func Greet(name string, loud bool) string { return name }\nfunc Many(values []string) {}\n",
	),
		"func Greet changed from func(string) string to func(string, bool) string",
		"func Many changed from func(...string) to func([]string)",
	)
}

func TestMethodChanges(t *testing.T) {
	t.Parallel()
	old := "type Client struct{}\n" +
		"func (c Client) Get() error { return nil }\n" +
		"func (c Client) Close() {}\n" +
		"func (c *Client) Do(n int) {}\n" +
		"func (c Client) Name() string { return \"\" }\n" +
		"func (c Client) internal() {}\n" +
		"type hidden struct{}\n" +
		"func (h hidden) Gone() {}\n"
	current := "type Client struct{}\n" +
		"func (c *Client) Get() error { return nil }\n" +
		"func (c *Client) Do(n int64) {}\n" +
		"func (client Client) Name() string { return \"\" }\n"
	assertMessages(t, incompatible(t, old, current),
		"method Client.Close removed",
		"method Client.Do changed from func(int) to func(int64)",
		"method Client.Get moved from receiver Client to *Client",
	)
}

func TestAPointerMethodMovedToAValueReceiverIsCompatible(t *testing.T) {
	t.Parallel()
	assertMessages(t, incompatible(t,
		"type T struct{}\nfunc (t *T) M() {}\n",
		"type T struct{}\nfunc (t T) M() {}\n",
	))
}

func TestStructFieldChanges(t *testing.T) {
	t.Parallel()
	old := "type Base struct{}\ntype inner struct{}\n" +
		"type Config struct {\n\tBase\n\tinner\n\tHost, Port string\n\tTags []string `json:\"tags\"`\n\tsecret string\n}\n"
	current := "type Base struct{}\ntype inner struct{}\n" +
		"type Config struct {\n\t*Base\n\tHost string\n\tPort int\n\tTags []string `json:\"labels\"`\n}\n"
	assertMessages(t, incompatible(t, old, current),
		"field Config.Base changed type from Base to *Base",
		"field Config.Port changed type from string to int",
	)
	assertMessages(t, incompatible(t, current, "type Base struct{}\ntype Config struct{ Host string }\n"),
		"field Config.Base removed",
		"field Config.Port removed",
		"field Config.Tags removed",
	)
}

func TestInterfaceChanges(t *testing.T) {
	t.Parallel()
	assertMessages(t, incompatible(t,
		"type Store interface {\n\tGet(key string) ([]byte, error)\n\tDelete(key string) error\n\tio.Closer\n}\n",
		"type Store interface {\n\tGet(id string) ([]byte, bool)\n\tPut(key string, value []byte) error\n\tio.Reader\n}\n",
	),
		"interface Store embedded io.Closer removed",
		"interface Store embedded io.Reader added: every implementation outside the package must add it",
		"interface method Store.Delete removed",
		"interface method Store.Get changed from func(string) ([]byte, error) to func(string) ([]byte, bool)",
		"interface method Store.Put added: every implementation outside the package must add it",
	)
}

func TestAMethodAddedToASealedInterfaceIsCompatible(t *testing.T) {
	t.Parallel()
	assertMessages(t, incompatible(t,
		"type Event interface {\n\tName() string\n\tsealed()\n}\n",
		"type Event interface {\n\tName() string\n\tAt() int64\n\tsealed()\n}\n",
	))
}

func TestAnInterfaceThatEmbedsASealedInterfaceIsSealed(t *testing.T) {
	t.Parallel()
	const sealed = "type node interface{ node() }\n" +
		"type Node interface{ isNode() }\n" +
		"type base[T any] interface{ get() T }\n"
	assertMessages(t, incompatible(t,
		sealed+"type Expr interface {\n\tnode\n\tPos() int\n}\n"+
			"type Stmt interface{ Expr }\n"+
			"type Decl interface {\n\tNode\n\tName() string\n}\n"+
			"type Getter interface{ base[int] }\n",
		sealed+"type Expr interface {\n\tnode\n\tPos() int\n\tEnd() int\n}\n"+
			"type Stmt interface {\n\tExpr\n\tBody() int\n}\n"+
			"type Decl interface {\n\tNode\n\tName() string\n\tDoc() string\n}\n"+
			"type Getter interface {\n\tbase[int]\n\tSet(int)\n}\n",
	))

	// An interface that embeds one any package can implement stays open.
	assertMessages(t, incompatible(t,
		"type Reader interface{ Read() }\ntype ReadCloser interface {\n\tReader\n\tClose()\n}\n",
		"type Reader interface{ Read() }\ntype ReadCloser interface {\n\tReader\n\tClose()\n\tFlush()\n}\n",
	), "interface method ReadCloser.Flush added: every implementation outside the package must add it")
}

func TestARenamedImportIsCompatible(t *testing.T) {
	spectest.Proves(t, feature, requirement, "a-renamed-import-or-predeclared-alias-is-compatible")
	t.Parallel()
	assertMessages(t, incompatible(t,
		"import foo \"go.example.com/model\"\n\nfunc F(x foo.T) foo.U { return nil }\n",
		"import bar \"go.example.com/model\"\n\nfunc F(x bar.T) bar.U { return nil }\n",
	))
	// An import without a name is known by the name its path implies.
	assertMessages(t, incompatible(t,
		"import (\n\t\"go.example.com/model\"\n\t\"gopkg.in/yaml.v3\"\n\t\"go.example.com/mod/v2\"\n\t\"go.example.com/go-kit\"\n)\n\n"+
			"func F(model.T, yaml.Node, mod.V, kit.K) {}\n",
		"import (\n\tm \"go.example.com/model\"\n\ty \"gopkg.in/yaml.v3\"\n\tv2 \"go.example.com/mod/v2\"\n\tk \"go.example.com/go-kit\"\n)\n\n"+
			"func F(m.T, y.Node, v2.V, k.K) {}\n",
	))
	// The same name for another import path is a change, shown by path.
	assertMessages(t, incompatible(t,
		"import m \"go.example.com/model\"\n\nfunc F(m.T) {}\n",
		"import m \"go.example.com/other\"\n\nfunc F(m.T) {}\n",
	), "func F changed from func(go.example.com/model.T) to func(go.example.com/other.T)")
}

// A declaration can be rendered twice: a grouped parameter shares its type,
// and a const without a type repeats the previous spec's. Each rendering
// resolves a qualifier once, even when its import path is another import's
// name.
func TestRenderingAnExpressionTwiceGivesTheSameText(t *testing.T) {
	t.Parallel()
	surface := read(t, lib("import (\n\tfoo \"bar\"\n\tbar \"baz\"\n)\n\n"+
		"func F(a, b foo.T) {}\n\nconst (\n\tA foo.K = iota\n\tB\n)\n"))
	objects := surface.Packages[testModule].Objects
	if got := objects["F"].Type; got.Display != "func(bar.T, bar.T)" || got.Key != "func(bar.T, bar.T)" {
		t.Fatalf("F = %+v", got)
	}
	if objects["A"].Type != objects["B"].Type || objects["B"].Type.Display != "bar.K" {
		t.Fatalf("A = %+v, B = %+v", objects["A"].Type, objects["B"].Type)
	}
}

func TestPredeclaredAliasesAreCompatible(t *testing.T) {
	t.Parallel()
	assertMessages(t, incompatible(t,
		"func F(x interface{}) []byte { return nil }\nvar R rune\n"+
			"type Buffer struct{ Data []byte }\nfunc Map[T interface{}](T) {}\n",
		"func F(x any) []uint8 { return nil }\nvar R int32\n"+
			"type Buffer struct{ Data []uint8 }\nfunc Map[T any](T) {}\n",
	))
	assertMessages(t, incompatible(t,
		"func G(x any) {}\n",
		"func G(x string) {}\n",
	), "func G changed from func(any) to func(string)")
}

func TestReadsSelectsTheFilesReadReads(t *testing.T) {
	t.Parallel()
	got := Reads([]string{
		"go.mod", "lib.go", "lib_test.go", "README.md", "client/client.go",
		"internal/x/x.go", "testdata/x.go", "vendor/a/a.go", "_old/x.go", ".gen/x.go", "client/_x.go",
		"tools/go.mod", "tools/tools.go",
	})
	want := []string{"go.mod", "lib.go", "client/client.go", "tools/go.mod"}
	if !slices.Equal(got, want) {
		t.Fatalf("Reads = %q, want %q", got, want)
	}
}

func TestValueTypeChanges(t *testing.T) {
	t.Parallel()
	old := "var Default Client\nvar Inferred = 1\nconst Limit int = 3\n" +
		"type Level int\nconst (\n\tDebug Level = iota\n\tInfo\n)\ntype Client struct{}\n"
	current := "var Default *Client\nvar Inferred int64 = 1\nconst Limit int64 = 3\n" +
		"type Level int\ntype Mode int\nconst (\n\tDebug Mode = iota\n\tInfo\n)\ntype Client struct{}\n"
	assertMessages(t, incompatible(t, old, current),
		"const Debug changed type from Level to Mode",
		"const Info changed type from Level to Mode",
		"const Limit changed type from int to int64",
		"var Default changed type from Client to *Client",
	)
}

func TestKindAndFormChanges(t *testing.T) {
	t.Parallel()
	assertMessages(t, incompatible(t,
		"func Handler() {}\ntype ID string\ntype Opts struct{}\ntype Name = string\ntype Fn func(int)\n",
		"var Handler = func() {}\ntype ID int\ntype Opts interface{}\ntype Name string\ntype Fn func(string)\n",
	),
		"func Handler is now a var",
		"type Fn changed from func(int) to func(string)",
		"type ID changed from string to int",
		"type Name changed from an alias of string to string",
		"type Opts changed from a struct to an interface",
	)
}

func TestGenericsCompareTypeParametersByPosition(t *testing.T) {
	t.Parallel()
	assertMessages(t, incompatible(t,
		"type List[T any] struct{ Items []T }\n"+
			"func (l *List[T]) Push(v T) {}\n"+
			"func Map[T, U any](in []T, fn func(T) U) []U { return nil }\n",
		"type List[E any] struct{ Items []E }\n"+
			"func (list *List[X]) Push(value X) {}\n"+
			"func Map[In, Out any](in []In, fn func(In) Out) []Out { return nil }\n",
	))
	assertMessages(t, incompatible(t,
		"func Max[T int | float64](a, b T) T { return a }\ntype Set[K comparable] map[K]bool\n",
		"func Max[T ~int](a, b T) T { return a }\ntype Set[K any] map[K]bool\n",
	),
		"func Max changed its type parameters from [T int | float64] to [T ~int]",
		"type Set changed its type parameters from [K comparable] to [K any]",
	)
}

func TestAChangedPositionNamesTheCurrentDeclaration(t *testing.T) {
	t.Parallel()
	changes := Incompatible(read(t, lib("func Greet(name string) {}\n")), read(t, lib("\n\nfunc Greet(n int) {}\n")))
	if len(changes) != 1 || changes[0].Pos != (Position{File: "lib.go", Line: 5}) {
		t.Fatalf("changes = %+v, want lib.go:5", changes)
	}
}

func TestOnlyImportablePackagesFormTheSurface(t *testing.T) {
	t.Parallel()
	surface := read(t, map[string]string{
		"go.mod":                    "module " + testModule + "\n",
		"lib.go":                    "package lib\nfunc Root() {}\n",
		"lib_test.go":               "package lib\nfunc TestOnly() {}\n",
		"client/client.go":          "package client\nfunc New() {}\n",
		"client/internal/x/x.go":    "package x\nfunc Hidden() {}\n",
		"internal/y/y.go":           "package y\nfunc Hidden() {}\n",
		"testdata/z.go":             "package z\nfunc Fixture() {}\n",
		"vendor/v/v.go":             "package v\nfunc Vendored() {}\n",
		".gen/g.go":                 "package g\nfunc Generated() {}\n",
		"_old/o.go":                 "package o\nfunc Old() {}\n",
		"cmd/tool/main.go":          "package main\nfunc Exported() {}\n",
		"nested/go.mod":             "module other.example/nested\n",
		"nested/n.go":               "package nested\nfunc Other() {}\n",
		"nested/deeper/d.go":        "package deeper\nfunc Other() {}\n",
		"tools/tools.go":            "//go:build ignore\n\npackage tools\nfunc Ignored() {}\n",
		"tools/broken.go":           "//go:build ignore && linux\n\npackage tools\nthis does not parse\n",
		"platform/p_linux.go":       "//go:build linux\n\npackage platform\nfunc OnLinux() {}\n",
		"platform/p_other.go":       "//go:build !linux\n\npackage platform\nfunc Elsewhere() {}\n",
		"platform/.hidden.go":       "package platform\nfunc Hidden() {}\n",
		"platform/_draft.go":        "package platform\nfunc Draft() {}\n",
		"platform/README.md":        "# not Go\n",
		"empty/only_test.go":        "package empty\nfunc OnlyTest() {}\n",
		"client/v2/client_extra.go": "package client\nfunc V2() {}\n",
	})
	if surface.Module != testModule {
		t.Fatalf("module = %q, want %q", surface.Module, testModule)
	}
	got := map[string][]string{}
	for path, pkg := range surface.Packages {
		got[path] = sortedKeys(pkg.Objects)
	}
	want := map[string][]string{
		testModule:                {"Root"},
		testModule + "/client":    {"New"},
		testModule + "/client/v2": {"V2"},
		testModule + "/platform":  {"Elsewhere", "OnLinux"},
	}
	if len(got) != len(want) {
		t.Fatalf("packages = %v, want %v", got, want)
	}
	for path, names := range want {
		if !slices.Equal(got[path], names) {
			t.Fatalf("package %s = %v, want %v (all: %v)", path, got[path], names, got)
		}
	}
}

func TestARemovedPackageIsIncompatible(t *testing.T) {
	t.Parallel()
	old := read(t, map[string]string{
		"go.mod":          "module " + testModule + "\n",
		"lib.go":          "package lib\n",
		"client/c.go":     "package client\nfunc New() {}\n",
		"client/sub/s.go": "package sub\n",
	})
	current := read(t, map[string]string{"go.mod": "module " + testModule + "\n", "lib.go": "package lib\n"})
	changes := Incompatible(old, current)
	assertMessages(t, messages(changes), "package removed", "package removed")
	if changes[0].Package != testModule+"/client" || changes[1].Package != testModule+"/client/sub" || changes[0].Symbol != "" {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestAProjectWithoutGoModUsesTheGivenModulePath(t *testing.T) {
	t.Parallel()
	surface := read(t, map[string]string{"lib.go": "package lib\nfunc A() {}\n", "sub/s.go": "package sub\n"})
	if surface.Module != "fallback.example/project" {
		t.Fatalf("module = %q", surface.Module)
	}
	if _, ok := surface.Packages["fallback.example/project/sub"]; !ok {
		t.Fatalf("packages = %v", sortedKeys(surface.Packages))
	}
}

func TestTheFirstDeclarationOfANameWins(t *testing.T) {
	t.Parallel()
	surface := read(t, map[string]string{
		"a_linux.go":   "package lib\nfunc Open(path string) error { return nil }\n",
		"b_windows.go": "package lib\nfunc Open(path string, mode int) error { return nil }\n",
	})
	open := surface.Packages["fallback.example/project"].Objects["Open"]
	if open == nil || open.Type.Display != "func(string) error" || open.Pos.File != "a_linux.go" {
		t.Fatalf("Open = %+v", open)
	}
}

func TestASourceThatDoesNotParseIsAnError(t *testing.T) {
	t.Parallel()
	for name, source := range map[string]string{
		"body":   "package lib\nfunc (\n",
		"header": "not a package clause\n",
	} {
		_, err := Read(files(map[string]string{"lib.go": source}), testModule)
		if err == nil || !strings.Contains(err.Error(), "parse lib.go") {
			t.Fatalf("%s: err = %v, want a parse error naming the file", name, err)
		}
	}
}

func TestSatisfiableNeverSetsIgnore(t *testing.T) {
	t.Parallel()
	for source, want := range map[string]bool{
		"package p\n":                                        true,
		"//go:build ignore\n\npackage p\n":                   false,
		"//go:build !ignore\n\npackage p\n":                  true,
		"//go:build linux && !linux\n\npackage p\n":          false,
		"//go:build (linux || darwin) && cgo\n\npackage p\n": true,
	} {
		surface, err := Read(files(map[string]string{"p.go": source}), testModule)
		if err != nil {
			t.Fatal(err)
		}
		if _, got := surface.Packages[testModule]; got != want {
			t.Errorf("%q builds somewhere = %v, want %v", source, got, want)
		}
	}
}
