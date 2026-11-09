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
// coverage files, against the other components of the workspace at root. A
// relative coverage path is resolved against reportBase, the directory the
// task's steps ran in.
func (s Stack) attribution(root, projectAbs, reportBase, component string) Attribution {
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
			// A relative path is the steps' directory's, such as the stack
			// root's for a report a run there writes, unless only the
			// project holds the file.
			rel := abs
			abs = filepath.Join(reportBase, rel)
			if reportBase != projectAbs && !isRegular(abs) && isRegular(filepath.Join(projectAbs, rel)) {
				abs = filepath.Join(projectAbs, rel)
			}
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
			if _, seen := isComponent[dir]; !seen {
				if s.hasMarker(root, dir) {
					candidates = append(candidates, dir)
				} else {
					isComponent[dir] = false
				}
			}
			if dir == "." {
				break
			}
		}
		if len(candidates) > 0 {
			for _, c := range candidates {
				isComponent[c] = false
			}
			if s.IsComponent != nil {
				for _, c := range candidates {
					isComponent[c] = s.IsComponent(root, c)
				}
			} else {
				components, _ := s.Detect(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: candidates})
				for _, c := range components {
					if c.Name != "" {
						isComponent[c.Path] = true
					}
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
	base := filepath.Join(root, filepath.FromSlash(dir))
	for _, m := range s.Profile.Markers {
		candidates := []string{filepath.Join(base, filepath.FromSlash(m))}
		// A marker may name a file one directory down with a wildcard
		// directory: the marker alone is a pattern, never the path above it.
		if strings.ContainsAny(m, "*?[") {
			candidates, _ = filepath.Glob(filepath.Join(globEscape(base), filepath.FromSlash(m)))
		}
		for _, c := range candidates {
			if info, err := os.Stat(c); err == nil && info.Mode().IsRegular() {
				return true
			}
		}
	}
	return false
}

// globEscape escapes the pattern characters of a path, so filepath.Glob reads
// it as itself.
func globEscape(p string) string {
	var b strings.Builder
	for _, r := range p {
		if strings.ContainsRune(`*?[\`, r) && filepath.Separator != '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
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

func isRegular(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}
