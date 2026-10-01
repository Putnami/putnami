package configextract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocfg "go.putnami.dev/protocol/config"
	pctx "go.putnami.dev/sdk/extension/context"
)

func writeProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func findField(fields []protocfg.FieldSchema, name string) (protocfg.FieldSchema, bool) {
	for _, f := range fields {
		if f.Name == name {
			return f, true
		}
	}
	return protocfg.FieldSchema{}, false
}

func TestResolveOutputPath_ProjectOptionsSchemaFalse(t *testing.T) {
	ctx := &pctx.Context{
		Project: pctx.Project{
			Options: map[string]json.RawMessage{
				"generate": json.RawMessage(`{"schema":false}`),
			},
		},
	}

	if got := resolveOutputPath(ctx); got != FallbackOutputPath {
		t.Fatalf("resolveOutputPath = %q, want %q", got, FallbackOutputPath)
	}
	if ProjectWantsCommittedSchemas(ctx) {
		t.Fatal("ProjectWantsCommittedSchemas = true, want false")
	}
}

func TestResolveOutputPath_DefaultWhenProjectOptionUnset(t *testing.T) {
	if got := resolveOutputPath(&pctx.Context{}); got != DefaultOutputPath {
		t.Fatalf("resolveOutputPath = %q, want %q", got, DefaultOutputPath)
	}
}

func TestExtractFromProject_PicksUpSensitiveTag(t *testing.T) {
	dir := t.TempDir()
	src := `package myapp

import "go.putnami.dev/config"

type DatabaseOptions struct {
	Host     string ` + "`json:\"host\" default:\"localhost\"`" + `
	Password string ` + "`json:\"password\" env:\"DB_PASSWORD\" sensitive:\"true\"`" + `
	APIKey   string ` + "`sensitive:\"true\"`" + `
}

var DatabaseConfig = config.Config[DatabaseOptions]("database")
`
	file := filepath.Join(dir, "config.go")
	if err := os.WriteFile(file, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	blocks, err := extractFromProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Path != "database" {
		t.Fatalf("expected path=database, got %q", blocks[0].Path)
	}

	byName := map[string]bool{}
	sensByName := map[string]bool{}
	for _, f := range blocks[0].Fields {
		byName[f.Name] = true
		sensByName[f.Name] = f.Sensitive
	}

	if !byName["host"] || !byName["password"] || !byName["APIKey"] {
		t.Errorf("missing fields; got %v", byName)
	}
	if sensByName["host"] {
		t.Errorf("host should not be sensitive")
	}
	if !sensByName["password"] {
		t.Errorf("password should be sensitive")
	}
	if !sensByName["APIKey"] {
		t.Errorf("APIKey should be sensitive (no json tag, falls back to Go name)")
	}
}

func TestExtractFromProject_PicksUpProductionUnsafeDefaultTag(t *testing.T) {
	dir := t.TempDir()
	src := `package myapp

import "go.putnami.dev/config"

type DatabaseOptions struct {
	Host   string ` + "`json:\"host\" default:\"localhost\"`" + `
	Driver string ` + "`json:\"driver\" default:\"memory\" productionUnsafeDefault:\"true\"`" + `
}

var DatabaseConfig = config.Config[DatabaseOptions]("database")
`
	file := filepath.Join(dir, "config.go")
	if err := os.WriteFile(file, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	blocks, err := extractFromProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}

	unsafeByName := map[string]bool{}
	for _, f := range blocks[0].Fields {
		unsafeByName[f.Name] = f.ProductionUnsafeDefault
	}
	if unsafeByName["host"] {
		t.Errorf("host should not be marked production-unsafe")
	}
	if !unsafeByName["driver"] {
		t.Errorf("driver should be marked production-unsafe from productionUnsafeDefault:\"true\" tag")
	}
}

func TestExtractFromProject_ExpandsNestedStruct(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.25\n",
		"server/server.go": `package server

import "go.putnami.dev/config"

type TLS struct {
	CertFile string ` + "`json:\"certFile\"`" + `
	KeyFile  string ` + "`json:\"keyFile\" sensitive:\"true\"`" + `
}

type Server struct {
	Host string ` + "`json:\"host\"`" + `
	Port int    ` + "`json:\"port\"`" + `
	TLS  TLS    ` + "`json:\"tls\"`" + `
}

var Cfg = config.Config[Server]("server")
`,
	})

	blocks, err := extractFromProject(dir)
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	tls, ok := findField(blocks[0].Fields, "tls")
	if !ok {
		t.Fatal("missing tls field")
	}
	if tls.Type != protocfg.FieldTypeObject {
		t.Errorf("tls type = %q, want object", tls.Type)
	}
	cert, ok := findField(tls.Fields, "certFile")
	if !ok || cert.Type != protocfg.FieldTypeString {
		t.Errorf("nested certFile missing or wrong type: %+v", cert)
	}
	key, ok := findField(tls.Fields, "keyFile")
	if !ok || !key.Sensitive {
		t.Errorf("nested keyFile missing or not sensitive: %+v", key)
	}
}

