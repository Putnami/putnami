package extension

import (
	"os"
	"path/filepath"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

func TestBuildJobMap(t *testing.T) {
	exts := []*ExtensionDescription{
		{
			Name: "@putnami/ts",
			Jobs: map[string]*JobDefinition{
				"build": {Name: "build", ExtensionName: "@putnami/ts"},
				"test":  {Name: "test", ExtensionName: "@putnami/ts"},
			},
		},
		{
			Name: "@putnami/go",
			Jobs: map[string]*JobDefinition{
				"build": {Name: "build", ExtensionName: "@putnami/go"},
				"lint":  {Name: "lint", ExtensionName: "@putnami/go"},
			},
		},
	}

	jobMap := BuildJobMap(exts)

	if len(jobMap["build"]) != 2 {
		t.Errorf("build jobs = %d, want 2", len(jobMap["build"]))
	}
	if len(jobMap["test"]) != 1 {
		t.Errorf("test jobs = %d, want 1", len(jobMap["test"]))
	}
	if len(jobMap["lint"]) != 1 {
		t.Errorf("lint jobs = %d, want 1", len(jobMap["lint"]))
	}
}

func TestFindExtensionByName(t *testing.T) {
	exts := []*ExtensionDescription{
		{Name: "@putnami/ts"},
		{Name: "@putnami/go"},
	}

	found := FindExtensionByName(exts, "@putnami/go")
	if found == nil {
		t.Fatal("FindExtensionByName should find @putnami/go")
	}
	if found.Name != "@putnami/go" {
		t.Errorf("Name = %q, want %q", found.Name, "@putnami/go")
	}

	notFound := FindExtensionByName(exts, "@putnami/python")
	if notFound != nil {
		t.Error("FindExtensionByName should return nil for unknown extension")
	}
}

func TestDiscoverExtensions_FromProjectPaths(t *testing.T) {
	dir := t.TempDir()

	// Create an extension project
	extDir := filepath.Join(dir, "extensions", "test-ext")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}

	manifest := `{
		"name": "@putnami/test-ext",
		"cliContract": 4,
		"commands": {
			"build": {
				"run": [{"id": "build", "task": "build-exec"}]
			}
		},
		"tasks": {
			"build-exec": {
				"kind": "command",
				"command": "echo",
				"args": ["build"]
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &wsproto.Config{}
	exts, err := DiscoverExtensions(dir, cfg, []string{"extensions/test-ext"})
	if err != nil {
		t.Fatalf("DiscoverExtensions: %v", err)
	}
	if len(exts) != 1 {
		t.Fatalf("expected 1 extension, got %d", len(exts))
	}
	if exts[0].Name != "@putnami/test-ext" {
		t.Errorf("Name = %q, want %q", exts[0].Name, "@putnami/test-ext")
	}
	if !exts[0].LocalSource {
		t.Error("project-scanned extension should retain local-source provenance")
	}
}

func TestDiscoverExtensions_Dedup(t *testing.T) {
	dir := t.TempDir()

	extDir := filepath.Join(dir, "ext")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}

	manifest := `{
		"name": "@putnami/test",
		"cliContract": 4,
		"commands": {"build": {"run": [{"id": "b", "task": "t"}]}},
		"tasks": {"t": {"kind": "command", "command": "echo"}}
	}`
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{"ext": ""}}}
	// Pass the same path twice: once in projectPaths, once in cfg.Extensions
	exts, err := DiscoverExtensions(dir, cfg, []string{"ext"})
	if err != nil {
		t.Fatalf("DiscoverExtensions: %v", err)
	}
	if len(exts) != 1 {
		t.Errorf("expected 1 extension (deduplicated), got %d", len(exts))
	}
}

func TestDiscoverExtensions_NormalizesLeadingSlashWorkspaceRef(t *testing.T) {
	dir := t.TempDir()

	extDir := filepath.Join(dir, "go", "extension")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}

	manifest := `{
		"name": "@putnami/go",
		"cliContract": 4,
		"commands": {"build": {"run": [{"id": "b", "task": "t"}]}},
		"tasks": {"t": {"kind": "command", "command": "echo"}}
	}`
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{"/go/extension": ""}}}
	exts, err := DiscoverExtensions(dir, cfg, nil)
	if err != nil {
		t.Fatalf("DiscoverExtensions: %v", err)
	}
	if len(exts) != 1 {
		t.Fatalf("expected 1 extension, got %d", len(exts))
	}
	if exts[0].Path != extDir {
		t.Errorf("Path = %q, want %q", exts[0].Path, extDir)
	}
	// The relative path keeps this platform's separators; its consumers
	// normalize it to slash form where they compare it with a project path.
	wantRel := filepath.FromSlash("go/extension")
	if exts[0].RelPath != wantRel {
		t.Errorf("RelPath = %q, want %q", exts[0].RelPath, wantRel)
	}
	if !exts[0].LocalSource {
		t.Error("workspace-relative extension should retain local-source provenance")
	}
	if got := exts[0].Jobs["build"].ExtensionPath; got != wantRel {
		t.Errorf("job ExtensionPath = %q, want %q", got, wantRel)
	}
}

func TestDiscoverExtensions_RejectsWorkspaceRefTraversal(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "workspace")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	outsideDir := filepath.Join(parent, "outside")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}

	manifest := `{
		"name": "@putnami/outside",
		"cliContract": 4,
		"commands": {"build": {"run": [{"id": "b", "task": "t"}]}},
		"tasks": {"t": {"kind": "command", "command": "echo"}}
	}`
	if err := os.WriteFile(filepath.Join(outsideDir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{"../outside": ""}}}
	exts, err := DiscoverExtensions(dir, cfg, nil)
	if err != nil {
		t.Fatalf("DiscoverExtensions: %v", err)
	}
	if len(exts) != 0 {
		t.Fatalf("expected traversal ref to be ignored, got %d extension(s)", len(exts))
	}
}

func TestDiscoverExtensions_PreservesAbsoluteExtensionRef(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "workspace")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	extDir := filepath.Join(parent, "shared-ext")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}

	manifest := `{
		"name": "@putnami/shared",
		"cliContract": 4,
		"commands": {"build": {"run": [{"id": "b", "task": "t"}]}},
		"tasks": {"t": {"kind": "command", "command": "echo"}}
	}`
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{extDir: ""}}}
	exts, err := DiscoverExtensions(dir, cfg, nil)
	if err != nil {
		t.Fatalf("DiscoverExtensions: %v", err)
	}
	if len(exts) != 1 {
		t.Fatalf("expected 1 extension, got %d", len(exts))
	}
	if exts[0].Path != extDir {
		t.Errorf("Path = %q, want %q", exts[0].Path, extDir)
	}
	if exts[0].RelPath != "" {
		t.Errorf("RelPath = %q, want empty for absolute extension ref", exts[0].RelPath)
	}
	if !exts[0].LocalSource {
		t.Error("absolute extension ref should retain local-source provenance")
	}
	if got := exts[0].Jobs["build"].ExtensionPath; got != "" {
		t.Errorf("job ExtensionPath = %q, want empty for absolute extension ref", got)
	}
}

func TestDiscoverExtensions_EmptyInputs(t *testing.T) {
	dir := t.TempDir()
	cfg := &wsproto.Config{}
	exts, err := DiscoverExtensions(dir, cfg, nil)
	if err != nil {
		t.Fatalf("DiscoverExtensions: %v", err)
	}
	if len(exts) != 0 {
		t.Errorf("expected 0 extensions, got %d", len(exts))
	}
}

func TestDiscoverExtensions_NameFromPutnamiJSON(t *testing.T) {
	dir := t.TempDir()

	extDir := filepath.Join(dir, "ext")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Manifest without a name
	manifest := `{
		"cliContract": 4,
		"commands": {"build": {"run": [{"id": "b", "task": "t"}]}},
		"tasks": {"t": {"kind": "command", "command": "echo"}}
	}`
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	// putnami.json with a name
	rc := `{"name": "@putnami/from-rc"}`
	if err := os.WriteFile(filepath.Join(extDir, wsproto.ConfigFilename), []byte(rc), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &wsproto.Config{}
	exts, err := DiscoverExtensions(dir, cfg, []string{"ext"})
	if err != nil {
		t.Fatalf("DiscoverExtensions: %v", err)
	}
	if len(exts) != 1 {
		t.Fatalf("expected 1, got %d", len(exts))
	}
	if exts[0].Name != "@putnami/from-rc" {
		t.Errorf("Name = %q, want %q", exts[0].Name, "@putnami/from-rc")
	}
}

func TestDiscoverExtensions_NameFromPackageJSON(t *testing.T) {
	dir := t.TempDir()

	extDir := filepath.Join(dir, "ext")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}

	manifest := `{
		"cliContract": 4,
		"commands": {"build": {"run": [{"id": "b", "task": "t"}]}},
		"tasks": {"t": {"kind": "command", "command": "echo"}}
	}`
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	pkg := `{"name": "@putnami/from-pkg", "version": "2.0.0"}`
	if err := os.WriteFile(filepath.Join(extDir, "package.json"), []byte(pkg), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &wsproto.Config{}
	exts, err := DiscoverExtensions(dir, cfg, []string{"ext"})
	if err != nil {
		t.Fatalf("DiscoverExtensions: %v", err)
	}
	if exts[0].Name != "@putnami/from-pkg" {
		t.Errorf("Name = %q, want %q", exts[0].Name, "@putnami/from-pkg")
	}
	if exts[0].Version != "2.0.0" {
		t.Errorf("Version = %q, want %q", exts[0].Version, "2.0.0")
	}
}

func TestExtractDevDeps(t *testing.T) {
	data := []byte(`{"devDependencies": {"@putnami/ts": "^1.0.0", "@putnami/go": "^2.0.0"}}`)
	deps := extractDevDeps(data)
	if len(deps) != 2 {
		t.Fatalf("len = %d, want 2", len(deps))
	}
	if deps["@putnami/ts"] != "^1.0.0" {
		t.Errorf("ts = %q, want %q", deps["@putnami/ts"], "^1.0.0")
	}
}

func TestExtractDevDeps_InvalidJSON(t *testing.T) {
	deps := extractDevDeps([]byte("{invalid"))
	if deps != nil {
		t.Error("extractDevDeps with invalid JSON should return nil")
	}
}

func TestExtractDevDeps_NoDeps(t *testing.T) {
	deps := extractDevDeps([]byte(`{"name": "test"}`))
	if deps != nil {
		t.Error("extractDevDeps with no devDependencies should return nil")
	}
}

func TestSelectPreparedExtensionsReplacesAmbientAndHonorsUnavailable(t *testing.T) {
	root := t.TempDir()
	manifest := `{"name":"@putnami/cloud","version":"1.2.3","cliContract":4,"commands":{}}`
	if err := os.WriteFile(filepath.Join(root, ManifestFilename), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	ambient := &ExtensionDescription{Name: "@putnami/cloud", Version: "9.9.9", Path: "/ambient/cloud"}
	local := &ExtensionDescription{Name: "local", Version: "dev", Path: "/workspace/local", LocalSource: true}

	selected := SelectPreparedExtensions([]*ExtensionDescription{ambient, local}, map[string]string{"@putnami/cloud": root})
	if len(selected) != 2 || selected[0] != local {
		t.Fatalf("prepared selection = %+v, want unrelated local plus exact cloud", selected)
	}
	if selected[1].Name != "@putnami/cloud" || selected[1].Version != "1.2.3" || selected[1].Path != root || selected[1].LocalSource {
		t.Fatalf("selected cloud = %+v, want exact installed root", selected[1])
	}

	selected = SelectPreparedExtensions([]*ExtensionDescription{ambient, local}, map[string]string{"@putnami/cloud": ""})
	if len(selected) != 1 || selected[0] != local {
		t.Fatalf("unavailable selection = %+v, want only unrelated local extension", selected)
	}
}
