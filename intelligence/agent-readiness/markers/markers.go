// Package markers computes the twelve deterministic markers of method 0.1
// from what the collector read: the tree at HEAD, a few configuration files,
// and 90 days of history. The method page, https://putnami.dev/agent-readiness/method,
// defines each marker, its states and its unit.
//
// Every marker is sent once for the whole repository. An area gets its own
// copy of a marker only when its reading differs from the repository's, and
// only for the most active areas, which keeps the payload small on large
// monorepos: the server reads an absent area marker as the repository's.
package markers

import (
	"math"
	"path"
	"regexp"
	"sort"
	"strings"

	"go.putnami.dev/intelligence/agent-readiness/areas"
	"go.putnami.dev/intelligence/agent-readiness/contract"
	"go.putnami.dev/intelligence/agent-readiness/history"
	"go.putnami.dev/intelligence/agent-readiness/internal/ci"
	"go.putnami.dev/intelligence/agent-readiness/internal/gitrepo"
	"go.putnami.dev/intelligence/agent-readiness/internal/scope"
	"go.putnami.dev/intelligence/agent-readiness/inventory"
)

// MaxDetailedAreas bounds how many areas get their own markers.
const MaxDetailedAreas = 40

const (
	secondsPerDay   = 24 * 60 * 60
	maxCommandBytes = 300
	maxSample       = 5
)

// Input is everything the markers read. The collector gathers it in a fixed
// number of git passes.
type Input struct {
	// Now is the collection time in Unix seconds.
	Now int64
	// Born is the committer time of the oldest root commit in Unix seconds,
	// zero when unknown.
	Born int64
	// Files are every tracked file at HEAD, sorted by path.
	Files  []gitrepo.File
	Layout areas.Layout
	// Repo and Areas are the window's activity, as Measure returns it.
	Repo  Activity
	Areas []Activity
	// Changes are the window's first-parent changes.
	Changes []history.Change
	// Contents holds the files Wanted named.
	Contents map[string][]byte
	// Dates holds the history of the files Dated named.
	Dates gitrepo.FileDates
	// Signals are the lines that match SignalPattern at HEAD.
	Signals []gitrepo.Match
	// SignalContents holds the content of the files SignalFiles named, so
	// that the sample holds only the matches CountsSignal keeps.
	SignalContents map[string][]byte
	// SignalsAdded counts, per file, the matching lines the window added
	// that CountsSignal kept.
	SignalsAdded map[string]int
}

// Compute returns the repository-wide markers in catalog order, then the
// area markers of the most active areas.
func Compute(in Input) []contract.Marker {
	c := newComputation(in)
	repoWide := map[string]contract.Marker{}
	var out []contract.Marker
	for _, definition := range contract.MarkerCatalog {
		marker := c.repository(definition.ID)
		repoWide[definition.ID] = marker
		out = append(out, marker)
	}
	for _, area := range c.detailed() {
		for _, definition := range contract.MarkerCatalog {
			marker, ok := c.area(definition.ID, area)
			if !ok || same(marker, repoWide[definition.ID]) {
				continue
			}
			marker.Area = in.Layout.Areas[area].Name
			out = append(out, marker)
		}
	}
	return out
}

func same(a, b contract.Marker) bool {
	if a.State != b.State || (a.Value == nil) != (b.Value == nil) {
		return false
	}
	return a.Value == nil || math.Abs(*a.Value-*b.Value) < 1e-9
}

type computation struct {
	Input
	paths    map[string]bool
	configs  []ci.Config
	commands []command
	signal   SignalContext
}

func newComputation(in Input) *computation {
	c := &computation{Input: in, paths: map[string]bool{}}
	files := make([]string, 0, len(in.Files))
	for _, file := range in.Files {
		c.paths[file.Path] = true
		files = append(files, file.Path)
	}
	c.configs = ci.Load(files, in.Contents)
	c.commands = discoverCommands(c)
	c.signal = NewSignalContext(files, in.Contents, in.SignalContents)
	return c
}

