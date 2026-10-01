package context

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParams_String_Present(t *testing.T) {
	p := Params{"key": json.RawMessage(`"hello"`)}
	if got := p.String("key"); got != "hello" {
		t.Errorf("String = %q, want %q", got, "hello")
	}
}

func TestParams_String_Missing(t *testing.T) {
	p := Params{}
	if got := p.String("key"); got != "" {
		t.Errorf("String = %q, want empty", got)
	}
}

func TestParams_String_Fallback(t *testing.T) {
	p := Params{"alt": json.RawMessage(`"world"`)}
	if got := p.String("key", "alt"); got != "world" {
		t.Errorf("String = %q, want %q", got, "world")
	}
}

func TestParams_String_MultipleFallbacks(t *testing.T) {
	p := Params{"third": json.RawMessage(`"found"`)}
	if got := p.String("first", "second", "third"); got != "found" {
		t.Errorf("String = %q, want %q", got, "found")
	}
}

func TestParams_String_RawValue(t *testing.T) {
	p := Params{"key": json.RawMessage(`42`)}
	if got := p.String("key"); got != "42" {
		t.Errorf("String = %q, want %q", got, "42")
	}
}

func TestParams_Bool_True(t *testing.T) {
	p := Params{"flag": json.RawMessage(`true`)}
	if got := p.Bool("flag", false); !got {
		t.Error("Bool = false, want true")
	}
}

func TestParams_Bool_False(t *testing.T) {
	p := Params{"flag": json.RawMessage(`false`)}
	if got := p.Bool("flag", true); got {
		t.Error("Bool = true, want false")
	}
}

func TestParams_Bool_Missing(t *testing.T) {
	p := Params{}
	if got := p.Bool("flag", true); !got {
		t.Error("Bool missing should return default true")
	}
	if got := p.Bool("flag", false); got {
		t.Error("Bool missing should return default false")
	}
}

func TestParams_Bool_StringTruthy(t *testing.T) {
	tests := []struct {
		val  string
		want bool
	}{
		{`"true"`, true},
		{`"1"`, true},
		{`"yes"`, true},
		{`"on"`, true},
		{`"false"`, false},
		{`"0"`, false},
		{`"no"`, false},
		{`"off"`, false},
	}

	for _, tt := range tests {
		p := Params{"flag": json.RawMessage(tt.val)}
		if got := p.Bool("flag", !tt.want); got != tt.want {
			t.Errorf("Bool(%s) = %v, want %v", tt.val, got, tt.want)
		}
	}
}

func TestParams_Bool_Fallback(t *testing.T) {
	p := Params{"alt": json.RawMessage(`true`)}
	if got := p.Bool("flag", false, "alt"); !got {
		t.Error("Bool fallback should return true from alt key")
	}
}

func TestParams_Int_Present(t *testing.T) {
	p := Params{"count": json.RawMessage(`42`)}
	if got := p.Int("count", 0); got != 42 {
		t.Errorf("Int = %d, want 42", got)
	}
}

func TestParams_Int_Missing(t *testing.T) {
	p := Params{}
	if got := p.Int("count", 10); got != 10 {
		t.Errorf("Int missing = %d, want 10", got)
	}
}

func TestParams_Int_Fallback(t *testing.T) {
	p := Params{"alt": json.RawMessage(`7`)}
	if got := p.Int("count", 0, "alt"); got != 7 {
		t.Errorf("Int fallback = %d, want 7", got)
	}
}

func TestParams_Int_Float(t *testing.T) {
	p := Params{"count": json.RawMessage(`3.9`)}
	if got := p.Int("count", 0); got != 3 {
		t.Errorf("Int float = %d, want 3 (truncated)", got)
	}
}

func TestParams_Int_NumericString(t *testing.T) {
	p := Params{"count": json.RawMessage(`"42"`)}
	if got := p.Int("count", 0); got != 42 {
		t.Errorf("Int string = %d, want 42", got)
	}
}

func TestParams_Int_FloatString(t *testing.T) {
	p := Params{"count": json.RawMessage(`"3.9"`)}
	if got := p.Int("count", 0); got != 3 {
		t.Errorf("Int float string = %d, want 3 (truncated)", got)
	}
}

func TestParams_Int_NonNumericString(t *testing.T) {
	p := Params{"count": json.RawMessage(`"abc"`)}
	if got := p.Int("count", 5); got != 5 {
		t.Errorf("Int non-numeric = %d, want default 5", got)
	}
}

