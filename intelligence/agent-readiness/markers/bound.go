package markers

import (
	"path"
	"regexp"
	"slices"
	"strings"

	"go.putnami.dev/intelligence/agent-readiness/areas"
	"go.putnami.dev/intelligence/agent-readiness/contract"
	"go.putnami.dev/intelligence/agent-readiness/internal/ci"
	"go.putnami.dev/intelligence/agent-readiness/internal/scope"
)

// graphTools order builds by the areas a manifest declares.
var graphTools = map[string]bool{"bazel": true, "cargo": true, "go.work": true, "lerna": true, "maven": true, "nx": true, "putnami": true}

// graphFiles are build-graph tools configured by a root file of their own.
var graphFiles = []string{"turbo.json", "nx.json", "MODULE.bazel", "WORKSPACE", "WORKSPACE.bazel", "pants.toml", "rush.json"}

func (c *computation) declaredAreas() contract.Marker {
	const id = "bound.declared-areas"
	if !c.Layout.Declared() {
		return marker(id, contract.MarkerAbsent, ratio(0, c.Repo.FileChanges),
			fileList("git ls-files --", []string{"pnpm-workspace.yaml", "go.work", "Cargo.toml", "pom.xml", "turbo.json", "nx.json", "MODULE.bazel", "putnami.workspace.json"}), nil)
	}
	var sample, graphs []string
	for _, manifest := range c.Layout.Manifests {
		sample = append(sample, manifest.Path)
		if graphTools[manifest.Kind] {
			graphs = append(graphs, manifest.Path)
		}
	}
	for _, file := range c.present(graphFiles) {
		if !slices.Contains(sample, file) {
			sample = append(sample, file)
		}
		graphs = append(graphs, file)
	}
	value := ratio(c.Repo.DeclaredChanges, c.Repo.FileChanges)
	if len(graphs) == 0 {
		return marker(id, contract.MarkerExists, value, fileList("git ls-files --", sample), sample)
	}
	// The share is read on today's areas, so it holds at once for the whole
	// window: only the age of the build graph proves the areas held.
	return enforcedFor(marker(id, contract.MarkerEnforced, value, fileList("git log --diff-filter=A --format=%cs --", graphs), sample),
		days(c.Now, c.oldestAdded(graphs)))
}

// graphManifests names the workspace manifests and build-graph files whose
// age bound.declared-areas reads.
func graphManifests(layout areas.Layout) []string {
	paths := append([]string{}, graphFiles...)
	for _, manifest := range layout.Manifests {
		paths = append(paths, manifest.Path)
	}
	return paths
}

// boundaryFiles configure a tool that forbids deep imports.
var boundaryFiles = []string{
	".dependency-cruiser.js", ".dependency-cruiser.cjs", ".dependency-cruiser.mjs", ".dependency-cruiser.json", ".importlinter",
}

func boundaryConfig(base string) bool {
	for _, name := range boundaryFiles {
		if base == name {
			return true
		}
	}
	return false
}

// boundaryRuleNames are the ESLint rules that forbid deep imports.
var boundaryRuleNames = []string{"boundaries/", "no-restricted-imports", "enforce-module-boundaries", "import/no-internal-modules"}

func isESLintConfig(base string) bool {
	return strings.HasPrefix(base, ".eslintrc") || strings.HasPrefix(base, "eslint.config.")
}

// boundaryTools returns the configurations that forbid deep imports and the
// CI jobs that run them.
func (c *computation) boundaryTools() ([]string, []ci.Config) {
	var configs []string
	fragments := []string{"depcruise", "dependency-cruiser", "lint-imports"}
	for _, file := range c.Files {
		base := path.Base(file.Path)
		switch {
		case boundaryConfig(base):
			configs = append(configs, file.Path)
		case isESLintConfig(base) && mentionsAny(string(c.Contents[file.Path]), boundaryRuleNames):
			configs = append(configs, file.Path)
			fragments = append(fragments, "eslint", "lint")
		}
	}
	if len(configs) == 0 {
		return nil, nil
	}
	return configs, ci.Runs(c.configs, fragments...)
}

// entryPoint returns the file that declares an area's public surface: a
// package "exports" map, an index module, or a Go internal/ directory the
// compiler protects.
func (c *computation) entryPoint(area int) (string, bool) {
	dir := c.dir(area)
	goInternal := ""
	for _, file := range c.Files {
		if !strings.HasPrefix(file.Path, dir) || !c.inArea(file.Path, area) {
			continue
		}
		rest := strings.TrimPrefix(file.Path, dir)
		switch rest {
		case "index.ts", "index.js", "index.tsx", "src/index.ts", "src/index.js", "src/lib.rs", "src/mod.rs", "__init__.py":
			return file.Path, false
		case "package.json":
			if strings.Contains(string(c.Contents[file.Path]), `"exports"`) {
				return file.Path, false
			}
		}
		if goInternal == "" && strings.HasSuffix(file.Path, ".go") && strings.Contains("/"+rest, "/internal/") {
			goInternal = file.Path
		}
	}
	return goInternal, goInternal != ""
}

