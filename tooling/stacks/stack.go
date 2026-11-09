package stacks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
)

// Component is one component a stack's detector finds.
type Component struct {
	// Path is the workspace-relative component directory, "." for the root.
	Path string
	// Name is the project's source name: the crate, the module, the package.
	// Empty, the project keeps its directory's name, and steps receive the
	// component's Path as {component}.
	Name string
	// Ref is the build's own name for the component, which steps receive as
	// {component}, when it differs from Name: a Gradle task path prefix, a
	// Maven groupId:artifactId. Empty, it is Name.
	Ref string
	// Variant names the profile variant the component's build runs; empty
	// for a profile without variants.
	Variant string
	// InputOnly marks a stack root that holds no component, such as a virtual
	// Cargo workspace: an input-only project whose tasks run no step.
	InputOnly bool
	// SourceFile is the workspace-relative manifest the name was read from.
	SourceFile string
	// Root is the workspace-relative stack root: the directory of the build
	// that holds the component. When it is neither "." nor Path, the probe
	// answers the component with an edge to the project at Root, which
	// carries the root's files.
	Root string
	// Dependencies are the workspace-relative directories of the components
	// this one depends on.
	Dependencies []string
	// EdgeSource is the provenance of every edge the probe answers for the
	// component, its stack-root edge included, such as
	// wsproto.DependencySourceBuildGraph for a delegated stack whose tool's
	// graph states them. The probe reports it to a request that asks for
	// provenance. Empty reports none: the edges stay unattributed.
	EdgeSource wsproto.DependencySource
	// Type is the detector's classification, "library" or "application". The
	// planner gives a library no runnable command. Empty leaves it to the
	// project's own configuration.
	Type string
	// ReleaseMembers are the artifacts this component contributes to a release
	// set.
	ReleaseMembers []releaseset.MemberDeclaration
	// Bare marks a directory whose markers make it a workspace member but
	// that holds no component: the probe answers its name alone, with no
	// extension, tag or metadata, so it runs no task.
	Bare bool
	// Tasks are the profile tasks the component runs; nil runs every task.
	// A task outside it passes with no step run. A component whose list
	// names no profile task is no project.
	Tasks []string
	// Watched are other workspace-relative files the detector read to answer
	// for the component; the probe reports them among its watched files.
	Watched []string
}

// VersionMatcher reports whether the machine's toolchain version satisfies
// the exact expected one, for a project of the workspace at root.
type VersionMatcher func(root string, expected Expected, machine string) bool

// Detector finds the components among a probe request's candidate paths, in
// the workspace at root.
type Detector func(root string, request wsproto.ProbeRequest) ([]Component, []diag.Diagnostic)

// Stack is one stack extension: its name, its profile and its detector.
type Stack struct {
	// Extension is the extension's name, such as "@putnami/rust".
	Extension string
	// Profile is the stack profile the extension ships.
	Profile Profile
	// Detect finds the components.
	Detect Detector
	// CaseComponent names the component of a JUnit case from its class name,
	// else its suite name; ok is false when neither names one. Nil reads
	// every case as the task's component's.
	CaseComponent func(classname, suite string) (component string, ok bool)
	// ComponentKey is a component name as the stack compares two of them.
	// Nil compares names as they are.
	ComponentKey func(name string) string
	// IsComponent reports whether a workspace-relative directory that holds
	// one of the profile's markers is a component, without the detector. Nil
	// asks the detector, which may read the whole repository. The coverage
	// attribution asks it for each such directory a report's files lie in.
	IsComponent func(root, dir string) bool
	// Provide installs the toolchain of the component's resolved profile when
	// the one on PATH is missing or outside the versions its files state, or
	// returns the one an earlier call installed. expected holds the version
	// each of the toolchain's checks expects, in the order of Checks. Nil means
	// the stack installs no toolchain. The core calls it only for a resolved
	// profile that names toolchain.provisioned: a variant that names none
	// keeps its toolchain check only.
	Provide func(ctx context.Context, toolchain Toolchain, expected []Expected, info func(string)) (Provided, error)
	// Tools makes the tools of one task of a component available, beyond the
	// toolchain, and returns the directories to append to its PATH. meta is
	// the component's, so a stack with several builds installs only the tools
	// of the component's. A tool already on the PATH of env needs no
	// directory. Nil means the stack installs no tool.
	Tools func(ctx context.Context, task string, meta Metadata, env []string, info func(string)) ([]string, error)
	// Commands are the extension's own runtime commands, by name: a step
	// runs one as {runtime} <name> <args>. A name is neither a task nor a
	// control call.
	Commands map[string]Command
	// Runtime is the path {runtime} expands to; empty means the running
	// executable.
	Runtime string
	// CrossComponentCoverage counts a coverage file of another component of
	// the workspace as the task's: a stack whose tests live in components of
	// their own, such as a .NET test project, measures the code of the
	// components it references, which run no test of their own.
	CrossComponentCoverage bool
	// MatchVersion, when set, replaces the core's rule for an exact expected
	// version: equal, or a stable release of a shorter expected version.
	MatchVersion VersionMatcher
}