func TestParams_Float_Present(t *testing.T) {
	p := Params{"threshold": json.RawMessage(`85.5`)}
	if got := p.Float("threshold", 0); got != 85.5 {
		t.Errorf("Float = %v, want 85.5", got)
	}
}

func TestParams_Float_Integer(t *testing.T) {
	p := Params{"threshold": json.RawMessage(`80`)}
	if got := p.Float("threshold", 0); got != 80 {
		t.Errorf("Float = %v, want 80", got)
	}
}

func TestParams_Float_Missing(t *testing.T) {
	p := Params{}
	if got := p.Float("threshold", 12.5); got != 12.5 {
		t.Errorf("Float missing = %v, want 12.5", got)
	}
}

func TestParams_Float_Fallback(t *testing.T) {
	p := Params{"coverageThreshold": json.RawMessage(`70`)}
	if got := p.Float("coverage-threshold", 0, "coverageThreshold"); got != 70 {
		t.Errorf("Float fallback = %v, want 70", got)
	}
}

func TestParams_Float_NumericString(t *testing.T) {
	p := Params{"threshold": json.RawMessage(`"90.25"`)}
	if got := p.Float("threshold", 0); got != 90.25 {
		t.Errorf("Float string = %v, want 90.25", got)
	}
}

func TestParams_Float_NonNumericString(t *testing.T) {
	p := Params{"threshold": json.RawMessage(`"abc"`)}
	if got := p.Float("threshold", 5); got != 5 {
		t.Errorf("Float non-numeric = %v, want default 5", got)
	}
}

func TestContext_PublishChannels_Nil(t *testing.T) {
	ctx := &Context{}
	if channels := ctx.PublishChannels(); channels != nil {
		t.Errorf("PublishChannels = %v, want nil", channels)
	}
}

func TestContext_PublishChannels_Valid(t *testing.T) {
	ctx := &Context{
		Project: Project{
			Publish: json.RawMessage(`["npm", "docker"]`),
		},
	}
	channels := ctx.PublishChannels()
	if len(channels) != 2 {
		t.Fatalf("len(channels) = %d, want 2", len(channels))
	}
	if channels[0] != "npm" || channels[1] != "docker" {
		t.Errorf("channels = %v, want [npm, docker]", channels)
	}
}

func TestContext_PublishChannels_InvalidJSON(t *testing.T) {
	ctx := &Context{
		Project: Project{
			Publish: json.RawMessage(`{"invalid": true}`),
		},
	}
	if channels := ctx.PublishChannels(); channels != nil {
		t.Errorf("PublishChannels invalid JSON = %v, want nil", channels)
	}
}

func TestContext_HasPublishChannel(t *testing.T) {
	ctx := &Context{
		Project: Project{
			Publish: json.RawMessage(`["npm", "docker"]`),
		},
	}
	if !ctx.HasPublishChannel("npm") {
		t.Error("HasPublishChannel(npm) = false, want true")
	}
	if !ctx.HasPublishChannel("docker") {
		t.Error("HasPublishChannel(docker) = false, want true")
	}
	if ctx.HasPublishChannel("pypi") {
		t.Error("HasPublishChannel(pypi) = true, want false")
	}
}

func TestContext_HasPublishChannel_Empty(t *testing.T) {
	ctx := &Context{}
	if ctx.HasPublishChannel("npm") {
		t.Error("HasPublishChannel on empty context should be false")
	}
}

func TestProject_GetBinString(t *testing.T) {
	p := &Project{Bin: json.RawMessage(`"./dist/bin.js"`)}
	if got := p.GetBinString(); got != "./dist/bin.js" {
		t.Errorf("GetBinString = %q, want %q", got, "./dist/bin.js")
	}
}

func TestProject_GetBinString_Object(t *testing.T) {
	p := &Project{Bin: json.RawMessage(`{"cli": "./dist/cli.js"}`)}
	if got := p.GetBinString(); got != "" {
		t.Errorf("GetBinString object = %q, want empty", got)
	}
}

func TestProject_GetBinString_Nil(t *testing.T) {
	p := &Project{}
	if got := p.GetBinString(); got != "" {
		t.Errorf("GetBinString nil = %q, want empty", got)
	}
}

