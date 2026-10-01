package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestLineTagPatternDefaults(t *testing.T) {
	cases := []struct {
		name      string
		scopePath string
		line      *LineConfig
		want      string
	}{
		{"root line", "", &LineConfig{}, "v{version}"},
		{"scope line", "typescript", &LineConfig{}, "typescript/v{version}"},
		{"nested scope line", "go/framework", &LineConfig{}, "go/framework/v{version}"},
		{"declared pattern wins", "typescript", &LineConfig{Tag: "ts/v{version}"}, "ts/v{version}"},
		{"trailing slash is not a segment", "typescript/", &LineConfig{}, "typescript/v{version}"},
		{"no line block still has the root default", "", nil, "v{version}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LineTagPattern(tc.scopePath, tc.line); got != tc.want {
				t.Fatalf("LineTagPattern(%q, %+v) = %q, want %q", tc.scopePath, tc.line, got, tc.want)
			}
		})
	}
}

func TestRenderAndParseLineTag(t *testing.T) {
	lines := map[string]string{
		"":           LineTagPattern("", nil),
		"typescript": LineTagPattern("typescript", &LineConfig{Tag: "ts/v{version}"}),
		"go":         LineTagPattern("go", &LineConfig{Tag: "go/v{version}"}),
	}

	for scopePath, pattern := range lines {
		tag := RenderLineTag(pattern, "0.3.0")
		gotScope, gotVersion, ok := ParseLineTag(tag, lines)
		if !ok {
			t.Fatalf("ParseLineTag(%q) did not match any line", tag)
		}
		if gotScope != scopePath || gotVersion != "0.3.0" {
			t.Fatalf("ParseLineTag(%q) = (%q, %q), want (%q, %q)", tag, gotScope, gotVersion, scopePath, "0.3.0")
		}
	}

	// The most specific pattern wins: "ts/v{version}" claims the tag even
	// though the root line "v{version}" is declared too.
	if got := RenderLineTag(lines["typescript"], "1.2.3"); got != "ts/v1.2.3" {
		t.Fatalf("RenderLineTag = %q, want ts/v1.2.3", got)
	}
	scopePath, version, ok := ParseLineTag("ts/v1.2.3", lines)
	if !ok || scopePath != "typescript" || version != "1.2.3" {
		t.Fatalf("ParseLineTag(ts/v1.2.3) = (%q, %q, %v), want (typescript, 1.2.3, true)", scopePath, version, ok)
	}

	// A tag no line produced is not attributed to one.
	if _, _, ok := ParseLineTag("nightly-2026-09-04", lines); ok {
		t.Error("ParseLineTag matched a tag no line pattern produces")
	}
	// A tag that is the bare pattern prefix carries no version.
	if _, _, ok := ParseLineTag("ts/v", lines); ok {
		t.Error("ParseLineTag accepted an empty version")
	}
}

func TestScopeLineIsNotInherited(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "typescript", "framework", "web")

	writeFile(t, filepath.Join(root, "typescript", ConfigFilename), `{
		"tags": ["ts"],
		"line": { "tag": "ts/v{version}" }
	}`)
	writeFile(t, filepath.Join(root, "typescript", "framework", ConfigFilename), `{
		"tags": ["framework"]
	}`)
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}

	sc := LoadScopeChain(root, project)
	if sc == nil {
		t.Fatal("expected a merged scope chain")
	}
	if sc.Line != nil {
		t.Fatalf("merged chain carries a line block %+v; a line belongs to the scope that declares it", sc.Line)
	}
	if !ReadScopeConfig(filepath.Join(root, "typescript")).IsLine() {
		t.Error("the declaring scope should still report itself as a line")
	}
	if ReadScopeConfig(filepath.Join(root, "typescript", "framework")).IsLine() {
		t.Error("a scope below a line must not become a line itself")
	}
}

func TestScanAllScopesCollectsLines(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "typescript", ConfigFilename), `{"line": {"tag": "ts/v{version}"}}`)
	writeFile(t, filepath.Join(root, "go", ConfigFilename), `{"line": {}}`)

	index, err := ScanAllScopes(root, []string{"typescript/framework/web", "go/framework/http"})
	if err != nil {
		t.Fatalf("ScanAllScopes: %v", err)
	}
	want := map[string]string{"typescript": "ts/v{version}", "go": "go/v{version}"}
	for scopePath, pattern := range want {
		if index.Lines[scopePath] != pattern {
			t.Errorf("line %q = %q, want %q", scopePath, index.Lines[scopePath], pattern)
		}
	}
	if len(index.Lines) != len(want) {
		t.Errorf("lines = %v, want exactly %v", index.Lines, want)
	}
}

func TestScanAllScopesWithoutLinesIsOneLine(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "typescript", ConfigFilename), `{"tags": ["ts"]}`)

	index, err := ScanAllScopes(root, []string{"typescript/framework/web"})
	if err != nil {
		t.Fatalf("ScanAllScopes: %v", err)
	}
	if len(index.Lines) != 1 || index.Lines[""] != "v{version}" {
		t.Fatalf("lines = %v, want the implicit root line {\"\": \"v{version}\"}", index.Lines)
	}
}

