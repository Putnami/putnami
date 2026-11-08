// Package composer is the Composer detector of @putnami/observe: it finds the
// components of a PHP repository, such as a Symfony application, from its
// composer.json manifests, without running Composer.
//
// It answers over the workspace probe wire (go.putnami.dev/protocol/workspace):
// Probe has the shape of a probe answer, and its result passes the probe
// protocol's strict validation, so the detector can move to its own extension
// once the CLI can run a probe outside a workspace.
// Outside a workspace no core lists the candidate directories, so Candidates
// walks the repository for them.
//
// One composer.json is one component. A path repository makes a required
// package an edge to its directory. The directories directly under a
// component's PSR-4 roots are its areas. Each component's metadata names what
// the change analyses read: its lockfile, its source and test roots, and the
// report outputs of its PHPUnit configuration.
package composer

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
)

// ExtensionName is the extension the detector answers under; it keys the
// provider-owned metadata of each project.
const ExtensionName = "@putnami/observe"

// Ecosystem is the evidence ecosystem slug of a Composer component.
const Ecosystem = "composer"

// Files the detector reads.
const (
	Marker   = "composer.json"
	Lockfile = "composer.lock"
)

// Project types.
const (
	TypeApplication = "application"
	TypeLibrary     = "library"
)

// Diagnostic codes.
const (
	CodeManifestInvalid = "observe.composer_manifest_invalid"
	CodePHPUnitInvalid  = "observe.composer_phpunit_invalid"
	CodeWalkBounded     = "observe.composer_walk_bounded"
)

// MaxWalkedEntries bounds the directory entries Candidates visits.
const MaxWalkedEntries = 200000

// phpunitConfigs are the PHPUnit configuration names, in the order PHPUnit
// looks them up.
var phpunitConfigs = []string{"phpunit.xml", "phpunit.dist.xml", "phpunit.xml.dist"}

// skippedDirs are never walked: dependencies installed by package managers.
var skippedDirs = map[string]bool{"vendor": true, "node_modules": true}

// Metadata is what the detector knows about one component beyond the probe
// project's own members. Every path is repository-relative and
// slash-separated.
type Metadata struct {
	// Lockfile is the composer.lock beside the manifest, when there is one.
	Lockfile string `json:"lockfile,omitempty"`
	// Sources are the PSR-4 autoload roots inside the component.
	Sources []string `json:"sources,omitempty"`
	// Tests are the PSR-4 autoload-dev roots and the PHPUnit test suite
	// directories inside the component.
	Tests []string `json:"tests,omitempty"`
	// Areas are the directories directly under the source and test roots.
	Areas []Area `json:"areas,omitempty"`
	// PHPUnit is the PHPUnit configuration beside the manifest.
	PHPUnit *PHPUnit `json:"phpunit,omitempty"`
}

// Area is one part of a component, such as a Symfony bundle or the
// src/Controller directory of an application.
type Area struct {
	// Name is the directory name.
	Name string `json:"name"`
	// Paths are the directories of the area, one per root that holds it.
	Paths []string `json:"paths"`
}

// PHPUnit is a PHPUnit configuration file and the reports it writes.
type PHPUnit struct {
	// Config is the configuration file.
	Config string `json:"config"`
	// JUnit, Clover and Cobertura are the report outputs the configuration
	// names, when it names them. A CI that passes --log-junit or
	// --coverage-clover instead leaves them empty.
	JUnit     string `json:"junit,omitempty"`
	Clover    string `json:"clover,omitempty"`
	Cobertura string `json:"cobertura,omitempty"`
}

// Location is where a repository path belongs.
type Location struct {
	// Component is the path of the component that holds the file.
	Component string
	// Area is the name of the area that holds it, or empty.
	Area string
}

// Detect finds the components of the repository at root.
func Detect(root string) wsproto.ProbeResult {
	paths, diags := Candidates(root)
	result := Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: ExtensionName, Paths: paths})
	result.Diagnostics = append(diags, result.Diagnostics...)
	return result
}