func TestGenerateSchemaCommit(t *testing.T) {
	tests := []struct {
		name         string
		options      map[string]json.RawMessage
		wantCommit   bool
		wantDeclared bool
	}{
		{"schema false", map[string]json.RawMessage{"generate": json.RawMessage(`{"schema":false}`)}, false, true},
		{"schema true", map[string]json.RawMessage{"generate": json.RawMessage(`{"schema":true}`)}, true, true},
		{"other generate options only", map[string]json.RawMessage{"generate": json.RawMessage(`{"assets":[]}`)}, false, false},
		{"no generate key", map[string]json.RawMessage{"build": json.RawMessage(`{"lib":true}`)}, false, false},
		{"invalid generate JSON", map[string]json.RawMessage{"generate": json.RawMessage(`not-json`)}, false, false},
		{"no options", nil, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &Context{Project: Project{Options: tt.options}}
			commit, declared := GenerateSchemaCommit(ctx)
			if commit != tt.wantCommit || declared != tt.wantDeclared {
				t.Errorf("GenerateSchemaCommit = (%v, %v), want (%v, %v)", commit, declared, tt.wantCommit, tt.wantDeclared)
			}
		})
	}
}

func TestGenerateSchemaCommit_NilContext(t *testing.T) {
	if commit, declared := GenerateSchemaCommit(nil); commit || declared {
		t.Errorf("GenerateSchemaCommit(nil) = (%v, %v), want (false, false)", commit, declared)
	}
}

func TestParse_Valid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "context.json")

	data := `{
		"workspaceRoot": "/workspace",
		"outputPath": "/out",
		"cacheRoot": "/cache",
		"workspace": {"name": "test", "version": "1.0.0"},
		"project": {"name": "myapp", "fullPath": "/workspace/myapp", "path": "myapp"},
		"extension": {"name": "@putnami/go", "root": "/ext/go"},
		"job": {"name": "build"},
		"params": {"target": "\"linux/amd64\"", "verbose": "true"},
		"filePatterns": ["**/*.go"],
		"version": {"sha": "abc123", "branch": "main", "isDirty": false, "tag": "v1.0.0", "suffix": ""}
	}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if ctx.WorkspaceRoot != "/workspace" {
		t.Errorf("WorkspaceRoot = %q, want %q", ctx.WorkspaceRoot, "/workspace")
	}
	if ctx.OutputPath != "/out" {
		t.Errorf("OutputPath = %q, want %q", ctx.OutputPath, "/out")
	}
	if ctx.Workspace.Name != "test" {
		t.Errorf("Workspace.Name = %q, want %q", ctx.Workspace.Name, "test")
	}
	if ctx.Project.Name != "myapp" {
		t.Errorf("Project.Name = %q, want %q", ctx.Project.Name, "myapp")
	}
	if ctx.Project.Path != "myapp" {
		t.Errorf("Project.Path = %q, want %q", ctx.Project.Path, "myapp")
	}
	if ctx.Extension.Name != "@putnami/go" {
		t.Errorf("Extension.Name = %q, want %q", ctx.Extension.Name, "@putnami/go")
	}
	if ctx.Extension.Root != "/ext/go" {
		t.Errorf("Extension.Root = %q, want %q", ctx.Extension.Root, "/ext/go")
	}
	if ctx.Job.Name != "build" {
		t.Errorf("Job.Name = %q, want %q", ctx.Job.Name, "build")
	}
	if len(ctx.FilePatterns) != 1 || ctx.FilePatterns[0] != "**/*.go" {
		t.Errorf("FilePatterns = %v, want [**/*.go]", ctx.FilePatterns)
	}
	if ctx.Version == nil {
		t.Fatal("Version should not be nil")
	}
	if ctx.Version.SHA != "abc123" {
		t.Errorf("Version.SHA = %q, want %q", ctx.Version.SHA, "abc123")
	}
	if ctx.Version.IsDirty {
		t.Error("Version.IsDirty = true, want false")
	}
}

func TestParse_FileNotFound(t *testing.T) {
	_, err := Parse("/nonexistent/file.json")
	if err == nil {
		t.Error("Parse nonexistent file should return error")
	}
}

func TestParse_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "context.json")
	if err := os.WriteFile(path, []byte("{invalid"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Parse(path)
	if err == nil {
		t.Error("Parse invalid JSON should return error")
	}
}

func TestParse_EmptyProject(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "context.json")

	data := `{
		"workspaceRoot": "/workspace",
		"project": {},
		"extension": {"name": "@putnami/go"},
		"job": {"name": "build"}
	}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ctx.Project.Name != "" {
		t.Errorf("Project.Name = %q, want empty", ctx.Project.Name)
	}
}

