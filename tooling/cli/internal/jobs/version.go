package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	putnamigit "go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// VersionInfo is the JSON structure written to .gen/version.json for each project.
//
// Version is the full deployable identifier (e.g. "0.1.0-abc1234" or
// "0.0.0-abc1234-deadbee" when the working tree is dirty). It matches the
// string used as the docker tag / npm version by the package and publish jobs,
// so deployers can read it as the single source of truth.
type VersionInfo struct {
	Name               string                   `json:"name"`
	Version            string                   `json:"version"`
	Suffix             string                   `json:"suffix,omitempty"`
	SHA                string                   `json:"sha"`
	Branch             string                   `json:"branch"`
	IsDirty            bool                     `json:"isDirty"`
	BuildTime          string                   `json:"buildTime"`
	CapabilityRoot     string                   `json:"capabilityRoot,omitempty"`
	CapabilityPackages []CapabilityPackageStamp `json:"capabilityPackages,omitempty"`
}

// CapabilityPackageStamp is the deterministic, build-time package inventory
// consumed by capability-manifest emitters. Version is the resolved workspace
// project version (never a dependency range or volatile VCS suffix), while
// EvidencePath is workspace-relative. CapabilityManifestPath is relative to the
// workload project root so language emitters can merge a dependency's shipped
// static manifest without embedding a machine-specific workspace path.
//
// The inventory covers WORKSPACE PROJECTS ONLY, and that scope is half of a
// contract rather than an omission. The scheduler reaches a package by walking
// putnami.json dependencies, so a released framework module a workload consumes
// from its language package manager — go.putnami.dev/events required as a module
// rather than checked out as a sibling project — has no project to stamp, no
// source root to bind, and no version core could resolve without parsing a
// provider-owned language manifest.
//
// A capability emitter therefore owns the complementary half: a concrete
// contributor absent from this inventory is resolved against the language
// runtime's own record of the published packages that went into the artifact
// (Go: go.putnami.dev/app/capability_modules.go reads runtime/debug build
// info). A contributor that is in neither is a real workspace misconfiguration
// and stays an error on both sides. Widening this stamp to invent entries for
// external packages would fabricate exactly the source roots and bindings that
// contract exists to keep honest.
//
// SourceBindingUnavailable is true exactly when Git does not manage the
// workspace root — no git program, or no repository — and it means the build
// makes no source claim: SourceBinding is empty and the emitter writes its
// manifest without feature evidence. A binding that failed inside a repository
// is not that state: it leaves SourceBinding empty with the marker unset, which
// every emitter refuses.
type CapabilityPackageStamp struct {
	Package                  string `json:"package"`
	Version                  string `json:"version"`
	EvidencePath             string `json:"evidencePath"`
	SourceRoot               string `json:"sourceRoot"`
	SourceBinding            string `json:"sourceBinding"`
	SourceBindingUnavailable bool   `json:"sourceBindingUnavailable,omitempty"`
	CapabilityManifestPath   string `json:"capabilityManifestPath,omitempty"`
}

// versionStampRelPath is the project-relative, slash-form path of the build
// stamp generateVersionFilesAt writes. store.isVersionStamp recognizes the same
// file when hashing cache-key inputs, and restoreDeclaredCacheHit uses it to
// decide whether a restored declared output replaced the stamp.
const versionStampRelPath = ".gen/version.json"

// versionStampOwnedFields is the closed set of top-level keys the SCHEDULER owns
// in .gen/version.json: exactly the JSON names of VersionInfo, no more and no
// less (TestVersionStampOwnedFieldsMatchVersionInfo derives the same set from the
// struct tags and fails when the two drift).
//
// The stamp is a shared document with more than one writer, so the set has to be
// stated rather than implied by "whatever the last writer marshaled":
//
//   - the TypeScript build-generate task adds `contentHash`, the identity of the
//     built artifact bytes (build.UpdateVersionContentHash). The running app
//     reads it back through getBuildInfo() to namespace its disk cache, so
//     losing it silently changes which cache directory a workload uses.
//   - docker publish overlays a `publish` object (image, digest, channels) so
//     later publish hooks in the same run graph can match the image
//     (extension-sdk/dockerpublish.mergeVersionJSON).
//
// Both merge into the document instead of replacing it. writeVersionStamp does
// the same in the other direction, so the scheduler and the extensions can each
// own their half of one file.
var versionStampOwnedFields = map[string]bool{
	"name":               true,
	"version":            true,
	"suffix":             true,
	"sha":                true,
	"branch":             true,
	"isDirty":            true,
	"buildTime":          true,
	"capabilityRoot":     true,
	"capabilityPackages": true,
}