func (c *computation) boundaryRules(area int) contract.Marker {
	const id = "bound.boundary-rules"
	configs, jobs := c.boundaryTools()
	goJobs := ci.Runs(c.configs, "go build", "go test", "go vet", "putnami build", "putnami test")
	var covering []scopedTool
	for _, tool := range c.scopedBoundaryTools() {
		if area < 0 || tool.covers(c.dir(area)) {
			covering = append(covering, tool)
		}
	}
	// A tool that forbids imports across the areas it covers enforces the
	// boundary of each of them, entry point or not, once CI runs it.
	var scoped *contract.Marker
	for _, tool := range covering {
		if len(tool.jobs) > 0 {
			found := marker(id, contract.MarkerEnforced, days(c.Now, c.enforcedSince(tool.configs, tool.jobs)),
				fileList("git log --diff-filter=A --format=%cs --", tool.configs), append(append([]string{}, tool.configs...), jobPaths(tool.jobs)...))
			if scoped == nil || longer(found, *scoped) {
				scoped = &found
			}
		}
	}
	result := c.entryBoundary(id, area, configs, jobs, goJobs)
	switch {
	case scoped != nil && (result.State != contract.MarkerEnforced || longer(*scoped, result)):
		// The enforcement that has held longest speaks for the marker.
		return *scoped
	case result.State == contract.MarkerAbsent && len(covering) > 0:
		var toolConfigs []string
		for _, tool := range covering {
			toolConfigs = append(toolConfigs, tool.configs...)
		}
		return marker(id, contract.MarkerExists, nil, fileList("git ls-files --", toolConfigs), toolConfigs)
	}
	return result
}

// longer reports whether enforced marker a has held longer than b. A marker
// whose enforcement has no known age holds for less than any dated one.
func longer(a, b contract.Marker) bool {
	switch {
	case a.Value == nil:
		return false
	case b.Value == nil:
		return true
	}
	return *a.Value > *b.Value
}

// entryBoundary reads bound.boundary-rules from the area's entry point, or
// from every area's entry point for the repository-wide marker.
func (c *computation) entryBoundary(id string, area int, configs []string, jobs, goJobs []ci.Config) contract.Marker {
	if area < 0 {
		var entries []string
		compilerEnforced := false
		for i := range c.Layout.Areas {
			if entry, isGo := c.entryPoint(i); entry != "" {
				entries = append(entries, entry)
				compilerEnforced = compilerEnforced || isGo
			}
		}
		return c.boundaryMarker(id, entries, compilerEnforced, configs, jobs, goJobs)
	}
	entry, isGo := c.entryPoint(area)
	var entries []string
	if entry != "" {
		entries = []string{entry}
	}
	return c.boundaryMarker(id, entries, isGo, configs, jobs, goJobs)
}

type scopedTool struct {
	configs []string
	jobs    []ci.Config
}

// covers reports whether one of the tool's configurations sits in dir or
// above it. dir is an area's directory prefix, "" for the repository.
func (t scopedTool) covers(dir string) bool {
	for _, config := range t.configs {
		root := path.Dir(config)
		if root == "." || strings.HasPrefix(dir, root+"/") {
			return true
		}
	}
	return false
}

// architectureManifest is the file the Putnami architecture gate reads: the
// dependencies a domain may take on the others.
const architectureManifest = "putnami.architecture.json"

// architectureRuns are the CI fragments that run the Putnami architecture
// gate: validate-workspace, directly or as the companion of validate.
var architectureRuns = []string{"putnami validate-workspace", "putnami architecture", "putnami validate"}

// depguardRuns are the CI fragments that run golangci-lint.
var depguardRuns = []string{"golangci-lint", "lint"}

// managedSections are the parts of a Maven project that only declare a
// version for the modules that ask for one.
var managedSections = regexp.MustCompile(`(?s)<(dependencyManagement|pluginManagement)>.*?</(dependencyManagement|pluginManagement)>`)

// archunitArtifact matches a Maven dependency on an ArchUnit artifact.
var archunitArtifact = regexp.MustCompile(`<artifactId>\s*archunit`)

// goModule matches the module path a go.mod declares.
var goModule = regexp.MustCompile(`(?m)^module\s+(\S+)`)

// boundaryToolConfig reports whether a file may configure a scoped boundary
// tool, so that the collector reads and dates it.
func boundaryToolConfig(file string) bool {
	base := path.Base(file)
	switch {
	case base == architectureManifest, strings.HasPrefix(base, ".golangci."):
		return true
	case base == "build.gradle", base == "build.gradle.kts":
		return true
	}
	return false
}

