package swift

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// MaxLocatedFiles bounds the entries the index of one test target
// directory visits.
const MaxLocatedFiles = 20000

// testTarget is one test target a class name may name.
type testTarget struct {
	// module is the target's module name: its name, each character that is
	// no identifier character replaced by _.
	module    string
	dir       string
	component string
}

type locator struct {
	root    string
	targets []testTarget
	mu      sync.Mutex
	found   map[string]located
	indexes map[string]index
}

type located struct {
	file, component string
	ok              bool
}

func newLocator(root string, d Detection) *locator {
	l := &locator{root: root, found: map[string]located{}, indexes: map[string]index{}}
	for _, p := range d.Packages {
		for _, t := range p.TestTargets() {
			l.targets = append(l.targets, testTarget{module: moduleName(t.Name), dir: t.Dir, component: t.Dir})
		}
	}
	for _, p := range d.Projects {
		for _, name := range p.TestTargets {
			dir := p.Dir
			if candidate, ok := join(p.Dir, name); ok && isDir(root, candidate) {
				dir = candidate
			}
			l.targets = append(l.targets, testTarget{module: moduleName(name), dir: dir, component: p.Dir})
		}
	}
	return l
}

var nonIdentifier = regexp.MustCompile(`[^A-Za-z0-9_]`)

// moduleName is the module a target builds: its name, as Swift spells it in
// a qualified type name.
func moduleName(target string) string {
	return nonIdentifier.ReplaceAllString(target, "_")
}

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// split reads Module.Class, Module.Outer.Inner or Class.
func split(name string) (module, class string, ok bool) {
	parts := strings.Split(strings.TrimSpace(name), ".")
	for _, part := range parts {
		if !identifier.MatchString(part) {
			return "", "", false
		}
	}
	switch len(parts) {
	case 1:
		return "", parts[0], true
	default:
		return parts[0], parts[1], true
	}
}

func (l *locator) locate(name string) (string, string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if hit, seen := l.found[name]; seen {
		return hit.file, hit.component, hit.ok
	}
	hit := l.search(name)
	l.found[name] = hit
	return hit.file, hit.component, hit.ok
}

func (l *locator) search(name string) located {
	module, class, ok := split(name)
	if !ok {
		return located{}
	}
	var named []testTarget
	for _, t := range l.targets {
		if module == "" || t.module == module {
			named = append(named, t)
		}
	}
	for _, t := range named {
		if file, ok := l.classFile(t.dir, class); ok {
			return located{file: file, component: t.component, ok: true}
		}
	}
	// A module named outright places the case on its target even when no
	// file there is named after the class or declares it.
	if module != "" && len(named) > 0 {
		return located{file: named[0].dir, component: named[0].component, ok: true}
	}
	return located{}
}

// declaration matches the type declarations and extensions of a Swift file,
// one per line, attributes and modifiers allowed before the keyword.
var declaration = regexp.MustCompile(`(?m)^\s*(?:@\w+(?:\([^)]*\))?\s+)*(?:(?:public|internal|private|fileprivate|open|final|package)\s+)*(?:class|struct|actor|enum|extension)\s+([A-Za-z_][A-Za-z0-9_]*)`)

// index is the Swift files of one search directory: each file by its base
// name, and the first file, in walk order, that declares each type.
type index struct {
	named    map[string]string
	declared map[string]string
}

// classFile finds the Swift file of class under the repository directory
// dir: Class.swift, else the first file, in walk order, that declares it.
// The directory is walked and each of its files read once, on the first
// lookup.
func (l *locator) classFile(dir, class string) (string, bool) {
	idx, ok := l.indexes[dir]
	if !ok {
		idx = l.buildIndex(dir)
		l.indexes[dir] = idx
	}
	if file, ok := idx.named[class]; ok {
		return file, true
	}
	file, ok := idx.declared[class]
	return file, ok
}

func (l *locator) buildIndex(dir string) index {
	idx := index{named: map[string]string{}, declared: map[string]string{}}
	visited := 0
	abs := filepath.Join(l.root, filepath.FromSlash(dir))
	_ = filepath.WalkDir(abs, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		visited++
		if visited > MaxLocatedFiles {
			return fs.SkipAll
		}
		if entry.IsDir() {
			if p != abs && (strings.HasPrefix(entry.Name(), ".") || skippedDirs[entry.Name()]) {
				return fs.SkipDir
			}
			return nil
		}
		base, isSwift := strings.CutSuffix(entry.Name(), ".swift")
		if !isSwift || !entry.Type().IsRegular() {
			return nil
		}
		rel, ok := relative(l.root, p)
		if !ok {
			return nil
		}
		if _, seen := idx.named[base]; !seen {
			idx.named[base] = rel
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		for _, m := range declaration.FindAllSubmatch(data, -1) {
			if _, seen := idx.declared[string(m[1])]; !seen {
				idx.declared[string(m[1])] = rel
			}
		}
		return nil
	})
	return idx
}

func isDir(root, dir string) bool {
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir)))
	return err == nil && info.IsDir()
}