// Command is a runtime command of a stack extension. It runs in the step's
// directory, with the step's environment, and its error fails the step.
type Command func(args []string, stdout, stderr io.Writer) error

// Provided is a toolchain a stack extension installed.
type Provided struct {
	// Executable is the absolute path of the toolchain command.
	Executable string
	// Env holds NAME=VALUE entries the steps of the task also receive.
	Env []string
}

// Metadata is what the probe records about a project under
// project.metadata[<extension>]. It reaches every task's cache key.
type Metadata struct {
	// Component is the build's name for the component, steps' {component};
	// empty for a stack root that holds none.
	Component string `json:"component"`
	// Variant is the profile variant of the component.
	Variant string `json:"variant,omitempty"`
	// Root is the workspace-relative stack root.
	Root string `json:"root"`
	// Toolchain is the expected toolchain version the files state.
	Toolchain Expected `json:"toolchain"`
	// Toolchains are the expected versions of the further checks, in the
	// order of toolchain.also.
	Toolchains []Expected `json:"toolchains,omitempty"`
	// ReleaseSet declares the artifacts the project contributes to a release
	// set. It is absent on a project that publishes none.
	ReleaseSet *releaseset.ProjectMetadata `json:"releaseSet,omitempty"`
	// Manifest is the workspace-relative manifest the detector read the
	// component from; steps receive its absolute path as {manifest}.
	Manifest string `json:"manifest,omitempty"`
	// Tasks are the profile tasks the component runs; absent, every task.
	Tasks []string `json:"tasks,omitempty"`
}

// Stack-root diagnostics.
const (
	CodeProjectConfigInvalid = "stacks.project_config_invalid"
	CodeStackRootUnsupported = "stacks.stack_root_unsupported"
	CodeComponentConflict    = "stacks.component_conflict"
)