// TestParse_Selection pins the SDK's half of the resolved-selection contract:
// an extension reads the run's scope off the wire instead of
// re-deriving it from flags it was never given.
func TestParse_Selection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "context.json")
	data := `{
		"protocolVersion": 2,
		"workspaceRoot": "/workspace",
		"outputPath": "/out",
		"cacheRoot": "/cache",
		"workspace": {"name": "test"},
		"project": {"name": "myapp", "fullPath": "/workspace/myapp", "path": "myapp"},
		"selection": {
			"mode": "impacted",
			"scoped": true,
			"baseline": "origin/main",
			"baselineSource": "trunk",
			"projects": ["/apps/console", "/libs/widget"]
		},
		"extension": {"name": "@putnami/sdd", "root": "/ext/sdd"},
		"job": {"name": "validate"},
		"params": {}
	}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ctx.Selection == nil {
		t.Fatal("Selection should not be nil")
	}
	if ctx.Selection.Mode != SelectionModeImpacted {
		t.Errorf("Selection.Mode = %q, want %q", ctx.Selection.Mode, SelectionModeImpacted)
	}
	if !ctx.Selection.Scoped {
		t.Error("Selection.Scoped = false, want true for an impacted run")
	}
	if ctx.Selection.Baseline != "origin/main" || ctx.Selection.BaselineSource != "trunk" {
		t.Errorf("Selection baseline = %q/%q, want origin/main/trunk",
			ctx.Selection.Baseline, ctx.Selection.BaselineSource)
	}
	if len(ctx.Selection.ProjectIDs) != 2 || ctx.Selection.ProjectIDs[0] != "/apps/console" {
		t.Errorf("Selection.ProjectIDs = %v, want [/apps/console /libs/widget]", ctx.Selection.ProjectIDs)
	}
	if ctx.Selection.EmptyImpact {
		t.Error("Selection.EmptyImpact = true, want false for a non-empty impacted set")
	}

	// The mode vocabulary is the protocol's, re-exported rather than restated.
	for _, mode := range []string{SelectionModeAll, SelectionModeProjects, SelectionModeImpacted} {
		if mode == "" {
			t.Error("a selection mode constant is empty")
		}
	}
}

// A context from an orchestrator that resolved nothing leaves the member nil.
// A consumer must read that as "unknown", never as "the run was unscoped".
func TestParse_SelectionAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "context.json")
	data := `{
		"workspaceRoot": "/workspace",
		"project": {"name": "myapp", "path": "myapp", "fullPath": "/workspace/myapp"},
		"extension": {"name": "@putnami/go"},
		"job": {"name": "build"}
	}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ctx.Selection != nil {
		t.Errorf("Selection = %+v, want nil when the orchestrator resolved none", ctx.Selection)
	}
}

// TestParse_UserScope pins the SDK's half of the user-scope member: an
// extension reads the caller directory, and the presence of the member is how
// it knows no workspace exists. A workspace context leaves it nil.
func TestParse_UserScope(t *testing.T) {
	dir := t.TempDir()
	read := func(t *testing.T, userScope string) *Context {
		t.Helper()
		path := filepath.Join(dir, "context.json")
		data := `{
			"protocolVersion": 2,
			"workspaceRoot": "/home/u/.putnami/user",
			"outputPath": "/home/u/.putnami/user/.putnami/out/audit-run",
			"cacheRoot": "/home/u/.putnami/user/.putnami/cache",
			"workspace": {"name": "user"},
			"project": {"name": "user", "fullPath": "/home/u/.putnami/user", "path": "."},` + userScope + `
			"extension": {"name": "@acme/audit", "root": "/ext/audit"},
			"job": {"name": "audit-run"},
			"params": {}
		}`
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		ctx, err := Parse(path)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		return ctx
	}

	callerDir := func(scope *UserScope) string {
		if scope == nil {
			return ""
		}
		return scope.CallerDir
	}
	ctx := read(t, `"userScope": {"callerDir": "/home/u/src/repository"},`)
	if got := callerDir(ctx.UserScope); got != "/home/u/src/repository" {
		t.Fatalf("UserScope.CallerDir = %q, want the caller directory", got)
	}
	if workspaceCtx := read(t, ""); workspaceCtx.UserScope != nil {
		t.Fatalf("a workspace context carries UserScope %+v", workspaceCtx.UserScope)
	}
}