func TestNestedLineIsAnError(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go", ConfigFilename), `{"line": {}}`)
	writeFile(t, filepath.Join(root, "go", "framework", ConfigFilename), `{"line": {}}`)

	_, err := ScanAllScopes(root, []string{"go/framework/http"})
	if err == nil {
		t.Fatal("expected an error for a line declared under another line")
	}

	// A sibling whose path merely shares a prefix is not nested.
	sibling := t.TempDir()
	writeFile(t, filepath.Join(sibling, "go", ConfigFilename), `{"line": {}}`)
	writeFile(t, filepath.Join(sibling, "golang", ConfigFilename), `{"line": {}}`)
	if _, err := ScanAllScopes(sibling, []string{"go/http", "golang/http"}); err != nil {
		t.Fatalf("ScanAllScopes over sibling lines: %v", err)
	}
}

func TestValidateScopeLine(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"valid pattern", `{"line": {"tag": "ts/v{version}"}}`, ""},
		{"default pattern", `{"line": {}}`, ""},
		{"two placeholders", `{"line": {"tag": "ts/v{version}-{version}"}}`, "invalid-line"},
		{"no placeholder", `{"line": {"tag": "ts/v1"}}`, "invalid-line"},
		{"bad character", `{"line": {"tag": "ts release/v{version}"}}`, "invalid-line"},
		{"line on an activated scope", `{"activate": true, "line": {}}`, "invalid-line"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := ParseAndValidateScopeConfig([]byte(tc.doc))
			if tc.want == "" {
				if diag.HasErrors(diags) {
					t.Fatalf("%s produced errors: %v", tc.doc, diags)
				}
				return
			}
			if !hasCode(diags, tc.want) {
				t.Fatalf("%s produced %v, want code %q", tc.doc, diags, tc.want)
			}
		})
	}
}

func TestRegistriesEntryPerEcosystem(t *testing.T) {
	cfg, diags := ParseAndValidateWorkspaceConfig([]byte(`{
		"name": "ws",
		"registries": {
			"npm": { "publish": "https://npm.putnami.dev" },
			"go": { "origin": "https://go.putnami.dev" }
		}
	}`))
	if diag.HasErrors(diags) {
		t.Fatalf("valid registries produced errors: %v", diags)
	}
	entry, ok := cfg.RegistryEntry("npm")
	if !ok {
		t.Fatal("RegistryEntry(npm) reported no entry")
	}
	var npm struct {
		Publish string `json:"publish"`
	}
	if err := json.Unmarshal(entry, &npm); err != nil {
		t.Fatalf("registries.npm is not readable as its profile's shape: %v", err)
	}
	if npm.Publish != "https://npm.putnami.dev" {
		t.Errorf("registries.npm.publish = %q", npm.Publish)
	}
	if _, ok := cfg.RegistryEntry("oci"); ok {
		t.Error("RegistryEntry(oci) reported an entry the workspace does not declare")
	}

	// The key is an ecosystem id and the value is an object; what is inside is
	// the profile's business, so an unknown key inside the entry stays valid.
	if _, diags := ParseAndValidateWorkspaceConfig([]byte(`{"name":"ws","registries":{"NPM":{}}}`)); !hasCode(diags, "invalid-registries") {
		t.Errorf("an uppercase ecosystem key produced %v, want invalid-registries", diags)
	}
	if _, diags := ParseAndValidateWorkspaceConfig([]byte(`{"name":"ws","registries":{"npm":"https://npm.putnami.dev"}}`)); !hasCode(diags, "invalid-registries") {
		t.Errorf("a scalar registries entry produced %v, want invalid-registries", diags)
	}

	// A project overrides one entry in its own document, with the same rules.
	proj, diags := ParseAndValidateProjectConfig([]byte(`{
		"name": "@putnami/cli",
		"registries": { "npm": { "publish": "https://npm.example.test" } }
	}`))
	if diag.HasErrors(diags) {
		t.Fatalf("valid project registries produced errors: %v", diags)
	}
	if len(proj.Registries) != 1 {
		t.Fatalf("project registries = %v, want exactly npm", proj.Registries)
	}
	if _, diags := ParseAndValidateProjectConfig([]byte(`{"name":"p","registries":{"o ci":{}}}`)); !hasCode(diags, "invalid-registries") {
		t.Errorf("a bad project registries key produced %v, want invalid-registries", diags)
	}
}

func TestMergeWorkspaceConfigReplacesRegistryEntryWhole(t *testing.T) {
	target := &Config{Registries: map[string]json.RawMessage{
		"npm": json.RawMessage(`{"publish":"https://npm.putnami.dev","scopes":{"@putnami":"https://npm.putnami.dev"}}`),
		"go":  json.RawMessage(`{"origin":"https://go.putnami.dev"}`),
	}}
	MergeWorkspaceConfig(target, &Config{Registries: map[string]json.RawMessage{
		"npm": json.RawMessage(`{"publish":"https://npm.example.test"}`),
	}})

	if got := string(target.Registries["npm"]); got != `{"publish":"https://npm.example.test"}` {
		t.Errorf("npm entry = %s, want the later scope's entry verbatim", got)
	}
	if got := string(target.Registries["go"]); got != `{"origin":"https://go.putnami.dev"}` {
		t.Errorf("go entry = %s, want the earlier scope's entry untouched", got)
	}
}
