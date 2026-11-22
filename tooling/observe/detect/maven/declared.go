package maven

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// maxReferenceDepth bounds how many times a declared version's references
// are replaced, a property may name another, and how many parents a chain
// of the repository's POMs climbs.
const maxReferenceDepth = 8

// ReadDeclaredVersions reads the versions one POM declares, Maven having no
// lockfile, without the rest of its build: each dependency of dependencies
// and dependencyManagement, in the project and in its profiles, and the
// parent, as groupId:artifactId at the version it states.
// ${project.version}, ${project.parent.version}, the groupId references and
// the POM's own properties, its profiles' included, are resolved; the
// project's version and groupId default to its parent's. A dependency
// without a version, or whose name or version keeps a reference that does
// not resolve, is no package, and neither is one that module names, the
// repository's own modules; module may be nil. These are the versions the
// POM declares, not the ones Maven resolves.
func ReadDeclaredVersions(data []byte, module func(coordinates string) bool) (map[string][]string, error) {
	p, err := parsePOM(POM, data)
	if err != nil {
		return nil, err
	}
	out := declared{module: module, packages: map[string][]string{}}
	out.own(p, nil)
	return out.packages, nil
}

// IsDeclaredVersions reports whether a repository path is a POM whose
// declared versions ReadDeclared reads: a POM of the build, or a pom.xml
// the head checkout no longer holds, such as a module the change deletes. A
// pom.xml on disk outside the build, such as an archetype template or an
// integration test project, is not read.
func (d Detection) IsDeclaredVersions(file string) bool {
	if len(d.POMs) == 0 {
		return false
	}
	if d.IsPOM(file) {
		return true
	}
	if path.Base(file) != POM {
		return false
	}
	_, err := os.Lstat(filepath.Join(d.root, filepath.FromSlash(file)))
	return errors.Is(err, fs.ErrNotExist)
}

// ReadDeclared reads the versions a POM of the build declares, with data
// its content at one side of a change, as ReadDeclaredVersions does, and
// with the build's other POMs, as the detector read them: a property a
// parent POM of the repository defines resolves the POM's own dependencies,
// and a dependency of a module that inherits from it, directly or through
// other POMs of the repository, whose version only resolves through the
// properties of this POM or of its parents is one of its packages too. A
// version property bumped in a parent POM is so a change of that POM, while
// a module's own version stays the module's. The modules of the build are
// no package.
func (d Detection) ReadDeclared(file string, data []byte) (map[string][]string, error) {
	p, err := parsePOM(file, data)
	if err != nil {
		return nil, err
	}
	out := declared{module: d.IsModule, packages: map[string][]string{}}
	ancestors := d.ancestors(p)
	out.own(p, ancestors)
	files := make([]string, 0, len(d.poms))
	for f := range d.poms {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		if f == file {
			continue
		}
		child := d.poms[f]
		chain := d.ancestors(child)
		at := -1
		for i, a := range chain {
			if a.file == file {
				at = i
				break
			}
		}
		if at < 0 {
			continue
		}
		below := chain[:at]
		inherited := append(append(append([]pom(nil), below...), p), ancestors...)
		for _, dep := range child.declaredDependencies() {
			version := resolveDeclared(dep.Version, child, below)
			if !strings.Contains(version, "${") {
				continue
			}
			out.add(child, inherited, dep.GroupID, dep.ArtifactID, dep.Version)
		}
	}
	return out.packages, nil
}

// ancestors returns the POMs of the build p inherits from, its parent first,
// found by their groupId:artifactId.
func (d Detection) ancestors(p pom) []pom {
	var out []pom
	seen := map[string]bool{p.file: true}
	for current := p; len(out) < maxReferenceDepth; {
		parent := current.project.Parent
		coordinates := strings.TrimSpace(parent.GroupID) + ":" + strings.TrimSpace(parent.ArtifactID)
		file, ok := d.byCoordinates[coordinates]
		if !ok || seen[file] {
			break
		}
		seen[file] = true
		current = d.poms[file]
		out = append(out, current)
	}
	return out
}

// declared gathers the packages of a reading.
type declared struct {
	module   func(string) bool
	packages map[string][]string
}

// own adds the parent and the dependencies of p, its references resolved
// through p and then through inherited.
func (o declared) own(p pom, inherited []pom) {
	parent := p.project.Parent
	o.add(p, inherited, parent.GroupID, parent.ArtifactID, parent.Version)
	for _, dep := range p.declaredDependencies() {
		o.add(p, inherited, dep.GroupID, dep.ArtifactID, dep.Version)
	}
}

func (o declared) add(p pom, inherited []pom, groupID, artifactID, version string) {
	name := resolveDeclared(groupID, p, inherited) + ":" + resolveDeclared(artifactID, p, inherited)
	version = resolveDeclared(version, p, inherited)
	if version == "" || strings.HasPrefix(name, ":") || strings.HasSuffix(name, ":") ||
		strings.Contains(name, "${") || strings.Contains(version, "${") {
		return
	}
	if o.module != nil && o.module(name) {
		return
	}
	for _, v := range o.packages[name] {
		if v == version {
			return
		}
	}
	o.packages[name] = append(o.packages[name], version)
}

// declaredDependencies are the dependencies of dependencies and
// dependencyManagement, in the project and in its profiles.
func (p pom) declaredDependencies() []dependency {
	deps := append(append([]dependency(nil), p.project.Dependencies...), p.project.Managed...)
	for _, profile := range p.project.Profiles {
		deps = append(append(deps, profile.Dependencies...), profile.Managed...)
	}
	return deps
}

// version is the project's version, else its parent's.
func (p pom) version() string {
	if v := strings.TrimSpace(p.project.Version); v != "" {
		return v
	}
	return strings.TrimSpace(p.project.Parent.Version)
}

// property returns the value of a property of p: its own, else the first
// profile's that defines it. Which profile is active is not known, so a
// project property wins over a profile's of the same name.
func (p pom) property(name string) (string, bool) {
	for _, entry := range p.project.Properties.Entries {
		if entry.XMLName.Local == name {
			return strings.TrimSpace(entry.Value), true
		}
	}
	for _, profile := range p.project.Profiles {
		for _, entry := range profile.Properties.Entries {
			if entry.XMLName.Local == name {
				return strings.TrimSpace(entry.Value), true
			}
		}
	}
	return "", false
}

// resolveDeclared replaces the references of a value p declares: those
// resolve replaces, the project's and its parent's version, a property of p
// and then of each inherited POM in order, and a property that names
// another, up to maxReferenceDepth times.
func resolveDeclared(value string, p pom, inherited []pom) string {
	value = strings.TrimSpace(value)
	for range maxReferenceDepth {
		if !strings.Contains(value, "${") {
			return value
		}
		next := property.ReplaceAllStringFunc(value, func(ref string) string {
			name := ref[2 : len(ref)-1]
			switch name {
			case "project.version", "pom.version", "version":
				if v := p.version(); v != "" {
					return v
				}
				return ref
			case "project.parent.version", "parent.version":
				if v := strings.TrimSpace(p.project.Parent.Version); v != "" {
					return v
				}
				return ref
			}
			if resolved := p.resolve(ref); resolved != ref {
				return resolved
			}
			if v, ok := p.property(name); ok {
				return v
			}
			for _, a := range inherited {
				if v, ok := a.property(name); ok {
					return v
				}
			}
			return ref
		})
		if next == value {
			return value
		}
		value = next
	}
	return value
}