// scopedBoundaryTools returns the scoped boundary tools the repository
// configures, each with the CI jobs that run it. A depguard rule counts only
// when it names a package of the repository's own Go modules: a rule that
// only bans a third-party package guards no boundary between areas. An
// ArchUnit dependency counts where a Maven or Gradle build declares it for
// its own modules, not in a section that only manages versions.
func (c *computation) scopedBoundaryTools() []scopedTool {
	var manifests, depguards, archunits, modules []string
	for _, file := range c.Files {
		if path.Base(file.Path) == "go.mod" {
			if found := goModule.FindSubmatch(c.Contents[file.Path]); found != nil {
				modules = append(modules, string(found[1]))
			}
		}
	}
	for _, file := range c.Files {
		if scope.Skipped(file.Path) || areas.Fixture(path.Dir(file.Path)) {
			continue
		}
		base := path.Base(file.Path)
		content := string(c.Contents[file.Path])
		switch {
		case base == architectureManifest:
			manifests = append(manifests, file.Path)
		case strings.HasPrefix(base, ".golangci.") && strings.Contains(content, "depguard") && mentionsAny(content, modules):
			depguards = append(depguards, file.Path)
		case base == "pom.xml" && archunitArtifact.MatchString(managedSections.ReplaceAllString(content, "")),
			(base == "build.gradle" || base == "build.gradle.kts") && strings.Contains(content, "com.tngtech.archunit"):
			archunits = append(archunits, file.Path)
		}
	}
	var tools []scopedTool
	if len(manifests) > 0 {
		tools = append(tools, scopedTool{configs: manifests, jobs: ci.Runs(c.configs, architectureRuns...)})
	}
	if len(depguards) > 0 {
		tools = append(tools, scopedTool{configs: depguards, jobs: ci.Runs(c.configs, depguardRuns...)})
	}
	if len(archunits) > 0 {
		// ArchUnit rules are tests: the Maven or Gradle build runs them.
		tools = append(tools, scopedTool{configs: archunits, jobs: ci.Runs(c.configs, append(append([]string{}, javaBuilds...), testRuns...)...)})
	}
	return tools
}

func (c *computation) boundaryMarker(id string, entries []string, compilerEnforced bool, configs []string, jobs, goJobs []ci.Config) contract.Marker {
	if len(entries) == 0 {
		return marker(id, contract.MarkerAbsent, nil, "git ls-files -- '*index.ts' '*index.js' '*/internal/*.go'", nil)
	}
	command := fileList("git ls-files --", entries)
	switch {
	case len(jobs) > 0:
		return marker(id, contract.MarkerEnforced, days(c.Now, c.enforcedSince(configs, jobs)),
			fileList("git log --diff-filter=A --format=%cs --", configs), append(configs, jobPaths(jobs)...))
	case compilerEnforced && len(goJobs) > 0:
		return marker(id, contract.MarkerEnforced, days(c.Now, c.enforcedSince(nil, goJobs)), command, append(entries, jobPaths(goJobs)...))
	}
	return marker(id, contract.MarkerExists, nil, command, entries)
}

func (c *computation) crossArea(area int) contract.Marker {
	const id = "bound.cross-area-changes"
	activity := c.activity(area)
	command := "git log --since=90.days --no-merges --name-only --format=%H"
	if area >= 0 {
		command = "git log --since=90.days --no-merges --full-diff --name-only --format=%H -- " + c.Layout.Areas[area].Path
	}
	commits := activity.Commits
	if area < 0 {
		// A commit counts when it changed at least one area other than the
		// root, which only holds shared configuration.
		commits = c.nonRootCommits()
	}
	value := ratio(activity.CrossArea, commits)
	if value == nil {
		return marker(id, contract.MarkerAbsent, nil, command, nil)
	}
	// The treatment is CI that checks the areas depending on a change, so a
	// cross-area change cannot land unchecked: it makes the marker enforced
	// whatever the share. EnforcedDays says how long that CI has run: L4
	// needs the share at or under the bound with the CI in place for the
	// whole 90-day window the value measures.
	if jobs := ci.Runs(c.configs, impactSelections...); len(jobs) > 0 {
		return enforcedFor(marker(id, contract.MarkerEnforced, value, command, jobPaths(jobs)), days(c.Now, c.enforcedSince(nil, jobs)))
	}
	return marker(id, contract.MarkerExists, value, command, nil)
}

// impactSelections are the CI fragments that select the areas a change
// affects and their dependents: Putnami --impacted, Nx affected, and a
// Turborepo git-range filter or --affected.
var impactSelections = []string{"--impacted", "nx affected", "--affected", "...["}

// nonRootCommits counts the commits that changed an area other than the
// root, or the root when it is the only area.
func (c *computation) nonRootCommits() int {
	if len(c.Layout.Areas) == 1 {
		return c.Repo.Commits
	}
	return c.Repo.AreaCommits
}