// detailed returns the areas with commits in the window, most active first,
// up to MaxDetailedAreas.
func (c *computation) detailed() []int {
	var active []int
	for i, activity := range c.Areas {
		if activity.Commits > 0 {
			active = append(active, i)
		}
	}
	sort.SliceStable(active, func(i, j int) bool {
		return c.Areas[active[i]].Commits > c.Areas[active[j]].Commits
	})
	if len(active) > MaxDetailedAreas {
		active = active[:MaxDetailedAreas]
	}
	return active
}

func (c *computation) repository(id string) contract.Marker {
	switch id {
	case "understand.instructions":
		return c.instructions(-1)
	case "understand.commands":
		return c.discoverable()
	case "understand.area-docs":
		return c.areaDocs(-1)
	case "bound.declared-areas":
		return c.declaredAreas()
	case "bound.boundary-rules":
		return c.boundaryRules(-1)
	case "bound.cross-area-changes":
		return c.crossArea(-1)
	case "verify.tests":
		return c.tests(-1)
	case "verify.static-checks":
		return c.staticChecks()
	case "verify.reliable-signal":
		return c.reliableSignal(-1)
	case "verify.pinned-toolchain":
		return c.pinnedToolchain()
	case "recover.ownership":
		return c.ownership(-1)
	case "recover.small-changes":
		return c.smallChanges()
	case "recover.contained-changes":
		return c.containedChanges()
	}
	panic("markers: no rule for " + id)
}

// area returns an area's reading of a marker; false when the marker only
// exists for the whole repository or the area has nothing of its own.
func (c *computation) area(id string, area int) (contract.Marker, bool) {
	if c.Layout.Areas[area].Path == areas.RootPath {
		return contract.Marker{}, false
	}
	switch id {
	case "understand.instructions":
		if len(c.instructionFiles(area)) == 0 {
			return contract.Marker{}, false
		}
		return c.instructions(area), true
	case "understand.area-docs":
		return c.areaDocs(area), true
	case "bound.boundary-rules":
		return c.boundaryRules(area), true
	case "bound.cross-area-changes":
		return c.crossArea(area), true
	case "verify.tests":
		return c.tests(area), true
	case "verify.reliable-signal":
		return c.reliableSignal(area), true
	case "recover.ownership":
		return c.ownership(area), true
	}
	return contract.Marker{}, false
}

func (c *computation) activity(area int) Activity {
	if area < 0 {
		return c.Repo
	}
	return c.Areas[area]
}

// dir is the area's directory prefix, "" for the repository.
func (c *computation) dir(area int) string {
	if area < 0 || c.Layout.Areas[area].Path == areas.RootPath {
		return ""
	}
	return c.Layout.Areas[area].Path + "/"
}

// inArea reports whether a file belongs to the area, or to any area for -1.
func (c *computation) inArea(file string, area int) bool {
	return area < 0 || c.Layout.Assign(file) == area
}

func marker(id string, state contract.MarkerState, value *float64, command string, sample []string) contract.Marker {
	return contract.Marker{ID: id, State: state, Value: value, Evidence: evidence(command, sample)}
}

func evidence(command string, sample []string) contract.Evidence {
	kept := []string{}
	for _, location := range sample {
		if len(kept) == maxSample {
			break
		}
		if safeLocation(location) {
			kept = append(kept, location)
		}
	}
	return contract.Evidence{Command: command, Sample: kept}
}

func safeLocation(location string) bool {
	file, _, _ := strings.Cut(location, ":")
	return len(location) <= 512 && areasPath.MatchString(file)
}

// fileList builds a "git ls-files -- <paths>" style command that fits the
// contract's 300 bytes, dropping the paths that do not fit.
func fileList(prefix string, paths []string) string {
	command := prefix
	for _, file := range paths {
		if !safeLocation(file) || len(command)+1+len(file) > maxCommandBytes {
			continue
		}
		command += " " + file
	}
	return command
}

func days(now, then int64) *float64 {
	if then <= 0 {
		return nil
	}
	return number(float64(max(0, now-then) / secondsPerDay))
}

func number(value float64) *float64 {
	rounded := math.Round(value*10000) / 10000
	return &rounded
}

