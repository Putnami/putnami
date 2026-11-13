package stacks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
)

// NamedByTypeScript reports whether the TypeScript extension names the
// project at dir of the workspace at root: the workspace declares
// TypeScriptExtension and dir holds a package.json that states a name. A
// monorepo tool then reports no source name for the project and keeps its own
// name as the component its steps receive, so the two providers never
// disagree on the name (decision language-name-wins).
func NamedByTypeScript(root, dir string) bool {
	return packageName(root, dir) != "" && DeclaresExtension(root, TypeScriptExtension)
}

// DeclaresExtension reports whether the workspace at root declares the
// extension: by name, or by a path whose putnami.extension.json states that
// name.
func DeclaresExtension(root, name string) bool {
	data, err := os.ReadFile(filepath.Join(root, wsproto.WorkspaceConfigFilename)) //nolint:gosec // the workspace file
	if err != nil {
		return false
	}
	var doc struct {
		Extensions json.RawMessage `json:"extensions"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return false
	}
	for _, e := range extensionNames(doc.Extensions) {
		if e == name {
			return true
		}
		if !strings.HasPrefix(e, "/") && !strings.HasPrefix(e, ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(e, "/")), "putnami.extension.json")) //nolint:gosec // a manifest the workspace declares
		if err != nil {
			continue
		}
		var manifest struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &manifest) == nil && manifest.Name == name {
			return true
		}
	}
	return false
}

// extensionNames reads the extensions of a workspace document: a list of
// names, or an object of name to version.
func extensionNames(raw json.RawMessage) []string {
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var versions map[string]any
	if json.Unmarshal(raw, &versions) != nil {
		return nil
	}
	names := make([]string, 0, len(versions))
	for name := range versions {
		names = append(names, name)
	}
	return names
}

// packageName is the name the package.json at dir of root states; empty when
// there is none.
func packageName(root, dir string) string {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), "package.json")) //nolint:gosec // a manifest under the workspace root
	if err != nil {
		return ""
	}
	var manifest struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(data, &manifest) != nil {
		return ""
	}
	return strings.TrimSpace(manifest.Name)
}