// Answer returns the probe answer of the repository at root, the function
// wsproto.ServeProbe serves.
func Answer(root string) wsproto.ProbeAnswerFunc {
	return func(request wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
		return Probe(root, request), nil
	}
}

// Candidates walks the repository at root and returns, in path order, the
// directories that hold a composer.json. It skips .git, vendor, node_modules
// and hidden directories.
func Candidates(root string) ([]string, []diag.Diagnostic) {
	var found []string
	var diags []diag.Diagnostic
	walked := 0
	err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		walked++
		if walked > MaxWalkedEntries {
			diags = append(diags, diag.Warningf(CodeWalkBounded, "", "the Composer detector stopped after %d entries", MaxWalkedEntries))
			return fs.SkipAll
		}
		if entry.IsDir() {
			name := entry.Name()
			if p != root && (strings.HasPrefix(name, ".") || skippedDirs[name]) {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Name() != Marker {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return nil
		}
		if dir, ok := wsproto.NormalizeProbePath(filepath.ToSlash(rel)); ok {
			found = append(found, dir)
		}
		return nil
	})
	if err != nil {
		diags = append(diags, diag.Warningf(CodeWalkBounded, "", "the Composer detector cannot walk the repository: %v", err))
	}
	slices.Sort(found)
	return found, diags
}

// manifest is the part of composer.json the detector reads.
type manifest struct {
	Name         string          `json:"name"`
	Type         string          `json:"type"`
	Version      string          `json:"version"`
	Require      map[string]any  `json:"require"`
	RequireDev   map[string]any  `json:"require-dev"`
	Autoload     autoload        `json:"autoload"`
	AutoloadDev  autoload        `json:"autoload-dev"`
	Repositories json.RawMessage `json:"repositories"`
}

type autoload struct {
	PSR4 map[string]json.RawMessage `json:"psr-4"`
}

// component is one parsed manifest.
type component struct {
	dir      string
	manifest manifest
	meta     Metadata
}

// Probe answers one probe request against the repository at root: one
// project per requested directory that holds a composer.json. A manifest
// that does not parse is a diagnostic.
func Probe(root string, request wsproto.ProbeRequest) wsproto.ProbeResult {
	result := wsproto.ProbeResult{Version: wsproto.ProbeProtocolVersion, Extension: ExtensionName}
	var components []*component
	seen := map[string]bool{}
	for _, candidate := range request.Paths {
		dir, ok := wsproto.NormalizeProbePath(candidate)
		if !ok || seen[dir] {
			continue
		}
		seen[dir] = true
		file := join(dir, Marker)
		data, err := os.ReadFile(abs(root, file))
		if err != nil {
			continue
		}
		var m manifest
		if err := json.Unmarshal(data, &m); err != nil {
			result.Diagnostics = append(result.Diagnostics, diag.Warningf(CodeManifestInvalid, file, "%s is left out: %v", file, err))
			continue
		}
		components = append(components, &component{dir: dir, manifest: m})
	}
	slices.SortFunc(components, func(a, b *component) int { return strings.Compare(a.dir, b.dir) })
	dirs := make([]string, len(components))
	byDir := map[string]*component{}
	for i, c := range components {
		dirs[i] = c.dir
		byDir[c.dir] = c
	}

	provided := pathPackages(root, components, byDir)
	for _, c := range components {
		result.Diagnostics = append(result.Diagnostics, readPHPUnit(root, c, dirs, byDir)...)
		c.meta.Lockfile = existing(root, join(c.dir, Lockfile))
		for _, r := range psr4Roots(c.dir, c.manifest.Autoload) {
			if owner := byDir[owning(dirs, r)]; owner != nil {
				owner.meta.Sources = append(owner.meta.Sources, r)
			}
		}
		for _, r := range psr4Roots(c.dir, c.manifest.AutoloadDev) {
			if owner := byDir[owning(dirs, r)]; owner != nil {
				owner.meta.Tests = append(owner.meta.Tests, r)
			}
		}
	}
	for _, c := range components {
		if len(c.manifest.Autoload.PSR4) == 0 && len(c.manifest.AutoloadDev.PSR4) == 0 && isDir(root, join(c.dir, "src")) {
			c.meta.Sources = append(c.meta.Sources, join(c.dir, "src"))
		}
		c.meta.Sources = sortedUnique(c.meta.Sources)
		// An autoload-dev root that a manifest also autoloads, such as an
		// application that autoloads its package's sources for its tests, stays
		// a source root.
		c.meta.Tests = sortedUnique(slices.DeleteFunc(c.meta.Tests, func(r string) bool { return slices.Contains(c.meta.Sources, r) }))
		c.meta.Areas = areas(root, c, dirs)
		result.Projects = append(result.Projects, project(c, provided))
	}
	return result
}