func TestExtractFromProject_ExpandsCrossPackageStruct(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.25\n",
		"shared/database.go": `package shared

type Database struct {
	URL  string ` + "`json:\"url\" sensitive:\"true\"`" + `
	Pool int    ` + "`json:\"pool\"`" + `
}
`,
		"app/app.go": `package app

import (
	"go.putnami.dev/config"
	"example.com/app/shared"
)

type App struct {
	DB shared.Database ` + "`json:\"db\"`" + `
}

var Cfg = config.Config[App]("app")
`,
	})

	blocks, err := extractFromProject(dir)
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	db, ok := findField(blocks[0].Fields, "db")
	if !ok {
		t.Fatal("missing db field")
	}
	if db.Type != protocfg.FieldTypeObject || len(db.Fields) != 2 {
		t.Errorf("cross-package struct not expanded: %+v", db)
	}
}

func TestExtractFromProject_ArrayAndMap(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.25\n",
		"app/app.go": `package app

import "go.putnami.dev/config"

type Listener struct {
	Host string ` + "`json:\"host\"`" + `
	Port int    ` + "`json:\"port\"`" + `
}

type App struct {
	Listeners []Listener        ` + "`json:\"listeners\"`" + `
	Labels    map[string]string ` + "`json:\"labels\"`" + `
}

var Cfg = config.Config[App]("app")
`,
	})

	blocks, err := extractFromProject(dir)
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	listeners, ok := findField(blocks[0].Fields, "listeners")
	if !ok {
		t.Fatal("missing listeners field")
	}
	if listeners.Type != protocfg.FieldTypeArray || listeners.Items == nil {
		t.Fatalf("listeners not array with items: %+v", listeners)
	}
	if listeners.Items.Type != protocfg.FieldTypeObject || len(listeners.Items.Fields) != 2 {
		t.Errorf("array element struct not expanded: %+v", listeners.Items)
	}

	labels, ok := findField(blocks[0].Fields, "labels")
	if !ok {
		t.Fatal("missing labels field")
	}
	if labels.Type != protocfg.FieldTypeMap || labels.Keys != "string" || labels.Values == nil {
		t.Fatalf("labels not map[string]string: %+v", labels)
	}
}

func TestExtractFromProject_RejectsStructMapKey(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.25\n",
		"app/app.go": `package app

import "go.putnami.dev/config"

type Key struct {
	A string
}

type App struct {
	Bad map[Key]string ` + "`json:\"bad\"`" + `
}

var Cfg = config.Config[App]("app")
`,
	})

	_, err := extractFromProject(dir)
	if err == nil {
		t.Fatal("expected error for non-primitive map key")
	}
	if !strings.Contains(err.Error(), "map key type") {
		t.Errorf("error should mention map key, got: %v", err)
	}
}

func TestExtractFromProject_CycleEmitsOpaqueObject(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.25\n",
		"app/app.go": `package app

import "go.putnami.dev/config"

type Node struct {
	Name string ` + "`json:\"name\"`" + `
	Next *Node  ` + "`json:\"next\"`" + `
}

var Cfg = config.Config[Node]("graph")
`,
	})

	blocks, err := extractFromProject(dir)
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	next, ok := findField(blocks[0].Fields, "next")
	if !ok {
		t.Fatal("missing next field")
	}
	if next.Type != protocfg.FieldTypeObject || len(next.Fields) != 0 {
		t.Errorf("cycle should emit opaque object with no fields, got: %+v", next)
	}
}

func TestExtractFromProject_EmbeddedFieldsAreInlined(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.25\n",
		"app/app.go": `package app

import "go.putnami.dev/config"

type Common struct {
	Name string ` + "`json:\"name\"`" + `
	ID   string ` + "`json:\"id\"`" + `
}

type App struct {
	Common
	Extra string ` + "`json:\"extra\"`" + `
}

var Cfg = config.Config[App]("app")
`,
	})

	blocks, err := extractFromProject(dir)
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	if _, ok := findField(blocks[0].Fields, "name"); !ok {
		t.Error("embedded name not promoted")
	}
	if _, ok := findField(blocks[0].Fields, "id"); !ok {
		t.Error("embedded id not promoted")
	}
	if _, ok := findField(blocks[0].Fields, "extra"); !ok {
		t.Error("outer extra missing")
	}
}

