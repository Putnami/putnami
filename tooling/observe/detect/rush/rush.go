// Package rush detects the projects of a Rush monorepo, the edges between
// them and the lockfiles Rush keeps under common/config. It reads rush.json
// and the projects' package.json files, and runs neither rush nor a package
// manager.
package rush

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/observe/detect/nodeworkspace"
	"go.putnami.dev/tooling/observe/internal/lockdiff"
)

// Config is the file at the repository root that makes it a Rush monorepo.
const Config = "rush.json"

// Ecosystem is the component ecosystem of a Rush project.
const Ecosystem = nodeworkspace.Ecosystem

// Lockfiles of each package manager, relative to the repository root.
const (
	PNPMLock      = "common/config/rush/pnpm-lock.yaml"
	NPMShrinkwrap = "common/config/rush/npm-shrinkwrap.json"
	YarnLock      = "common/config/rush/yarn.lock"
)

// TempScope is the scope of the temporary packages Rush writes for its own
// projects into a lockfile.
const TempScope = "@rush-temp/"

// Diagnostic codes.
const (
	CodeConfigInvalid   = "observe.rush_config_invalid"
	CodeManifestInvalid = "observe.rush_manifest_invalid"
)

// subspaceLock matches the pnpm lockfile of one subspace.
var subspaceLock = regexp.MustCompile(`^common/config/subspaces/[^/]+/pnpm-lock\.yaml$`)

// Project is one Rush project.
type Project struct {
	// Folder is the project's directory relative to the repository root.
	Folder string
	// Name is its package name.
	Name string
	// Version is the version of its package.json, empty when it has none.
	Version string
	// DependsOn lists the folders of the Rush projects it depends on, sorted.
	DependsOn []string
}

// Detection is what the Rush files of a repository declare.
type Detection struct {
	// Manager is pnpm, npm or yarn; empty when rush.json names none.
	Manager string
	// Lockfile is the repository-relative lockfile of the manager, empty
	// without a manager.
	Lockfile string
	// Projects are sorted by folder.
	Projects    []Project
	Diagnostics []diag.Diagnostic
}

// config is the subset of rush.json the detector reads.
type config struct {
	PNPMVersion string          `json:"pnpmVersion"`
	NPMVersion  string          `json:"npmVersion"`
	YarnVersion string          `json:"yarnVersion"`
	Projects    []configProject `json:"projects"`
}

type configProject struct {
	PackageName                string   `json:"packageName"`
	ProjectFolder              string   `json:"projectFolder"`
	DecoupledLocalDependencies []string `json:"decoupledLocalDependencies"`
	CyclicDependencyProjects   []string `json:"cyclicDependencyProjects"`
}

// Detect reads the Rush monorepo rooted at root. A repository without
// rush.json gives an empty detection; a rush.json that cannot be read or
// parsed gives a warning and no project.
func Detect(root string) Detection {
	data, err := os.ReadFile(filepath.Join(root, Config))
	if errors.Is(err, fs.ErrNotExist) {
		return Detection{}
	}
	if err != nil {
		return Detection{Diagnostics: []diag.Diagnostic{diag.Warningf(CodeConfigInvalid, Config, "rush.json cannot be read: %v", err)}}
	}
	var cfg config
	if err := json.Unmarshal(StripComments(data), &cfg); err != nil {
		return Detection{Diagnostics: []diag.Diagnostic{diag.Warningf(CodeConfigInvalid, Config, "rush.json cannot be parsed: %v", err)}}
	}

	var d Detection
	switch {
	case cfg.PNPMVersion != "":
		d.Manager, d.Lockfile = nodeworkspace.ManagerPNPM, PNPMLock
	case cfg.NPMVersion != "":
		d.Manager, d.Lockfile = nodeworkspace.ManagerNPM, NPMShrinkwrap
	case cfg.YarnVersion != "":
		d.Manager, d.Lockfile = nodeworkspace.ManagerYarn, YarnLock
	}

	type entry struct {
		Project
		decoupled map[string]bool
		manifest  *nodeworkspace.Manifest
	}
	var entries []*entry
	byName := map[string]*entry{}
	byFolder := map[string]bool{}
	for i, p := range cfg.Projects {
		at := Config + "#projects[" + strconv.Itoa(i) + "]"
		name := strings.TrimSpace(p.PackageName)
		folder, ok := wsproto.NormalizeProbePath(p.ProjectFolder)
		switch {
		case name == "":
			d.Diagnostics = append(d.Diagnostics, diag.Warningf(CodeConfigInvalid, at, "the project has no packageName and is left out"))
			continue
		case !ok || folder == wsproto.ProbeRootPath:
			d.Diagnostics = append(d.Diagnostics, diag.Warningf(CodeConfigInvalid, at, "the projectFolder %q of %s is not a directory inside the repository and the project is left out", p.ProjectFolder, name))
			continue
		case byName[name] != nil:
			d.Diagnostics = append(d.Diagnostics, diag.Warningf(CodeConfigInvalid, at, "%s is listed twice; the project in %s keeps it", name, byName[name].Folder))
			continue
		case byFolder[folder]:
			d.Diagnostics = append(d.Diagnostics, diag.Warningf(CodeConfigInvalid, at, "the folder %s of %s belongs to an earlier project and %s is left out", folder, name, name))
			continue
		}
		e := &entry{Project: Project{Folder: folder, Name: name}, decoupled: map[string]bool{}}
		for _, dep := range append(append([]string(nil), p.DecoupledLocalDependencies...), p.CyclicDependencyProjects...) {
			e.decoupled[dep] = true
		}
		manifest, err := nodeworkspace.ReadManifest(root, folder)
		switch {
		case err != nil:
			d.Diagnostics = append(d.Diagnostics, diag.Warningf(CodeManifestInvalid, path.Join(folder, nodeworkspace.PackageJSON),
				"the package.json of %s cannot be read; the project has no edges: %v", name, err))
		case manifest == nil:
			d.Diagnostics = append(d.Diagnostics, diag.Warningf(CodeManifestInvalid, path.Join(folder, nodeworkspace.PackageJSON),
				"%s has no package.json; the project has no edges", name))
		default:
			e.manifest = manifest
			e.Version = manifest.Version
		}
		entries = append(entries, e)
		byName[name] = e
		byFolder[folder] = true
	}

	for _, e := range entries {
		if e.manifest != nil {
			set := map[string]bool{}
			for _, deps := range []map[string]string{e.manifest.Dependencies, e.manifest.DevDependencies, e.manifest.OptionalDependencies, e.manifest.PeerDependencies} {
				for name, spec := range deps {
					target := edge(name, spec, e.decoupled, func(name string) (string, string, bool) {
						t, ok := byName[name]
						if !ok {
							return "", "", false
						}
						return t.Folder, t.Version, true
					})
					if target != "" && target != e.Folder {
						set[target] = true
					}
				}
			}
			for target := range set {
				e.DependsOn = append(e.DependsOn, target)
			}
			sort.Strings(e.DependsOn)
		}
		d.Projects = append(d.Projects, e.Project)
	}
	sort.Slice(d.Projects, func(i, j int) bool { return d.Projects[i].Folder < d.Projects[j].Folder })
	return d
}

