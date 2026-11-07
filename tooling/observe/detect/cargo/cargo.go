// Package cargo is the Cargo detector of @putnami/observe: it finds the
// crates of a Rust repository, such as a Cargo workspace that holds an API, a
// worker and the crate they share, from its Cargo.toml manifests, without
// running cargo.
//
// It answers over the workspace probe wire (go.putnami.dev/protocol/workspace):
// Probe has the shape of a probe answer, and its result passes the probe
// protocol's strict validation, so the detector can move to its own extension
// once the CLI can run a probe outside a workspace. Outside a workspace no core
// lists the candidate directories, so Candidates walks the repository for
// them.
//
// A member of the root's [workspace] is a workspace component; any other
// Cargo.toml with a [package] table is a manifest one. A dependency with a
// path, or with workspace = true when [workspace.dependencies] gives that
// entry a path, is an edge.
package cargo

import (
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/observe/detect/memberglob"
	"go.putnami.dev/tooling/observe/internal/toml"
)

// ExtensionName is the extension the detector answers under; it keys the
// provider-owned metadata of each project.
const ExtensionName = "@putnami/observe"

// Ecosystem is the evidence ecosystem slug of a crate.
const Ecosystem = "rust"

// Files the detector reads.
const (
	Marker    = "Cargo.toml"
	CargoLock = "Cargo.lock"
)

// Component sources, as protocol/evidence names them.
const (
	SourceWorkspace = "workspace"
	SourceManifest  = "manifest"
)

// Diagnostic codes.
const (
	CodeManifestInvalid = "observe.cargo_manifest_invalid"
	CodeWalkBounded     = "observe.cargo_walk_bounded"
)

// MaxWalkedEntries bounds the directory entries Candidates visits.
const MaxWalkedEntries = 200000

// skippedDirs are never walked: build outputs and vendored or installed
// packages. target also holds the copies cargo package writes.
var skippedDirs = map[string]bool{"target": true, "node_modules": true, "vendor": true}

// skipped reports whether a walk leaves the directory of that name out.
func skipped(name string) bool {
	return strings.HasPrefix(name, ".") || skippedDirs[name]
}

// dependencyTables are the tables of a manifest, or of one [target] platform,
// that declare dependencies.
var dependencyTables = []string{"dependencies", "dev-dependencies", "build-dependencies"}

// Metadata is what the detector knows about one crate beyond the probe
// project's own members. Every path is repository-relative and
// slash-separated.
type Metadata struct {
	// Source is workspace for a workspace member, manifest otherwise.
	Source string `json:"source"`
	// Lockfile is the Cargo.lock beside the manifest, else the workspace's
	// Cargo.lock for a member.
	Lockfile string `json:"lockfile,omitempty"`
}

// Detect finds the crates of the repository at root.
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
// directories that hold a Cargo.toml. It skips target, node_modules, vendor
// and hidden directories, such as .git and .cargo.
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
			diags = append(diags, diag.Warningf(CodeWalkBounded, "", "the Cargo detector stopped after %d entries", MaxWalkedEntries))
			return fs.SkipAll
		}
		if entry.IsDir() {
			if p != root && skipped(entry.Name()) {
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
		diags = append(diags, diag.Warningf(CodeWalkBounded, "", "the Cargo detector cannot walk the repository: %v", err))
	}
	slices.Sort(found)
	return found, diags
}

// component is one parsed manifest.
type component struct {
	dir      string
	manifest toml.Table
	name     string
	meta     Metadata
}

// workspace is the Cargo workspace the root Cargo.toml declares.
type workspace struct {
	declared bool
	members  map[string]bool
	// dependencies is [workspace.dependencies].
	dependencies toml.Table
	// version is [workspace.package] version, which a member inherits with
	// version.workspace = true.
	version string
}