// project turns a component into its probe project.
func project(c *component, provided map[string]string) wsproto.ProbeProject {
	p := wsproto.ProbeProject{
		Path:       c.dir,
		SourceName: c.manifest.Name,
		SourceFile: join(c.dir, Marker),
		Version:    c.manifest.Version,
		Type:       TypeLibrary,
	}
	if c.manifest.Type == "project" {
		p.Type = TypeApplication
	}
	var deps []string
	for _, required := range []map[string]any{c.manifest.Require, c.manifest.RequireDev} {
		for name := range required {
			if dir, ok := provided[strings.ToLower(name)]; ok && dir != c.dir {
				deps = append(deps, dir)
			}
		}
	}
	p.Dependencies = sortedUnique(deps)
	if c.meta.Lockfile != "" {
		p.WatchedFiles = append(p.WatchedFiles, c.meta.Lockfile)
	}
	if c.meta.PHPUnit != nil {
		p.WatchedFiles = append(p.WatchedFiles, c.meta.PHPUnit.Config)
	}
	// Metadata marshals a struct of strings and slices, which cannot fail.
	p.Metadata, _ = json.Marshal(c.meta)
	return p
}

// pathPackages maps each package name a path repository provides, in lower
// case, to its directory. A path repository's url is a directory or a glob
// pattern relative to the manifest that declares it.
func pathPackages(root string, components []*component, byDir map[string]*component) map[string]string {
	provided := map[string]string{}
	for _, c := range components {
		for _, url := range pathRepositoryURLs(c.manifest.Repositories) {
			// The pattern is globbed relative to the repository, so the
			// directory the repository is checked out in never matters.
			pattern, ok := under(c.dir, url)
			if !ok {
				continue
			}
			matches, err := fs.Glob(os.DirFS(root), pattern)
			if err != nil {
				continue
			}
			for _, match := range matches {
				dir, ok := wsproto.NormalizeProbePath(match)
				if !ok {
					continue
				}
				if target := byDir[dir]; target != nil && target.manifest.Name != "" {
					name := strings.ToLower(target.manifest.Name)
					if _, taken := provided[name]; !taken {
						provided[name] = dir
					}
				}
			}
		}
	}
	return provided
}

// PathRepositoryDirs returns, in path order, the directories holding a
// composer.json that the path repositories of the manifest in dir match: the
// packages that manifest's Composer build installs from the repository. A
// directory with no composer.json returns none.
func PathRepositoryDirs(root, dir string) ([]string, error) {
	data, err := os.ReadFile(abs(root, join(dir, Marker)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	var dirs []string
	for _, url := range pathRepositoryURLs(m.Repositories) {
		pattern, ok := under(dir, url)
		if !ok {
			continue
		}
		matches, err := fs.Glob(os.DirFS(root), pattern)
		if err != nil {
			continue
		}
		for _, match := range matches {
			if d, ok := wsproto.NormalizeProbePath(match); ok && existing(root, join(d, Marker)) != "" {
				dirs = append(dirs, d)
			}
		}
	}
	return sortedUnique(dirs), nil
}

// pathRepositoryURLs returns the urls of the path repositories a manifest
// declares, as a list or as an object keyed by name.
func pathRepositoryURLs(raw json.RawMessage) []string {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		var keyed map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keyed); err != nil {
			return nil
		}
		keys := make([]string, 0, len(keyed))
		for key := range keyed {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			entries = append(entries, keyed[key])
		}
	}
	var urls []string
	for _, entry := range entries {
		var repository struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		}
		if json.Unmarshal(entry, &repository) == nil && repository.Type == "path" && repository.URL != "" {
			urls = append(urls, repository.URL)
		}
	}
	return urls
}