// Probe answers a workspace probe: one project per component the detector
// finds among the candidates, with its name, its edges, the extension, and
// its metadata; a bare directory gets its name alone.
func (s Stack) Probe(root string, request wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
	result := wsproto.ProbeResult{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension}
	components, diags := s.Detect(root, request)
	result.Diagnostics = append(result.Diagnostics, diags...)
	components, conflicts := firstPerPath(components)
	for _, c := range conflicts {
		result.Diagnostics = append(result.Diagnostics, diag.Warningf(CodeComponentConflict, c.Path,
			"%s holds two components; the first one the detector names is the project, and %s is left out", c.Path, c.Name))
	}
	deps, leftOut := rootEdges(components)
	for _, c := range components {
		if c.Bare {
			result.Projects = append(result.Projects, wsproto.ProbeProject{Path: c.Path, SourceName: c.Name, SourceFile: c.SourceFile})
			continue
		}
		if reason, out := leftOut[c.Path]; out {
			result.Diagnostics = append(result.Diagnostics, diag.Warningf(CodeStackRootUnsupported, c.Path, "%s is left out: %s", c.Path, reason))
			continue
		}
		profile, err := s.Profile.Variant(c.Variant)
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, diag.Warningf(CodeProjectConfigInvalid, c.Path, "%s is left out: %v", c.Path, err))
			continue
		}
		var expected []Expected
		for _, check := range profile.Toolchain.Checks() {
			e, err := check.ExpectedVersion(root, c.Path, c.Root)
			if err != nil {
				result.Diagnostics = append(result.Diagnostics, diag.Warningf(CodeToolchainUnpinned, c.Path, "the expected toolchain version cannot be read: %v", err))
			}
			expected = append(expected, e)
		}
		// A component whose manifest names no package, such as a Composer
		// application, is named by its directory in the steps: the same on
		// every checkout. Its probe project keeps no source name.
		component := c.Ref
		if component == "" {
			component = c.Name
		}
		if component == "" && !c.InputOnly {
			component = c.Path
		}
		var tasks []string
		if c.Tasks != nil {
			for _, t := range TaskNames {
				if slices.Contains(c.Tasks, t) {
					tasks = append(tasks, t)
				}
			}
			if len(tasks) == 0 {
				result.Diagnostics = append(result.Diagnostics, diag.Warningf(CodeNotAComponent, c.Path,
					"%s is left out: it runs none of the %s tasks", c.Path, s.Profile.ID))
				continue
			}
		}
		metadata := Metadata{Component: component, Variant: c.Variant, Root: c.Root, Toolchain: expected[0], Toolchains: expected[1:], Manifest: c.SourceFile, Tasks: tasks}
		if len(c.ReleaseMembers) > 0 {
			metadata.ReleaseSet = &releaseset.ProjectMetadata{Ecosystems: slices.Clone(c.ReleaseMembers)}
		}
		meta, err := json.Marshal(metadata)
		if err != nil {
			return wsproto.ProbeResult{}, err
		}
		rootFiles := profile.RootPatterns()
		var watched []string
		if c.SourceFile != "" {
			watched = appendNew(watched, c.SourceFile)
		}
		for _, f := range c.Watched {
			watched = appendNew(watched, f)
		}
		for _, m := range s.Profile.Markers {
			if !strings.ContainsAny(m, "*?[") {
				watched = appendNew(watched, joinRel(c.Path, m))
			}
		}
		for _, f := range profile.Toolchain.AllFiles() {
			watched = appendNew(watched, joinRel(c.Path, f.Path))
		}
		for _, f := range rootFiles {
			if !strings.ContainsAny(f, "*?[!") {
				watched = appendNew(watched, joinRel(c.Root, f))
			}
		}
		slices.Sort(watched)
		result.Projects = append(result.Projects, wsproto.ProbeProject{
			Path:              c.Path,
			SourceName:        c.Name,
			SourceFile:        c.SourceFile,
			Type:              c.Type,
			Dependencies:      deps[c.Path],
			DependencySources: edgeSources(request, c.EdgeSource, deps[c.Path]),
			Extensions:        []string{s.Extension},
			Tags:              []string{s.Profile.ID},
			WatchedFiles:      watched,
			Metadata:          meta,
		})
	}
	wsproto.NormalizeProbeResult(&result)
	return result, nil
}

// edgeSources attributes each of a component's edges to its edge source, for
// a request that asks for provenance. A component with no edge answers an
// empty map: it is attributed, and nothing is left to judge.
func edgeSources(request wsproto.ProbeRequest, source wsproto.DependencySource, deps []string) map[string]wsproto.DependencySource {
	if !request.DependencySources || source == "" {
		return nil
	}
	out := make(map[string]wsproto.DependencySource, len(deps))
	for _, dep := range deps {
		out[dep] = source
	}
	return out
}