// BuildRunVersions computes one version per version line of the workspace, from
// git alone (D9). A line git cannot answer for never fails the call.
//
// snapshot is the tree state captured BEFORE any in-run mutation: its
// SHA, branch, dirty flag and ordered suffix are what a version is stamped
// with, and recomputing them after a codegen job would fold putnami's own edits
// into a false "-<dirtyhash>". The line's BASE, in contrast, depends only on
// tags and history, so it is read here.
//
// A line whose git state cannot be read — a shallow clone, an unfetched tag set
// — degrades to 0.0.0 plus the snapshot's suffix rather than taking the run
// down: an ordinary build is not a release, and it must keep working in a
// checkout too thin to name a version. The paths that DO release refuse that
// checkout explicitly, through RequireFullClone.
//
// degraded is not a failure of the call, and a caller must not return it as
// one: every shallow-clone build would then fail. It joins, one per line, why
// a line could not be read — degraded to 0.0.0, or left out when there is no
// snapshot to stamp it with — beside versions that are otherwise complete.
// Without it a 0.0.0 stamp cannot tell a git call that failed from a line that
// is really untagged.
func BuildRunVersions(ws *workspace.Workspace, snapshot *putnamigit.VersionInfo) (versions RunVersions, degraded error) {
	if ws == nil {
		return nil, nil
	}
	versions = make(RunVersions, len(ws.Lines)+1)
	var lineErrs []error
	// An unusable support catalog degrades the bump to the stable reading,
	// the larger one, instead of failing a build that releases nothing.
	stable, catalogErr := workspace.StableChangeTest(ws)
	if catalogErr != nil {
		lineErrs = append(lineErrs, fmt.Errorf("every version line reads each commit as stable: %w", catalogErr))
	}
	for _, line := range runVersionLines(ws) {
		pattern, ok := ws.Lines[line]
		if !ok {
			pattern = wsproto.LineTagPattern(line, nil)
		}
		spec := putnamigit.LineSpec{ScopePath: line, TagPattern: pattern, Stable: stable}
		if line != "" {
			spec.Pathspecs = []string{line}
		}
		info, err := putnamigit.GetVersionInfo(ws.Root, spec)
		if err != nil {
			lineErrs = append(lineErrs, fmt.Errorf("version line %q: %w", line, err))
			if snapshot == nil {
				continue
			}
			info = &putnamigit.VersionInfo{
				Base: "0.0.0", SHA: snapshot.SHA, Branch: snapshot.Branch,
				Suffix: snapshot.Suffix, IsDirty: snapshot.IsDirty, Line: line,
			}
			info.Full = info.Base + "-" + info.Suffix
		} else if snapshot != nil {
			// The tree state the run observed wins over the one read now: a
			// hook or a codegen job may have dirtied the tree since.
			info.SHA, info.Branch, info.Suffix, info.IsDirty =
				snapshot.SHA, snapshot.Branch, snapshot.Suffix, snapshot.IsDirty
			if !info.Tagged {
				info.Full = info.Base + "-" + info.Suffix
			}
		}
		versions[line] = &JobContextVersion{
			Base: info.Base, Full: info.Full, SHA: info.SHA, Branch: info.Branch,
			Tag: info.Tag, Suffix: info.Suffix, Tagged: info.Tagged,
			IsDirty: info.IsDirty, Line: line,
		}
	}
	return versions, errors.Join(lineErrs...)
}

// DegradedVersionLines renders BuildRunVersions's degraded error as one
// single-line reason per version line, so a caller printing each behind a
// prefix keeps that prefix on every line: git's own stderr can span several.
func DegradedVersionLines(degraded error) []string {
	if degraded == nil {
		return nil
	}
	reasons := []error{degraded}
	if joined, ok := degraded.(interface{ Unwrap() []error }); ok {
		reasons = joined.Unwrap()
	}
	lines := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		lines = append(lines, strings.Join(strings.Fields(reason.Error()), " "))
	}
	return lines
}