// Probe answers one probe request against the repository at root: one
// project per requested directory whose Cargo.toml is a workspace member or
// has a [package] table. A manifest that does not parse is a diagnostic.
func Probe(root string, request wsproto.ProbeRequest) wsproto.ProbeResult {
	result := wsproto.ProbeResult{Version: wsproto.ProbeProtocolVersion, Extension: ExtensionName}
	ws, diags := readWorkspace(root)
	result.Diagnostics = append(result.Diagnostics, diags...)
	var components []*component
	seen := map[string]bool{}
	for _, candidate := range request.Paths {
		dir, ok := wsproto.NormalizeProbePath(candidate)
		if !ok || seen[dir] {
			continue
		}
		seen[dir] = true
		manifest, found, err := readManifest(root, dir)
		if !found {
			continue
		}
		if err != nil {
			file := join(dir, Marker)
			result.Diagnostics = append(result.Diagnostics, diag.Warningf(CodeManifestInvalid, file, "%s is left out: %v", file, err))
			continue
		}
		_, hasPackage := manifest["package"].(toml.Table)
		c := &component{dir: dir, manifest: manifest, name: toml.String(manifest, "package", "name"), meta: Metadata{Source: SourceManifest}}
		switch {
		case dir == wsproto.ProbeRootPath && ws.declared:
			// A virtual manifest is no crate; a root package is a member.
			if !hasPackage {
				continue
			}
			c.meta.Source = SourceWorkspace
		case ws.members[dir]:
			c.meta.Source = SourceWorkspace
		case !hasPackage:
			continue
		}
		components = append(components, c)
	}
	slices.SortFunc(components, func(a, b *component) int { return strings.Compare(a.dir, b.dir) })
	byDir := map[string]*component{}
	for _, c := range components {
		byDir[c.dir] = c
	}
	rootLock := existing(root, CargoLock)
	for _, c := range components {
		c.meta.Lockfile = existing(root, join(c.dir, CargoLock))
		if c.meta.Lockfile == "" && c.meta.Source == SourceWorkspace {
			c.meta.Lockfile = rootLock
		}
		result.Projects = append(result.Projects, project(c, ws, edges(c, ws, byDir)))
	}
	return result
}

// readWorkspace reads the [workspace] of the root Cargo.toml: the directories
// its members globs match, less those at or below an exclude entry, that hold a
// Cargo.toml, its [workspace.dependencies] and its [workspace.package]
// version.
func readWorkspace(root string) (workspace, []diag.Diagnostic) {
	var ws workspace
	manifest, found, err := readManifest(root, wsproto.ProbeRootPath)
	if !found || err != nil {
		// Probe reports a root manifest that does not parse.
		return ws, nil
	}
	table, ok := manifest["workspace"].(toml.Table)
	if !ok {
		return ws, nil
	}
	ws.declared = true
	ws.members = map[string]bool{}
	ws.dependencies, _ = table["dependencies"].(toml.Table)
	ws.version = toml.String(table, "package", "version")
	var diags []diag.Diagnostic
	patterns := func(key string) [][]string {
		var out [][]string
		for _, pattern := range toml.Strings(table, key) {
			segments, ok := memberglob.Segments(pattern)
			if !ok {
				diags = append(diags, diag.Warningf(CodeManifestInvalid, Marker, "the Cargo workspace %s pattern %q is not a repository-relative glob and is ignored", key, pattern))
				continue
			}
			out = append(out, segments)
		}
		return out
	}
	include, exclude := patterns("members"), patterns("exclude")
	dirs, bounded := memberglob.Dirs(root, include, skipped, MaxWalkedEntries)
	if bounded {
		diags = append(diags, diag.Warningf(CodeWalkBounded, Marker, "the Cargo workspace walk stopped after %d entries; members past it are left out", MaxWalkedEntries))
	}
	for _, dir := range dirs {
		parts := strings.Split(dir, "/")
		if slices.ContainsFunc(exclude, func(segments []string) bool { return excludes(segments, parts) }) {
			continue
		}
		if existing(root, join(dir, Marker)) != "" {
			ws.members[dir] = true
		}
	}
	return ws, diags
}

// excludes reports whether an exclude entry holds the directory: Cargo
// excludes the entry's directory and every directory below it.
func excludes(segments, parts []string) bool {
	for n := len(parts); n > 0; n-- {
		if memberglob.Match(segments, parts[:n]) {
			return true
		}
	}
	return false
}

// readManifest reads the Cargo.toml of a directory. found is false when there
// is none.
func readManifest(root, dir string) (toml.Table, bool, error) {
	data, err := os.ReadFile(abs(root, join(dir, Marker)))
	if err != nil {
		return nil, false, nil
	}
	manifest, err := toml.Parse(data)
	return manifest, true, err
}

// packageVersion is the [package] version, or the workspace's for a member
// that writes version.workspace = true.
func packageVersion(manifest toml.Table, ws workspace) string {
	switch v := toml.Get(manifest, "package", "version").(type) {
	case string:
		return v
	case toml.Table:
		if v["workspace"] == true {
			return ws.version
		}
	}
	return ""
}