// psr4Roots returns the PSR-4 directories of an autoload section, joined to
// the manifest's directory. A value is one directory or a list of them; a
// directory outside the repository is left out.
func psr4Roots(dir string, section autoload) []string {
	var roots []string
	for _, raw := range section.PSR4 {
		var list []string
		var one string
		if json.Unmarshal(raw, &one) == nil {
			list = []string{one}
		} else if json.Unmarshal(raw, &list) != nil {
			continue
		}
		for _, entry := range list {
			if r, ok := under(dir, entry); ok {
				roots = append(roots, r)
			}
		}
	}
	return roots
}

// phpunitConfig is the part of a PHPUnit configuration the detector reads,
// in the PHPUnit 10+ layout and the older one.
type phpunitConfig struct {
	TestSuites []struct {
		Directories []string `xml:"directory"`
	} `xml:"testsuites>testsuite"`
	Logging struct {
		JUnit struct {
			OutputFile string `xml:"outputFile,attr"`
		} `xml:"junit"`
		Logs []struct {
			Type   string `xml:"type,attr"`
			Target string `xml:"target,attr"`
		} `xml:"log"`
	} `xml:"logging"`
	Reports struct {
		Clover struct {
			OutputFile string `xml:"outputFile,attr"`
		} `xml:"clover"`
		Cobertura struct {
			OutputFile string `xml:"outputFile,attr"`
		} `xml:"cobertura"`
	} `xml:"coverage>report"`
}

// readPHPUnit reads the PHPUnit configuration beside a manifest: it records
// the report outputs on the component and gives each test suite directory to
// the component that holds it.
func readPHPUnit(root string, c *component, dirs []string, byDir map[string]*component) []diag.Diagnostic {
	for _, name := range phpunitConfigs {
		file := join(c.dir, name)
		data, err := os.ReadFile(abs(root, file))
		if err != nil {
			continue
		}
		var config phpunitConfig
		if err := xml.NewDecoder(bytes.NewReader(data)).Decode(&config); err != nil {
			return []diag.Diagnostic{diag.Warningf(CodePHPUnitInvalid, file, "%s is left out: %v", file, err)}
		}
		unit := &PHPUnit{Config: file}
		unit.JUnit = output(c.dir, config.Logging.JUnit.OutputFile)
		unit.Clover = output(c.dir, config.Reports.Clover.OutputFile)
		unit.Cobertura = output(c.dir, config.Reports.Cobertura.OutputFile)
		for _, log := range config.Logging.Logs {
			switch log.Type {
			case "junit":
				unit.JUnit = firstNonEmpty(unit.JUnit, output(c.dir, log.Target))
			case "coverage-clover":
				unit.Clover = firstNonEmpty(unit.Clover, output(c.dir, log.Target))
			case "coverage-cobertura":
				unit.Cobertura = firstNonEmpty(unit.Cobertura, output(c.dir, log.Target))
			}
		}
		c.meta.PHPUnit = unit
		for _, suite := range config.TestSuites {
			for _, directory := range suite.Directories {
				r, ok := under(c.dir, directory)
				if !ok {
					continue
				}
				if owner := byDir[owning(dirs, r)]; owner != nil {
					owner.meta.Tests = append(owner.meta.Tests, r)
				}
			}
		}
		return nil
	}
	return nil
}