// edge returns the folder of the Rush project a dependency links, or "" for
// an external one or one whose key the project decouples. A workspace: range
// links the project it names, an alias included; another range links it when
// the project's version satisfies it. An npm: alias is never linked.
func edge(name, spec string, decoupled map[string]bool, project func(string) (folder, version string, ok bool)) string {
	if decoupled[name] {
		return ""
	}
	spec = strings.TrimSpace(spec)
	rest, workspace := strings.CutPrefix(spec, "workspace:")
	if workspace {
		if at := strings.LastIndex(rest, "@"); at > 0 {
			name = rest[:at]
		}
	}
	folder, version, ok := project(name)
	if !ok {
		return ""
	}
	if workspace || nodeworkspace.Satisfies(version, spec) {
		return folder
	}
	return ""
}

// LockReader returns the reader of a lockfile Rush keeps: the lockfile of
// the manager, and in pnpm mode each subspace's pnpm-lock.yaml. It reads with
// the generic reader of that lockfile and leaves out Rush's temporary
// packages.
func (d Detection) LockReader(file string) (lockdiff.Reader, bool) {
	var read lockdiff.Reader
	switch {
	case d.Lockfile == "":
		return nil, false
	case file == d.Lockfile && d.Manager == nodeworkspace.ManagerNPM:
		read = lockdiff.ReadPackageLock
	case file == d.Lockfile && d.Manager == nodeworkspace.ManagerYarn:
		read = lockdiff.ReadYarnLock
	case d.Manager == nodeworkspace.ManagerPNPM && (file == d.Lockfile || subspaceLock.MatchString(file)):
		read = lockdiff.ReadPNPMLock
	default:
		return nil, false
	}
	return func(data []byte) (lockdiff.Packages, error) {
		packages, err := read(data)
		if err != nil {
			return nil, err
		}
		for name := range packages {
			if strings.HasPrefix(name, TempScope) {
				delete(packages, name)
			}
		}
		return packages, nil
	}, true
}

// StripComments returns JSON with comments as plain JSON: it blanks the line
// and block comments outside strings and drops a comma before a closing
// bracket or brace. Line breaks are kept, so a parse error keeps its offset
// line.
func StripComments(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch {
		case c == '"':
			start := i
			for i++; i < len(data) && data[i] != '"'; i++ {
				if data[i] == '\\' {
					i++
				}
			}
			end := min(i+1, len(data))
			out = append(out, data[start:end]...)
		case c == '/' && i+1 < len(data) && data[i+1] == '/':
			for i < len(data) && data[i] != '\n' {
				out = append(out, ' ')
				i++
			}
			if i < len(data) {
				out = append(out, '\n')
			}
		case c == '/' && i+1 < len(data) && data[i+1] == '*':
			out = append(out, ' ', ' ')
			for i += 2; i < len(data) && !(data[i] == '*' && i+1 < len(data) && data[i+1] == '/'); i++ {
				if data[i] == '\n' {
					out = append(out, '\n')
				} else {
					out = append(out, ' ')
				}
			}
			if i < len(data) {
				out = append(out, ' ', ' ')
				i++
			}
		case c == ']' || c == '}':
			if j := lastSignificant(out); j >= 0 && out[j] == ',' {
				out[j] = ' '
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}

// lastSignificant returns the index of the last byte that is not white
// space, or -1.
func lastSignificant(b []byte) int {
	for j := len(b) - 1; j >= 0; j-- {
		switch b[j] {
		case ' ', '\t', '\n', '\r':
			continue
		}
		return j
	}
	return -1
}
