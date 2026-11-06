package swift

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// MaxLocatedFiles bounds the files one test target search visits.
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
}

type located struct {
	file, component string
	ok              bool
}

func newLocator(root string, d Detection) *locator {
	l := &locator{root: root, found: map[string]located{}}
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

// classFile finds the Swift file of class under the repository directory
// dir: Class.swift, else the first file, in walk order, that declares it.
func (l *locator) classFile(dir, class string) (string, bool) {
	declares := regexp.MustCompile(`(?m)^\s*(?:@\w+(?:\([^)]*\))?\s+)*(?:(?:public|internal|private|fileprivate|open|final)\s+)*(?:class|struct|actor|enum|extension)\s+` + class + `\b`)
	var named, declaring string
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
		if !strings.HasSuffix(entry.Name(), ".swift") {
			return nil
		}
		rel, ok := relative(l.root, p)
		if !ok {
			return nil
		}
		if entry.Name() == class+".swift" {
			named = rel
			return fs.SkipAll
		}
		if declaring == "" {
			if data, err := os.ReadFile(p); err == nil && declares.Match(data) {
				declaring = rel
			}
		}
		return nil
	})
	if named != "" {
		return named, true
	}
	return declaring, declaring != ""
}

func isDir(root, dir string) bool {
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir)))
	return err == nil && info.IsDir()
}