// output resolves a report output the configuration names against its
// directory; one outside the repository is left out.
func output(dir, name string) string {
	if strings.TrimSpace(name) == "" {
		return ""
	}
	r, ok := under(dir, name)
	if !ok {
		return ""
	}
	return r
}

// areas lists the directories directly under a component's own roots. A root
// that is the component directory itself names no area.
func areas(root string, c *component, dirs []string) []Area {
	byName := map[string][]string{}
	for _, r := range slices.Concat(c.meta.Sources, c.meta.Tests) {
		if r == c.dir {
			continue
		}
		entries, err := os.ReadDir(abs(root, r))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() || strings.HasPrefix(name, ".") {
				continue
			}
			p := join(r, name)
			if owning(dirs, p) != c.dir {
				continue
			}
			byName[name] = append(byName[name], p)
		}
	}
	list := make([]Area, 0, len(byName))
	for name, paths := range byName {
		list = append(list, Area{Name: name, Paths: sortedUnique(paths)})
	}
	slices.SortFunc(list, func(a, b Area) int { return strings.Compare(a.Name, b.Name) })
	return list
}

// ReadMetadata returns the detector's metadata of a probe project.
func ReadMetadata(p wsproto.ProbeProject) (Metadata, error) {
	var m Metadata
	if len(p.Metadata) == 0 {
		return m, nil
	}
	err := json.Unmarshal(p.Metadata, &m)
	return m, err
}

// Locate maps a repository path to the deepest component that holds it and
// to the area of that component whose path holds it. It reports false when
// no component holds the path.
func Locate(result wsproto.ProbeResult, file string) (Location, bool) {
	file, ok := wsproto.NormalizeProbePath(file)
	if !ok {
		return Location{}, false
	}
	var holder *wsproto.ProbeProject
	for i := range result.Projects {
		p := &result.Projects[i]
		if holds(p.Path, file) && (holder == nil || depth(p.Path) > depth(holder.Path)) {
			holder = p
		}
	}
	if holder == nil {
		return Location{}, false
	}
	location := Location{Component: holder.Path}
	meta, err := ReadMetadata(*holder)
	if err != nil {
		return location, true
	}
	deepest := ""
	for _, area := range meta.Areas {
		for _, p := range area.Paths {
			if holds(p, file) && len(p) > len(deepest) {
				deepest, location.Area = p, area.Name
			}
		}
	}
	return location, true
}

// owning returns the deepest component directory that holds p, or "" when
// none does.
func owning(dirs []string, p string) string {
	best := ""
	for _, dir := range dirs {
		if holds(dir, p) && (best == "" || depth(dir) > depth(best)) {
			best = dir
		}
	}
	return best
}

// holds reports whether the directory dir holds the path p.
func holds(dir, p string) bool {
	return dir == wsproto.ProbeRootPath || p == dir || strings.HasPrefix(p, dir+"/")
}

// under joins a path a manifest or a configuration names to the directory
// that holds it. An absolute path, or one outside the repository, is refused.
func under(dir, name string) (string, bool) {
	name = strings.TrimSpace(name)
	if strings.HasPrefix(name, "/") || filepath.IsAbs(name) {
		return "", false
	}
	return wsproto.NormalizeProbePath(join(dir, name))
}

// depth orders component directories from the repository root down.
func depth(dir string) int {
	if dir == wsproto.ProbeRootPath {
		return 0
	}
	return len(dir)
}

// join joins a slash path to a component directory.
func join(dir, name string) string {
	if dir == wsproto.ProbeRootPath {
		return path.Clean(name)
	}
	return path.Join(dir, name)
}

func abs(root, rel string) string {
	return filepath.Join(root, filepath.FromSlash(rel))
}

func existing(root, rel string) string {
	if info, err := os.Stat(abs(root, rel)); err == nil && info.Mode().IsRegular() {
		return rel
	}
	return ""
}

func isDir(root, rel string) bool {
	info, err := os.Stat(abs(root, rel))
	return err == nil && info.IsDir()
}

func sortedUnique(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	slices.Sort(list)
	return slices.Compact(list)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