func TestExtractFromProject_DescTagSurfacesDescription(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.25\n",
		"app/app.go": `package app

import "go.putnami.dev/config"

type App struct {
	Host string ` + "`json:\"host\" desc:\"Server bind address\"`" + `
}

var Cfg = config.Config[App]("app")
`,
	})

	blocks, err := extractFromProject(dir)
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	host, ok := findField(blocks[0].Fields, "host")
	if !ok || host.Description != "Server bind address" {
		t.Errorf("description not surfaced: %+v", host)
	}
}

func TestExtractFromProject_ResolvesNamedTypeAlias(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.25\n",
		"shared/database.go": `package shared

type Database struct {
	URL string ` + "`json:\"url\"`" + `
}
`,
		"app/app.go": `package app

import (
	"go.putnami.dev/config"
	"example.com/app/shared"
)

// Both forms must resolve: alias (=) and named type without =.
type DBAlias = shared.Database
type DBNamed shared.Database

type App struct {
	A DBAlias ` + "`json:\"a\"`" + `
	B DBNamed ` + "`json:\"b\"`" + `
}

var Cfg = config.Config[App]("app")
`,
	})

	blocks, err := extractFromProject(dir)
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	for _, name := range []string{"a", "b"} {
		f, ok := findField(blocks[0].Fields, name)
		if !ok {
			t.Fatalf("missing field %q", name)
		}
		if f.Type != protocfg.FieldTypeObject || len(f.Fields) != 1 {
			t.Errorf("field %q did not resolve through indirection: %+v", name, f)
		}
	}
}

func TestExtractFromProject_AliasCycleEmitsOpaqueObject(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.25\n",
		"app/app.go": `package app

import "go.putnami.dev/config"

// Mutually-recursive aliases: resolution must terminate without panicking.
type A B
type B A

type App struct {
	Wrap A ` + "`json:\"wrap\"`" + `
}

var Cfg = config.Config[App]("app")
`,
	})

	blocks, err := extractFromProject(dir)
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	wrap, ok := findField(blocks[0].Fields, "wrap")
	if !ok {
		t.Fatal("missing wrap field")
	}
	if wrap.Type != protocfg.FieldTypeObject || len(wrap.Fields) != 0 {
		t.Errorf("alias cycle should emit opaque object, got: %+v", wrap)
	}
}

func TestExtractFromProject_DuplicateConfigPathErrors(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.25\n",
		"app/a.go": `package app

import "go.putnami.dev/config"

type Server struct {
	Host string ` + "`json:\"host\"`" + `
}

var A = config.Config[Server]("server")
`,
		"app/b.go": `package app

import "go.putnami.dev/config"

type ServerAlt struct {
	Port int ` + "`json:\"port\"`" + `
}

var B = config.Config[ServerAlt]("server")
`,
	})

	_, err := extractFromProject(dir)
	if err == nil {
		t.Fatal("expected error for duplicate Config path")
	}
	if !strings.Contains(err.Error(), `duplicate config path "server"`) {
		t.Errorf("error should call out the duplicate path; got: %v", err)
	}
	if !strings.Contains(err.Error(), "a.go") || !strings.Contains(err.Error(), "b.go") {
		t.Errorf("error should list both source files; got: %v", err)
	}
	// Position annotations make the error actionable — point users at the
	// exact line, not just the file.
	if !strings.Contains(err.Error(), "a.go:9") || !strings.Contains(err.Error(), "b.go:9") {
		t.Errorf("error should include line numbers; got: %v", err)
	}
}

func TestAliasFromImportPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		// Plain modules — basename wins.
		{"go.putnami.dev/config", "config"},
		{"example.com/foo/bar", "bar"},
		// gopkg.in SIV: ".vN" suffix is stripped to recover the package name.
		{"gopkg.in/yaml.v3", "yaml"},
		{"gopkg.in/check.v1", "check"},
		// go-modules SIV: when basename is "vN", the package name comes
		// from the parent component.
		{"github.com/foo/bar/v2", "bar"},
		{"example.com/very/deep/pkg/v10", "pkg"},
		// Defensive: a bare "v2" path with no parent must not crash and
		// should fall back to the basename.
		{"v2", "v2"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := aliasFromImportPath(tt.path); got != tt.want {
				t.Errorf("aliasFromImportPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