// firstPerPath keeps the first component the detector names at each path,
// and returns the others.
func firstPerPath(components []Component) (kept, dropped []Component) {
	seen := map[string]bool{}
	for _, c := range components {
		if seen[c.Path] {
			dropped = append(dropped, c)
			continue
		}
		seen[c.Path] = true
		kept = append(kept, c)
	}
	return kept, dropped
}

// rootEdges returns the dependencies the probe answers for each component:
// the detector's, and for a component whose stack root is neither the
// workspace root nor its own directory, the project at that root, whose
// selector answers the root's files. A component whose root is no component
// of the stack, or whose root reaches it in the graph of those edges, where
// the root edge would close a cycle, is left out with the reason; the
// components left out leave the graph until no root reaches its member.
func rootEdges(components []Component) (map[string][]string, map[string]string) {
	byPath := map[string]Component{}
	for _, c := range components {
		byPath[c.Path] = c
	}
	deps := map[string][]string{}
	leftOut := map[string]string{}
	var nested []Component
	for _, c := range components {
		deps[c.Path] = slices.Clone(c.Dependencies)
		if c.Root == wsproto.ProbeRootPath || c.Root == c.Path {
			continue
		}
		if stackRoot, ok := byPath[c.Root]; !ok || stackRoot.Root != stackRoot.Path {
			leftOut[c.Path] = fmt.Sprintf("its stack root %s is no project of the stack; run putnami projects sync", c.Root)
			continue
		}
		deps[c.Path] = appendNew(deps[c.Path], c.Root)
		slices.Sort(deps[c.Path])
		nested = append(nested, c)
	}
	for {
		var cyclic []Component
		for _, c := range nested {
			if _, out := leftOut[c.Path]; !out && reaches(deps, leftOut, c.Root, c.Path) {
				cyclic = append(cyclic, c)
			}
		}
		if len(cyclic) == 0 {
			break
		}
		for _, c := range cyclic {
			leftOut[c.Path] = fmt.Sprintf("its stack root %s depends on it, so the edge that carries the root's files into its key would close a cycle", c.Root)
		}
	}
	for _, c := range components {
		if _, out := leftOut[c.Path]; out {
			delete(deps, c.Path)
		}
	}
	return deps, leftOut
}

// reaches reports whether from reaches to over deps, through no component
// left out.
func reaches(deps map[string][]string, leftOut map[string]string, from, to string) bool {
	seen := map[string]bool{}
	next := []string{from}
	for len(next) > 0 {
		at := next[len(next)-1]
		next = next[:len(next)-1]
		if at == to {
			return true
		}
		if _, out := leftOut[at]; out || seen[at] {
			continue
		}
		seen[at] = true
		next = append(next, deps[at]...)
	}
	return false
}

func joinRel(dir, name string) string {
	if dir == "" || dir == "." {
		return name
	}
	return path.Join(dir, name)
}

// ProjectConfigFile is the project file a project's overrides live in.
const ProjectConfigFile = "putnami.json"

// ProjectOverride reads the override of a project from its own putnami.json:
// options[<extension>]. A project without the file or the block has none.
func (s Stack) ProjectOverride(projectAbs string) (json.RawMessage, error) {
	data, err := os.ReadFile(filepath.Join(projectAbs, ProjectConfigFile)) //nolint:gosec // the project's own config file
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Options map[string]json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", ProjectConfigFile, err)
	}
	return doc.Options[s.Extension], nil
}

