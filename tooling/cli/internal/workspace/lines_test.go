package workspace

import (
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

// Version lines and registries as the loader resolves them.
//
// A line is a scope that declares a `line` block; the version of every project
// under it comes from that line's git tags. Registries are the workspace's
// single source for every registry endpoint an extension needs. Both are
// authored facts, so the loader settles them before any job is planned.

func TestNearestAncestorLineWins(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "version-lines-are-scopes", "nearest-ancestor-line-wins")
	root := t.TempDir()
	writeFile(t, filepath.Join(root, wsproto.WorkspaceConfigFilename),
		`{"name":"ws","includes":["typescript","standalone"]}`)
	writeFile(t, filepath.Join(root, "typescript", wsproto.ConfigFilename),
		`{"line":{"tag":"ts/v{version}"},"includes":["framework/web"]}`)
	writeFile(t, filepath.Join(root, "typescript", "framework", wsproto.ConfigFilename),
		`{"tags":["framework"]}`)
	writeFile(t, filepath.Join(root, "typescript", "framework", "web", wsproto.ConfigFilename),
		`{"name":"@putnami/web"}`)
	writeFile(t, filepath.Join(root, "standalone", wsproto.ConfigFilename),
		`{"name":"standalone"}`)

	InvalidateLoadCache(root)
	ws, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	web := ws.ProjectByPath("typescript/framework/web")
	if web == nil {
		t.Fatal("typescript/framework/web was not discovered")
	}
	// The intermediate scope declares no line, so the project belongs to the
	// nearest ancestor that does — not to the scope directly above it.
	if web.Line != "typescript" {
		t.Errorf("line of @putnami/web = %q, want typescript", web.Line)
	}

	standalone := ws.ProjectByPath("standalone")
	if standalone == nil {
		t.Fatal("standalone was not discovered")
	}
	if standalone.Line != "" {
		t.Errorf("line of standalone = %q, want the implicit root line", standalone.Line)
	}

	if got := ws.Lines["typescript"]; got != "ts/v{version}" {
		t.Errorf("workspace lines = %v, want typescript -> ts/v{version}", ws.Lines)
	}
	if _, ok := ws.Lines[""]; ok {
		t.Errorf("workspace lines = %v, want no implicit root entry once a scope declares a line", ws.Lines)
	}
}

func TestWorkspaceWithoutLineBlockIsOneLine(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "version-lines-are-scopes", "nearest-ancestor-line-wins")
	root := t.TempDir()
	writeFile(t, filepath.Join(root, wsproto.WorkspaceConfigFilename), `{"name":"ws","includes":["app"]}`)
	writeFile(t, filepath.Join(root, "app", wsproto.ConfigFilename), `{"name":"app"}`)

	InvalidateLoadCache(root)
	ws, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(ws.Lines) != 1 || ws.Lines[""] != "v{version}" {
		t.Fatalf("lines = %v, want the single implicit root line", ws.Lines)
	}
	if app := ws.ProjectByPath("app"); app == nil || app.Line != "" {
		t.Fatalf("app line = %+v, want the implicit root line", app)
	}
}

func TestNestedLineIsRefused(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "version-lines-are-scopes", "nested-line-is-refused")
	root := t.TempDir()
	writeFile(t, filepath.Join(root, wsproto.WorkspaceConfigFilename), `{"name":"ws","includes":["go"]}`)
	writeFile(t, filepath.Join(root, "go", wsproto.ConfigFilename),
		`{"line":{},"includes":["framework/http"]}`)
	writeFile(t, filepath.Join(root, "go", "framework", wsproto.ConfigFilename), `{"line":{}}`)
	writeFile(t, filepath.Join(root, "go", "framework", "http", wsproto.ConfigFilename), `{"name":"http"}`)

	InvalidateLoadCache(root)
	if _, err := Load(root); err == nil {
		t.Fatal("Load accepted a version line declared under another version line")
	}
}

func TestProjectRegistriesOverrideReplacesTheEntry(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "registries-keyed-by-ecosystem", "project-override-replaces-the-entry")
	root := t.TempDir()
	writeFile(t, filepath.Join(root, wsproto.WorkspaceConfigFilename), `{
		"name": "ws",
		"includes": ["app", "lib"],
		"registries": {
			"npm": { "publish": "https://npm.putnami.dev", "scopes": { "@putnami": "https://npm.putnami.dev" } },
			"oci": { "publish": "oci.putnami.dev/putnami" }
		}
	}`)
	writeFile(t, filepath.Join(root, "app", wsproto.ConfigFilename), `{
		"name": "app",
		"registries": { "npm": { "publish": "https://npm.example.test" } }
	}`)
	writeFile(t, filepath.Join(root, "lib", wsproto.ConfigFilename), `{"name":"lib"}`)

	InvalidateLoadCache(root)
	ws, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := string(ws.Registries["oci"]); got != `{ "publish": "oci.putnami.dev/putnami" }` {
		t.Errorf("workspace oci entry = %s", got)
	}

	app := ws.ProjectByPath("app")
	if app == nil {
		t.Fatal("app was not discovered")
	}
	// The whole entry is replaced: the workspace's `scopes` key does not
	// survive into the project's npm entry, because the shape of an entry
	// belongs to the ecosystem profile, not to a merge rule here.
	if got := string(app.Registries["npm"]); got != `{ "publish": "https://npm.example.test" }` {
		t.Errorf("app npm entry = %s, want the project's entry verbatim", got)
	}
	// Ecosystems the project says nothing about still come from the workspace.
	if got := string(app.Registries["oci"]); got != `{ "publish": "oci.putnami.dev/putnami" }` {
		t.Errorf("app oci entry = %s, want the workspace entry", got)
	}

	lib := ws.ProjectByPath("lib")
	if lib == nil {
		t.Fatal("lib was not discovered")
	}
	if got := string(lib.Registries["npm"]); got != `{ "publish": "https://npm.putnami.dev", "scopes": { "@putnami": "https://npm.putnami.dev" } }` {
		t.Errorf("lib npm entry = %s, want the workspace entry untouched", got)
	}
}
