package depsedit

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"

	"go.putnami.dev/go/extension/internal/toolchain"
)

// memberModule is a bare add spec that names the module a member of the
// governing go.work declares. Its source is in the tree, so no registry is
// asked for it.
type memberModule struct {
	// Path is the module path the member's go.mod declares, which the spec
	// names.
	Path string
	// Version is the version to require: the one go.mod already requires,
	// else the placeholder of the path's major version (memberVersion).
	Version string
	// Member is the member directory relative to the workspace root, for the
	// messages.
	Member string
	// Replace is the member directory relative to the edited module, or ""
	// when go.mod already replaces the module and that directive stays.
	Replace string
}

// memberModules returns, by spec index, each bare spec of an add in dir that
// names the module path a use directive of dir's governing go.work declares.
//
// A bare spec that names a member resolves from the tree: the edited module
// requires the member at memberVersion and replaces it with the member's
// directory, relative to the module, unless its go.mod already replaces it.
// Every other spec, and any spec that states a version, @latest included, is
// left to the published lookup (resolveModules).
//
// Only the go.mod of each use directive is read. A spec is never matched
// against a project name. Without a go.work no spec is a member. A go.work
// this parser cannot read, a member whose go.mod declares no module path, and
// two members that declare the spec's module fail the add: the precedence of
// a member over the published module cannot be decided then.
func memberModules(workspaceRoot, dir string, specs []string) (map[int]memberModule, error) {
	var bare []int
	for i, spec := range specs {
		if !strings.Contains(spec, "@") {
			bare = append(bare, i)
		}
	}
	if len(bare) == 0 {
		return nil, nil
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	goWorkPath := toolchain.FindGoWork(dir)
	if goWorkPath == "" {
		return nil, nil
	}
	shown := displayPath(workspaceRoot, goWorkPath)
	members, err := workspaceMembers(goWorkPath)
	if err != nil {
		return nil, fmt.Errorf("%s: %w; a spec without a version may name a go.work member, so deps add resolves none", shown, err)
	}

	goModPath := filepath.Join(dir, "go.mod")
	data, err := os.ReadFile(goModPath)
	if err != nil {
		return nil, err
	}
	file, err := modfile.Parse(goModPath, data, nil)
	if err != nil {
		return nil, fmt.Errorf("read go.mod: %w", err)
	}

	resolved := map[int]memberModule{}
	for _, i := range bare {
		path := specs[i]
		dirs := members[path]
		if len(dirs) == 0 {
			continue
		}
		if len(dirs) > 1 {
			names := make([]string, 0, len(dirs))
			for _, memberDir := range dirs {
				names = append(names, displayPath(workspaceRoot, memberDir))
			}
			return nil, fmt.Errorf("%s uses %d directories that declare %s (%s); deps add does not choose one",
				shown, len(dirs), path, strings.Join(names, ", "))
		}
		if file.Module != nil && file.Module.Mod.Path == path {
			return nil, fmt.Errorf("%s is the module deps add edits; a module does not require itself", path)
		}
		version, err := memberVersion(file, path)
		if err != nil {
			return nil, err
		}
		m := memberModule{Path: path, Version: version, Member: displayPath(workspaceRoot, dirs[0])}
		if !replacesVersion(file.Replace, module.Version{Path: path, Version: version}) {
			if m.Replace, err = relativeReplace(dir, dirs[0]); err != nil {
				return nil, fmt.Errorf("replace %s with %s: %w", path, m.Member, err)
			}
		}
		resolved[i] = m
	}
	return resolved, nil
}

// workspaceMembers returns the absolute directory of each use directive of
// the go.work at goWorkPath, by the module path its go.mod declares. A
// directory is relative to go.work. A directory named twice is one member.
func workspaceMembers(goWorkPath string) (map[string][]string, error) {
	data, err := os.ReadFile(goWorkPath)
	if err != nil {
		return nil, err
	}
	work, err := modfile.ParseWork(goWorkPath, data, nil)
	if err != nil {
		return nil, err
	}
	members := map[string][]string{}
	for _, use := range work.Use {
		memberDir := filepath.FromSlash(use.Path)
		if !filepath.IsAbs(memberDir) {
			memberDir = filepath.Join(filepath.Dir(goWorkPath), memberDir)
		}
		memberDir = filepath.Clean(memberDir)
		path := modulePath(memberDir)
		if path == "" {
			return nil, fmt.Errorf("the member %s has no go.mod that declares a module path", use.Path)
		}
		if !slices.Contains(members[path], memberDir) {
			members[path] = append(members[path], memberDir)
		}
	}
	return members, nil
}

// memberVersion is the version the edited module requires a member at: the
// version go.mod requires path at when it is valid for path, else the
// placeholder of path's major version, v0.0.0 or vN.0.0 for a /vN or .vN
// path. The member's replace directive supplies the source, so no registry
// serves either.
func memberVersion(file *modfile.File, path string) (string, error) {
	for _, require := range file.Require {
		if require.Mod.Path == path && module.Check(path, require.Mod.Version) == nil {
			return require.Mod.Version, nil
		}
	}
	_, pathMajor, ok := module.SplitPathVersion(path)
	if !ok {
		return "", fmt.Errorf("%s is not a valid module path", path)
	}
	version := "v0.0.0"
	if major := module.PathMajorPrefix(pathMajor); major != "" {
		version = major + ".0.0"
	}
	if err := module.Check(path, version); err != nil {
		return "", err
	}
	return version, nil
}

// relativeReplace is the target of a replace directive in the module at dir
// that names memberDir: a slash-separated path relative to dir, starting with
// ./ or ../ so go reads it as a directory.
func relativeReplace(dir, memberDir string) (string, error) {
	rel, err := filepath.Rel(dir, memberDir)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if rel != ".." && !strings.HasPrefix(rel, "../") {
		rel = "./" + rel
	}
	return rel, nil
}

// displayPath is path relative to the workspace root, slash-separated, for a
// message, or path itself when no relative path reaches it.
func displayPath(workspaceRoot, path string) string {
	if root, err := filepath.Abs(workspaceRoot); err == nil {
		if rel, err := filepath.Rel(root, path); err == nil {
			return filepath.ToSlash(rel)
		}
	}
	return path
}