func ratio(part, whole int) *float64 {
	if whole == 0 {
		return nil
	}
	return number(float64(part) / float64(whole))
}

// lag is how many whole days a document trails the newest code change.
func lag(code, document int64) *float64 {
	if code == 0 || document == 0 {
		return nil
	}
	return number(float64(max(0, code-document) / secondsPerDay))
}

// present keeps the candidates tracked at HEAD, in their given order.
func (c *computation) present(candidates []string) []string {
	var found []string
	for _, candidate := range candidates {
		if c.paths[candidate] {
			found = append(found, candidate)
		}
	}
	return found
}

// oldestAdded returns the earliest time any of the files was added.
func (c *computation) oldestAdded(files []string) int64 {
	var oldest int64
	for _, file := range files {
		if at, ok := c.Dates.Added[file]; ok && (oldest == 0 || at < oldest) {
			oldest = at
		}
	}
	return oldest
}

// enforcedSince is when both the configuration and the CI job that runs it
// existed: the later of their additions.
func (c *computation) enforcedSince(configs []string, jobs []ci.Config) int64 {
	configAdded, jobAdded := c.oldestAdded(configs), c.oldestAdded(jobPaths(jobs))
	if len(configs) == 0 {
		return jobAdded
	}
	if configAdded == 0 || jobAdded == 0 {
		return 0
	}
	return max(configAdded, jobAdded)
}

func jobPaths(jobs []ci.Config) []string {
	paths := make([]string, 0, len(jobs))
	for _, job := range jobs {
		paths = append(paths, job.Path)
	}
	return paths
}

// Wanted names the files whose content the markers read.
func Wanted(files []gitrepo.File) []string {
	var wanted []string
	for _, file := range files {
		if scope.Skipped(file.Path) {
			continue
		}
		base := path.Base(file.Path)
		switch {
		case ci.Provider(file.Path) != "", ci.IncludeCandidate(file.Path), ci.ActionCandidate(file.Path), inventory.IsInstructionFile(file.Path),
			codeownersFiles[file.Path], commandFiles[file.Path], base == "package.json", base == "project.json", base == "go.mod", base == "pyproject.toml", base == "pom.xml",
			isESLintConfig(base), strings.HasPrefix(base, "tsconfig") && strings.HasSuffix(base, ".json"), gradleBuildFile(file.Path),
			boundaryToolConfig(file.Path):
			wanted = append(wanted, file.Path)
		}
	}
	return wanted
}

// Dated names the files whose history the markers read: when each last
// changed and when it was added.
func Dated(files []gitrepo.File, layout areas.Layout) []string {
	var dated []string
	graphs := map[string]bool{}
	for _, file := range graphManifests(layout) {
		graphs[file] = true
	}
	for _, file := range files {
		if scope.Skipped(file.Path) && !lockfile(file.Path) {
			continue
		}
		base := path.Base(file.Path)
		switch {
		case ci.Provider(file.Path) != "", ci.IncludeCandidate(file.Path), ci.ActionCandidate(file.Path), inventory.IsInstructionFile(file.Path),
			codeownersFiles[file.Path], isReadme(base) && isAreaRoot(layout, file.Path), datedStaticConfig(file.Path), datedMavenConfig(file.Path),
			boundaryConfig(base), lockfile(file.Path), toolchainPins[base], gradleBuildFile(file.Path), javaWrapper(file.Path),
			boundaryToolConfig(file.Path), graphs[file.Path]:
			dated = append(dated, file.Path)
		}
	}
	return dated
}

func isAreaRoot(layout areas.Layout, file string) bool {
	if area := layout.Assign(file); area >= 0 {
		return path.Dir(file) == layout.Areas[area].Path
	}
	return path.Dir(file) == areas.RootPath
}

var areasPath = regexp.MustCompile(`^[A-Za-z0-9._@+-]+(/[A-Za-z0-9._@+-]+)*$`)

func lockfile(file string) bool {
	for _, name := range scope.Lockfiles {
		if path.Base(file) == name {
			return !strings.Contains(file, "node_modules/") && !strings.Contains(file, "vendor/")
		}
	}
	return false
}
