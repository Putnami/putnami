package stacks

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	readers "go.putnami.dev/evidence/readers"
	wsproto "go.putnami.dev/protocol/workspace"
)

// attribution is the Attribution of one task: the component's own cases and
// coverage files, against the other components of the workspace at root.
func (s Stack) attribution(root, projectAbs, component string) Attribution {
	var a Attribution
	if s.CaseComponent != nil {
		key := s.componentKey(component)
		a.Case = func(c readers.TestCase) Owner {
			suite := ""
			if len(c.Suites) > 0 {
				suite = c.Suites[len(c.Suites)-1].Name
			}
			name, ok := s.CaseComponent(c.Classname, suite)
			switch {
			case !ok:
				return OwnerNone
			case s.componentKey(name) == key:
				return OwnerTask
			default:
				return OwnerOther
			}
		}
	}
	owners := s.fileOwners(root)
	project, _ := relSlash(root, projectAbs)
	a.File = func(p string) Owner {
		abs := filepath.FromSlash(p)
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(projectAbs, abs)
		}
		dir, ok := owners(abs)
		switch {
		case !ok:
			return OwnerNone
		case dir == project:
			return OwnerTask
		default:
			return OwnerOther
		}
	}
	return a
}

func (s Stack) componentKey(name string) string {
	if s.ComponentKey == nil {
		return name
	}
	return s.ComponentKey(name)
}

// fileOwners returns the function that names the workspace-relative
// directory of the component that holds an absolute file: the deepest one,
// found by the stack's detector over the file's directories that hold a
// marker. A file outside the workspace, or in no component, has none. A
// stack root that holds no component of its own owns no file.
func (s Stack) fileOwners(root string) func(abs string) (string, bool) {
	roots := []string{root}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root {
		roots = append(roots, resolved)
	}
	isComponent := map[string]bool{}
	return func(abs string) (string, bool) {
		var rel string
		ok := false
		for _, r := range roots {
			if rel, ok = relSlash(r, filepath.Clean(abs)); ok {
				break
			}
		}
		if !ok {
			return "", false
		}
		var candidates []string
		for dir := path.Dir(rel); ; dir = path.Dir(dir) {
			if _, seen := isComponent[dir]; !seen && s.hasMarker(root, dir) {
				candidates = append(candidates, dir)
			}
			if dir == "." {
				break
			}
		}
		if len(candidates) > 0 {
			for _, c := range candidates {
				isComponent[c] = false
			}
			components, _ := s.Detect(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: candidates})
			for _, c := range components {
				if c.Name != "" {
					isComponent[c.Path] = true
				}
			}
		}
		for dir := path.Dir(rel); ; dir = path.Dir(dir) {
			if isComponent[dir] {
				return dir, true
			}
			if dir == "." {
				return "", false
			}
		}
	}
}

func (s Stack) hasMarker(root, dir string) bool {
	for _, m := range s.Profile.Markers {
		if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir), m)); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// relSlash is target relative to base in slash form; ok is false when target
// lies outside base.
func relSlash(base, target string) (string, bool) {
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}
