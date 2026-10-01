package workspace

import (
	"fmt"
	"path/filepath"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/jsonutil"
)

// The only project-manifest write core has left.
//
// A prior change deleted the three-way name writer over package.json / go.mod /
// pyproject.toml: each provider now aligns its own manifest from its own
// `workspace-sync` task. What core still writes is its OWN file — putnami.json —
// because that is where a user's explicitly authored, language-neutral project
// configuration lives, and it is the file whose values outrank every provider in
// the merge.
//
// Writing tags into a package.json `putnami` section, as `projects tag` used to,
// is exactly the layering this epic removes: it made the answer depend on which
// language the project happened to be written in, and it put core's authored
// values inside a manifest another writer owns.

// UpdateProjectConfigField sets (or, with a nil value, removes) a top-level
// field in a project's putnami.json, creating the file when the project has
// none.
//
// Key order is preserved and unrelated members are left byte-identical
// (jsonutil), so a `projects tag` never reformats a file the user maintains.
func UpdateProjectConfigField(wsRoot string, proj *Project, field string, value any) error {
	if proj == nil {
		return fmt.Errorf("no project to update")
	}
	projectDir := filepath.Join(wsRoot, proj.Path)
	if !fileExists(projectDir) {
		return fmt.Errorf("project directory %s does not exist", proj.Path)
	}

	path := wsproto.ResolveFile(projectDir, wsproto.ConfigFilename)
	raw := jsonutil.New()
	if fileExists(path) {
		existing, err := jsonutil.ReadFile(path)
		if err != nil {
			return err
		}
		raw = existing
	}

	if value == nil {
		raw.Delete(field)
	} else {
		raw.Set(field, value)
	}
	return jsonutil.WriteFile(path, raw)
}
