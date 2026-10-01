package toolchain

import (
	"path/filepath"
)

// WorkspaceGoModules reads workspaceRoot/go.work and returns the module paths
// declared by each member's go.mod, in go.work order. Both the `use (...)`
// block form and the single-line `use ./x` form are handled; // comments are
// stripped. A missing or memberless go.work yields nil. go.work and go.mod are
// parsed by hand (see gomod.go) so this extension stays free of a
// golang.org/x/mod workfile dependency and answers identically to the CLI
// parser it replaces.
func WorkspaceGoModules(workspaceRoot string) []string {
	rels, err := ParseGoWorkUses(filepath.Join(workspaceRoot, "go.work"))
	if err != nil {
		return nil
	}
	var modules []string
	for _, rel := range rels {
		goModPath := filepath.Join(workspaceRoot, filepath.FromSlash(rel), "go.mod")
		if mod := GoModModulePath(goModPath); mod != "" {
			modules = append(modules, mod)
		}
	}
	return modules
}

// GoModModulePath returns the module path declared by the go.mod at path, or
// "" when the file is unreadable or declares no module.
func GoModModulePath(path string) string {
	mod, err := ReadGoMod(path)
	if err != nil || mod == nil {
		return ""
	}
	return mod.Module
}