// edges returns the directories of the crates c depends on: each entry of its
// dependency tables, and of the same tables under each [target] platform,
// that gives a path, resolved against its directory, or workspace = true for
// an entry of [workspace.dependencies] that gives a path, resolved against
// the root, for a workspace member.
func edges(c *component, ws workspace, byDir map[string]*component) []string {
	var deps []string
	add := func(dir string, ok bool) {
		if ok && dir != c.dir && byDir[dir] != nil {
			deps = append(deps, dir)
		}
	}
	for _, table := range declaredTables(c.manifest) {
		for key, value := range table {
			entry, ok := value.(toml.Table)
			if !ok {
				// A version string names a registry crate.
				continue
			}
			if p, ok := entry["path"].(string); ok {
				add(under(c.dir, p))
				continue
			}
			if entry["workspace"] != true || c.meta.Source != SourceWorkspace {
				continue
			}
			if inherited, ok := ws.dependencies[key].(toml.Table); ok {
				if p, ok := inherited["path"].(string); ok {
					add(under(wsproto.ProbeRootPath, p))
				}
			}
		}
	}
	return sortedUnique(deps)
}

// declaredTables returns the dependency tables of a manifest: its own, then
// those of each [target] platform in name order.
func declaredTables(manifest toml.Table) []toml.Table {
	var out []toml.Table
	collect := func(t toml.Table) {
		for _, key := range dependencyTables {
			if deps, ok := t[key].(toml.Table); ok {
				out = append(out, deps)
			}
		}
	}
	collect(manifest)
	targets, _ := manifest["target"].(toml.Table)
	platforms := make([]string, 0, len(targets))
	for platform := range targets {
		platforms = append(platforms, platform)
	}
	slices.Sort(platforms)
	for _, platform := range platforms {
		if t, ok := targets[platform].(toml.Table); ok {
			collect(t)
		}
	}
	return out
}

// project turns a crate into its probe project.
func project(c *component, ws workspace, deps []string) wsproto.ProbeProject {
	p := wsproto.ProbeProject{
		Path:         c.dir,
		SourceName:   c.name,
		SourceFile:   join(c.dir, Marker),
		Version:      packageVersion(c.manifest, ws),
		Dependencies: deps,
		WatchedFiles: []string{join(c.dir, Marker)},
	}
	if c.meta.Lockfile != "" {
		p.WatchedFiles = append(p.WatchedFiles, c.meta.Lockfile)
	}
	// Metadata marshals a struct of strings, which cannot fail.
	p.Metadata, _ = json.Marshal(c.meta)
	return p
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

// binaryID is a cargo-nextest test binary id: a package name, then
// optionally :: and the binary's target, such as pricing::integration or
// api::bin/api.
var binaryID = regexp.MustCompile(`^([A-Za-z0-9_-]+)(::[A-Za-z0-9_./-]+)?$`)

// Locator returns the function that maps a cargo-nextest binary id, which a
// nextest JUnit report writes as each case's class name and suite name, to
// the Cargo.toml and directory of the crate of that package name. A
// workspace member names its package over another directory, else the first
// directory in path order does. Package names compare with - and _ as one.
func Locator(result wsproto.ProbeResult) func(string) (file, component string, ok bool) {
	byName := map[string]wsproto.ProbeProject{}
	for _, workspaceFirst := range []bool{true, false} {
		for _, p := range result.Projects {
			meta, _ := ReadMetadata(p)
			if (meta.Source == SourceWorkspace) != workspaceFirst || p.SourceName == "" {
				continue
			}
			if _, taken := byName[packageKey(p.SourceName)]; !taken {
				byName[packageKey(p.SourceName)] = p
			}
		}
	}
	return func(id string) (string, string, bool) {
		m := binaryID.FindStringSubmatch(strings.TrimSpace(id))
		if m == nil {
			return "", "", false
		}
		p, ok := byName[packageKey(m[1])]
		if !ok {
			return "", "", false
		}
		return p.SourceFile, p.Path, true
	}
}

// packageKey is a package name as Cargo compares it with its crate name.
func packageKey(name string) string {
	return strings.ReplaceAll(name, "-", "_")
}

// under joins a path a manifest names to the directory that holds it. An
// absolute path, or one outside the repository, is refused.
func under(dir, name string) (string, bool) {
	name = strings.TrimSpace(name)
	if strings.HasPrefix(name, "/") || filepath.IsAbs(name) {
		return "", false
	}
	return wsproto.NormalizeProbePath(join(dir, name))
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

func sortedUnique(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	slices.Sort(list)
	return slices.Compact(list)
}