// Resolved is the profile of one project: the profile, then its variant,
// then the project's override.
func (s Stack) Resolved(projectAbs, variant string) (Profile, error) {
	base, err := s.Profile.Variant(variant)
	if err != nil {
		return Profile{}, err
	}
	override, err := s.ProjectOverride(projectAbs)
	if err != nil {
		return Profile{}, err
	}
	p, err := base.Resolve(override)
	if err != nil {
		if variant != "" {
			return Profile{}, fmt.Errorf("%s options[%q], applied after the variant %s: %w", ProjectConfigFile, s.Extension, variant, err)
		}
		return Profile{}, fmt.Errorf("%s options[%q]: %w", ProjectConfigFile, s.Extension, err)
	}
	// A stack-root pattern reaches the key only through the manifest's
	// workspace input of its task, which declares the shipped profile's, every
	// variant's: an override may narrow them, never add one. A task keys the
	// host system only when the manifest declares it.
	for _, task := range TaskNames {
		if !s.Profile.HostDependent(task) && p.HostDependent(task) {
			return Profile{}, fmt.Errorf("%s options[%q]: tasks.%s names a target with hosts, but the extension keys %s with no hostPlatform; drop hosts and name the targets the host builds", ProjectConfigFile, s.Extension, task, task)
		}
		shipped := s.Profile.ShippedRootPatterns(task)
		_, resolved := p.TaskPatterns(task)
		for _, r := range resolved {
			if !slices.Contains(shipped, r) {
				return Profile{}, fmt.Errorf("%s options[%q]: {root}/%s is no stack-root input the extension declares for %s (%s); name a file under the project instead", ProjectConfigFile, s.Extension, r, task, strings.Join(shipped, ", "))
			}
		}
	}
	return p, nil
}

// Selector is the input selector of a task: "<stack id>:<task>".
func (s Stack) Selector(task string) string {
	return s.Profile.ID + ":" + task
}

// Selectors are the input selectors a stack extension lists.
func (s Stack) Selectors() []string {
	var out []string
	for _, task := range TaskNames {
		if s.Profile.HasTask(task) {
			out = append(out, s.Selector(task))
		}
	}
	slices.Sort(out)
	return out
}

// ResolveInputs answers an input-resolve request: for each asked selector,
// the project files the resolved profile's task reads. The project's
// putnami.json is in the content basis, so an override that changes the
// inputs changes the answer.
func (s Stack) ResolveInputs(root string, request wsproto.InputResolveRequest) wsproto.InputResolveResult {
	result := wsproto.InputResolveResult{Version: wsproto.InputResolveProtocolVersion, Extension: s.Extension}
	found := s.components(root, request.Inputs)
	for _, q := range request.Inputs {
		result.Inputs = append(result.Inputs, s.answer(root, q, found[q.Project]))
	}
	return result
}

func (s Stack) answer(root string, q wsproto.InputResolveQuery, c found) wsproto.InputResolveAnswer {
	fail := func(format string, args ...any) wsproto.InputResolveAnswer {
		msg := fmt.Sprintf(format, args...)
		if len(msg) > 4000 {
			msg = msg[:4000]
		}
		return wsproto.InputResolveAnswer{Project: q.Project, Selector: q.Selector, Error: msg}
	}
	task, ok := strings.CutPrefix(q.Selector, s.Profile.ID+":")
	if !ok || !slices.Contains(TaskNames, task) {
		return fail("%s answers no selector %s", s.Extension, q.Selector)
	}
	projectAbs := filepath.Join(root, filepath.FromSlash(q.Project))
	if !c.known && len(s.Profile.Variants) > 0 {
		return fail("%s finds no component in %s, so it cannot name its variant: run putnami projects sync", s.Extension, q.Project)
	}
	p, err := s.Resolved(projectAbs, c.variant)
	if err != nil {
		return fail("%v", err)
	}
	projectPatterns, rootPatterns := p.TaskPatterns(task)
	patterns := slices.Clone(projectPatterns)
	// A stack root that holds no component reads its root files alone.
	if c.rootOnly {
		patterns = nil
	}
	// A component that is its own stack root reads its root files in its
	// own directory; for one under the workspace root's build they are the
	// manifest's workspace input, and for one under a nested root the edge
	// to that root's project carries them.
	excludes, kept := p.TaskExcludes(task), p.KeptInputs()
	if c.ownRoot {
		for _, r := range rootPatterns {
			if excluded, ok := strings.CutPrefix(r, "!"); ok {
				excludes = appendNew(excludes, excluded)
			} else {
				patterns = appendNew(patterns, r)
			}
		}
	}
	patterns = appendNew(patterns, ProjectConfigFile)
	var files []string
	for _, pattern := range patterns {
		matches, err := globFiles(projectAbs, pattern, p.Excludes...)
		if err != nil {
			return fail("%v", err)
		}
		for _, m := range matches {
			rel, err := filepath.Rel(projectAbs, m)
			if err != nil {
				continue
			}
			if rel = filepath.ToSlash(rel); slices.Contains(kept, rel) || !Excluded(excludes, rel) {
				files = appendNew(files, rel)
			}
		}
	}
	// The basis leaves the skipped directories out, and its exclusions name
	// any entry of that name: a .git file at a worktree's root, a linked
	// Pods. It carries the input sets' exclusions too, so a write under an
	// excluded directory, such as the report a run writes, does not move its
	// digest; the files no exclusion leaves out then have a set of their own.
	// The answer lists only what its basis selects.
	basis := [][]string{slices.Clone(basisSet(patterns, p.Excludes))}
	if len(excludes) > 0 {
		for _, e := range excludes {
			basis[0] = append(basis[0], "!"+e)
		}
		basis = append(basis, kept)
	}
	files = slices.DeleteFunc(files, func(f string) bool {
		return !slices.ContainsFunc(basis, func(set []string) bool { return wsproto.SelectsPath(f, set) })
	})
	slices.Sort(files)
	return wsproto.InputResolveAnswer{
		Project:  q.Project,
		Selector: q.Selector,
		Files:    files,
		Basis:    wsproto.InputBasis{Content: basis},
	}
}