// runVersionLines is every line a project of this workspace can belong to: the
// declared lines, plus each project's resolved line. The second half matters
// for a project outside every declared line — its Line is "", and without an
// entry for it the project would carry no version at all.
func runVersionLines(ws *workspace.Workspace) []string {
	seen := make(map[string]bool, len(ws.Lines)+1)
	for line := range ws.Lines {
		seen[line] = true
	}
	for _, project := range ws.Projects {
		if project != nil {
			seen[project.Line] = true
		}
	}
	lines := make([]string, 0, len(seen))
	for line := range seen {
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return lines
}

// RequireFullClone refuses a checkout that cannot answer what version a commit
// carries. Publishing from one would stamp a number computed from the fraction
// of history that happened to be fetched.
func RequireFullClone(repoRoot string) error {
	shallow, err := putnamigit.IsShallow(repoRoot)
	if err != nil {
		return err
	}
	if shallow {
		return fmt.Errorf("version and publish need a full clone with tags; run git fetch --unshallow --tags")
	}
	return nil
}

// writeVersionStamp writes the scheduler's identity fields to path without
// destroying top-level fields it does not own.
//
// An owned field the marshal OMITTED (omitempty: an empty `suffix` on a build
// with no git metadata) is REMOVED rather than inherited from the file on disk.
// Carrying it through would leave the previous commit's suffix standing as this
// build's identity — a stale-identity bug of exactly the kind the re-stamp
// exists to prevent, only quieter.
//
// Unowned values are round-tripped through json.Number, so a large integer some
// other writer recorded cannot be re-encoded through float64 and lose precision.
// The result is a map, so json.MarshalIndent emits keys in sorted order: the
// layout is a deterministic function of the content alone, never of which writer
// touched the file last. That matters because the document is a cache-key input
// wherever a task's globs reach it, and it is the shape dockerpublish already
// writes.
//
// Against store.versionStampDigest, which is how the file reaches a cache key:
// that digest re-marshals the document as a MAP without its build time and its
// commit fields, so it is already key-order independent and the reordering here
// cannot move it.
// What the merge does change is that a carried field now contributes to the
// digest instead of being deleted on the next scheduler write — which is correct
// (it is real content) and strictly MORE stable than before, because the lossy
// write made a value flip between present and absent depending on which writer
// touched the file last. A project whose .gen starts empty still settles in one
// run — the restore introduces the field after that run's keys were computed —
// but that is the same one-time settle a real generate execution always had, not
// a loop: the next run seeds, keys and re-stamps on the identical document.
func writeVersionStamp(path string, info VersionInfo) error {
	owned, err := json.Marshal(info)
	if err != nil {
		return err
	}
	next := map[string]any{}
	if err := json.Unmarshal(owned, &next); err != nil {
		return err
	}

	// A file that is absent, unreadable, torn, or not a JSON object contributes
	// nothing: there is no unowned content to preserve, and the scheduler's own
	// fields must land either way.
	for key, value := range readVersionStampDocument(path) {
		if versionStampOwnedFields[key] {
			continue
		}
		next[key] = value
	}

	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// readVersionStampDocument decodes the stamp as a raw JSON object, or nil when
// there is nothing usable to preserve. UseNumber keeps numeric literals in their
// source form so re-encoding them is byte-exact.
func readVersionStampDocument(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var doc map[string]any
	if decoder.Decode(&doc) != nil {
		return nil
	}
	return doc
}

// preserveMatchingVersionFiles seeds version metadata before cache-key
// computation without changing a cache-restored version.json merely because a
// new invocation has a later build time. An all-hit run should retain the
// metadata of the build that produced its cached artifacts; the scheduler
// forces a refresh before the first real execution for a project.
func preserveMatchingVersionFiles(
	ws *workspace.Workspace,
	planned []*ScheduledJob,
	versions RunVersions,
	buildTime string,
) {
	generateVersionFilesAt(ws, planned, versions, buildTime, true)
}

func generateVersionFilesAt(
	ws *workspace.Workspace,
	planned []*ScheduledJob,
	versions RunVersions,
	buildTime string,
	preserveMatching bool,
) {
	_ = generateVersionFilesAtWithSourceBindings(
		ws, planned, versions, buildTime, preserveMatching, newCapabilitySourceBindingMemo(),
	)
}

// generateVersionFilesAtWithSourceBindings performs the same deterministic
// stamp update while reusing the supplied session memo. It returns only the Git
// processes physically spawned by memo misses during this call; the count is
// observational and cannot influence stamp bytes.
func generateVersionFilesAtWithSourceBindings(
	ws *workspace.Workspace,
	planned []*ScheduledJob,
	versions RunVersions,
	buildTime string,
	preserveMatching bool,
	sourceBindings *capabilitySourceBindingMemo,
) int {
	seen := make(map[string]bool)
	if sourceBindings == nil {
		sourceBindings = newCapabilitySourceBindingMemo()
	}
	spawnedProcesses := 0

	for _, job := range planned {
		proj := job.Project
		if seen[proj.Name] {
			continue
		}
		seen[proj.Name] = true

		projVersion := VersionInfoForProject(versions, proj)

		info := VersionInfo{
			Name:               proj.Name,
			BuildTime:          buildTime,
			CapabilityRoot:     capabilityRoot(ws, proj),
			CapabilityPackages: capabilityPackageStamps(ws, versions, proj, sourceBindings, &spawnedProcesses),
		}
		if projVersion != nil {
			info.Version = projVersion.Full
			info.Suffix = projVersion.Suffix
			info.SHA = projVersion.SHA
			info.Branch = projVersion.Branch
			info.IsDirty = projVersion.IsDirty
		} else {
			// No git metadata available — the line's version is unknown, and
			// the stamp says so rather than inventing a number.
			info.Version = "0.0.0"
		}

		projRoot := filepath.Join(ws.Root, proj.Path)
		if _, err := os.Stat(projRoot); err != nil {
			continue
		}
		versionPath := filepath.Join(projRoot, filepath.FromSlash(versionStampRelPath))
		if err := os.MkdirAll(filepath.Dir(versionPath), 0o755); err != nil {
			continue
		}
		if preserveMatching && versionFileMatchesBuild(versionPath, info) {
			continue
		}

		// Best-effort write: errors are non-fatal since jobs can still
		// proceed without version metadata.
		writeVersionStamp(versionPath, info) //nolint:errcheck // best-effort by contract
	}
	return spawnedProcesses
}

// versionFileMatchesBuild compares every build identity field except buildTime.
// buildTime belongs to the artifact-producing invocation, so a matching cached
// file is intentionally preserved until this project has an actual cache miss.
//
// It decodes into VersionInfo, so a field the scheduler does not own is INVISIBLE
// to it: a stamp carrying another writer's `contentHash` still matches, and is
// therefore preserved rather than rewritten. That is the intended reading — the
// question this asks is "is the scheduler's half of the document already correct",
// and the answer cannot depend on a key the scheduler neither writes nor reads.
func versionFileMatchesBuild(path string, want VersionInfo) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var got VersionInfo
	if json.Unmarshal(data, &got) != nil {
		return false
	}
	got.BuildTime = want.BuildTime
	return reflect.DeepEqual(got, want)
}

func capabilityRoot(ws *workspace.Workspace, project *workspace.Project) string {
	if ws == nil || project == nil {
		return "."
	}
	rel, err := filepath.Rel(filepath.Join(ws.Root, project.Path), ws.Root)
	if err != nil {
		return "."
	}
	return filepath.ToSlash(rel)
}

type capabilitySourceBindingResult struct {
	binding string
	err     error
	// unmanaged is true when the binding is absent because Git does not manage
	// the repository root. It is the only failure stamped as unavailable.
	unmanaged bool
}

type capabilitySourceBindingMemo struct {
	results map[string]capabilitySourceBindingResult
	// The scheduler's versionFilesMu serializes every map and invalidation.
	snapshots map[string]*putnamigit.SourceBindingSnapshot
	resolve   func(repoRoot, projectRoot string) (binding string, spawnedProcesses int, err error)
	// unmanaged answers, per repository root, whether Git manages it. It is
	// asked once, after the first binding failure, and holds for the run: a
	// root does not gain or lose its repository between two tasks. The
	// scheduler's memo asks the run's cache manager, which every execution key
	// reads too (newScheduler); a memo without one asks Git.
	unmanaged   map[string]bool
	isUnmanaged func(repoRoot string) bool
}

func newCapabilitySourceBindingMemo() *capabilitySourceBindingMemo {
	memo := &capabilitySourceBindingMemo{
		results:     make(map[string]capabilitySourceBindingResult),
		snapshots:   make(map[string]*putnamigit.SourceBindingSnapshot),
		unmanaged:   make(map[string]bool),
		isUnmanaged: func(repoRoot string) bool { return putnamigit.Unmanaged(repoRoot) != nil },
	}
	memo.resolve = func(repoRoot, projectRoot string) (string, int, error) {
		key := filepath.Clean(repoRoot)
		snapshot := memo.snapshots[key]
		spawnedProcesses := 0
		if snapshot == nil {
			var metrics putnamigit.SourceBindingMetrics
			var err error
			snapshot, metrics, err = putnamigit.ReadSourceBindingSnapshot(repoRoot)
			spawnedProcesses += metrics.SpawnedProcesses
			if err != nil {
				return "", spawnedProcesses, err
			}
			memo.snapshots[key] = snapshot
		}
		binding, metrics, err := snapshot.ProjectSourceBindingMeasured(projectRoot)
		return binding, spawnedProcesses + metrics.SpawnedProcesses, err
	}
	return memo
}

func (memo *capabilitySourceBindingMemo) sourceBinding(
	repoRoot, projectRoot string,
) (capabilitySourceBindingResult, int) {
	key := filepath.Clean(projectRoot)
	if result, ok := memo.results[key]; ok {
		return result, 0
	}
	result := capabilitySourceBindingResult{}
	repoKey := filepath.Clean(repoRoot)
	if memo.unmanaged[repoKey] {
		result.unmanaged = true
		memo.results[key] = result
		return result, 0
	}
	var spawnedProcesses int
	result.binding, spawnedProcesses, result.err = memo.resolve(repoRoot, projectRoot)
	if result.err != nil {
		unmanaged, known := memo.unmanaged[repoKey]
		if !known {
			unmanaged = memo.isUnmanaged(repoRoot)
			memo.unmanaged[repoKey] = unmanaged
			spawnedProcesses++
		}
		result.binding, result.unmanaged = "", unmanaged
	}
	memo.results[key] = result
	return result, spawnedProcesses
}

func (memo *capabilitySourceBindingMemo) invalidate(projectRoot string) {
	if memo == nil {
		return
	}
	delete(memo.results, filepath.Clean(projectRoot))
	// Enumeration can change without any previously known file changing (new
	// source, gitignore or index updates). Preserve unrelated computed bindings,
	// but make the next miss enumerate again before reading this project's bytes.
	clear(memo.snapshots)
}

func (memo *capabilitySourceBindingMemo) invalidateAll() {
	if memo == nil {
		return
	}
	clear(memo.results)
	clear(memo.snapshots)
}

func (s *Scheduler) invalidateCapabilitySourceBindingsForProject(job *ScheduledJob) {
	if job == nil || job.Project == nil || s.ws == nil {
		return
	}
	s.versionFilesMu.Lock()
	s.capabilitySourceBindings.invalidate(filepath.Join(s.ws.Root, job.Project.Path))
	s.versionFilesMu.Unlock()
}

// invalidateCapabilitySourceBindingsForRestore brackets materialization of a
// cached entry. Project-rooted outputs outside .gen can change source-v1;
// workspace-rooted outputs may intersect any project (including a root project),
// so they conservatively invalidate the whole memo. Command and invocation
// outputs live outside project source roots. Calling this both before and after
// materialization also clears a value computed concurrently with the write.
func (s *Scheduler) invalidateCapabilitySourceBindingsForRestore(job *ScheduledJob, entry *store.TaskEntry) {
	if job == nil || job.Project == nil || entry == nil {
		return
	}
	invalidateProject := false
	invalidateAll := false
	for _, output := range entry.Outputs {
		if !output.Present() {
			continue
		}
		switch output.Root {
		case extension.OutputRootWorkspace:
			invalidateAll = true
		case extension.OutputRootProject:
			clean := path.Clean(output.Path)
			if clean != ".gen" && !strings.HasPrefix(clean, ".gen/") {
				invalidateProject = true
			}
		}
	}
	if !invalidateProject && !invalidateAll {
		return
	}
	s.versionFilesMu.Lock()
	defer s.versionFilesMu.Unlock()
	if invalidateAll {
		s.capabilitySourceBindings.invalidateAll()
		return
	}
	if s.ws != nil {
		s.capabilitySourceBindings.invalidate(filepath.Join(s.ws.Root, job.Project.Path))
	}
}

func capabilityPackageStamps(
	ws *workspace.Workspace,
	versions RunVersions,
	project *workspace.Project,
	sourceBindings *capabilitySourceBindingMemo,
	spawnedProcesses *int,
) []CapabilityPackageStamp {
	if project == nil {
		return nil
	}
	byName := make(map[string]*workspace.Project)
	if ws != nil {
		for _, candidate := range ws.Projects {
			if candidate != nil && candidate.Name != "" {
				byName[candidate.Name] = candidate
			}
		}
	}
	byName[project.Name] = project

	seen := make(map[string]bool)
	queue := []string{project.Name}
	var projects []*workspace.Project
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		candidate := byName[name]
		if candidate == nil {
			// A declared dependency that is not a workspace project is a
			// published package: it has no project path to bind and no
			// workspace version to resolve. Emitting a partial entry would
			// hand the emitter an unbindable owner, so leave it out and let the
			// emitter resolve it from the artifact's own package graph
			// (CapabilityPackageStamp).
			continue
		}
		projects = append(projects, candidate)
		deps := append([]string(nil), candidate.Dependencies...)
		sort.Strings(deps)
		queue = append(queue, deps...)
	}

	projectRoot := project.Path
	if ws != nil {
		projectRoot = filepath.Join(ws.Root, project.Path)
	}
	stamps := make([]CapabilityPackageStamp, 0, len(projects))
	for _, candidate := range projects {
		version := LineBaseVersion(versions, candidate)
		if version == "" {
			version = "0.0.0"
		}
		evidence := capabilityPackageEvidence(ws, candidate)
		stamp := CapabilityPackageStamp{
			Package:      candidate.Name,
			Version:      version,
			EvidencePath: filepath.ToSlash(evidence),
			SourceRoot:   filepath.ToSlash(candidate.Path),
		}
		repoRoot, candidateRoot := candidate.Path, candidate.Path
		if ws != nil {
			repoRoot, candidateRoot = ws.Root, filepath.Join(ws.Root, candidate.Path)
		}
		result, spawned := sourceBindings.sourceBinding(repoRoot, candidateRoot)
		if spawnedProcesses != nil {
			*spawnedProcesses += spawned
		}
		stamp.SourceBinding = result.binding
		stamp.SourceBindingUnavailable = result.unmanaged
		if candidate.Name != project.Name {
			dependencyManifest := filepath.Join(candidate.Path, "schema", "capabilities.json")
			if ws != nil {
				dependencyManifest = filepath.Join(ws.Root, dependencyManifest)
			}
			if rel, err := filepath.Rel(projectRoot, dependencyManifest); err == nil {
				stamp.CapabilityManifestPath = filepath.ToSlash(rel)
			}
		}
		stamps = append(stamps, stamp)
	}
	sort.Slice(stamps, func(i, j int) bool { return stamps[i].Package < stamps[j].Package })
	return stamps
}

func capabilityPackageEvidence(ws *workspace.Workspace, project *workspace.Project) string {
	root := project.Path
	if ws != nil {
		root = filepath.Join(ws.Root, project.Path)
	}
	// Workspace discovery already parsed this exact descriptor and assigned
	// project.Name from it. Core deliberately does not parse provider-owned
	// language manifests.
	if _, err := os.Stat(filepath.Join(root, "putnami.json")); err == nil {
		return filepath.Join(project.Path, "putnami.json")
	}
	return ""
}