// basisSet is the basis of an answer: its patterns, without the directories a
// "**" walk skips, so a write under .git, .putnami or an excluded build
// directory neither moves the digest nor races its read. A directory a
// pattern names before its first wildcard is walked, so it stays in.
func basisSet(patterns, excludes []string) []string {
	if !slices.ContainsFunc(patterns, func(p string) bool { return strings.Contains(p, "**") }) {
		return patterns
	}
	out := slices.Clone(patterns)
	for _, dir := range append([]string{".git", ".putnami"}, excludes...) {
		if !slices.ContainsFunc(patterns, func(p string) bool { return literalPrefixNames(p, dir) }) {
			out = append(out, "!**/"+dir+"/**")
		}
	}
	return out
}

// literalPrefixNames reports whether pattern names dir as one of the
// directories before its first wildcard segment.
func literalPrefixNames(pattern, dir string) bool {
	segments := strings.Split(pattern, "/")
	for _, seg := range segments[:len(segments)-1] {
		if strings.ContainsAny(seg, "*?[") {
			return false
		}
		if seg == dir {
			return true
		}
	}
	return false
}

// found is what the detector says about an asked project.
type found struct {
	// known says the detector finds a component there.
	known bool
	// ownRoot says the project is its own stack root; the workspace root
	// always is.
	ownRoot bool
	// rootOnly says the project is a stack root that holds no component.
	rootOnly bool
	// variant is the component's profile variant.
	variant string
}

// components reports, for each asked project, what the detector finds
// there, with one detector run for the whole request.
func (s Stack) components(root string, queries []wsproto.InputResolveQuery) map[string]found {
	out := map[string]found{}
	var paths []string
	for _, q := range queries {
		p := q.Project
		if p == "" {
			p = wsproto.ProbeRootPath
		}
		if p == wsproto.ProbeRootPath {
			out[q.Project] = found{ownRoot: true}
		}
		paths = appendNew(paths, p)
	}
	if len(paths) == 0 {
		return out
	}
	slices.Sort(paths)
	components, _ := s.Detect(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: paths})
	components, _ = firstPerPath(components)
	for _, c := range components {
		f := found{known: true, ownRoot: c.Root == c.Path, rootOnly: c.InputOnly, variant: c.Variant}
		out[c.Path] = f
		if c.Path == wsproto.ProbeRootPath {
			out[""] = f
		}
	}
	return out
}
