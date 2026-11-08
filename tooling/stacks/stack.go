package stacks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
)

// Component is one component a stack's detector finds.
type Component struct {
	// Path is the workspace-relative component directory, "." for the root.
	Path string
	// Name is the detector's name for the component: the crate, the Gradle
	// path, the package. Steps receive it as {component}; an empty name
	// gives the component its Path instead.
	Name string
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
}

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
}

// Metadata is what the probe records about a project under
// project.metadata[<extension>]. It reaches every task's cache key.
type Metadata struct {
	// Component is the detector's name for the component, empty for a stack
	// root that holds none.
	Component string `json:"component"`
	// Root is the workspace-relative stack root.
	Root string `json:"root"`
	// Toolchain is the expected toolchain version the files state.
	Toolchain Expected `json:"toolchain"`
}

// Stack-root diagnostics.
const (
	CodeProjectConfigInvalid = "stacks.project_config_invalid"
	CodeStackRootUnsupported = "stacks.stack_root_unsupported"
)

// Probe answers a workspace probe: one project per component the detector
// finds among the candidates, with its name, its edges, the extension, and
// its metadata.
func (s Stack) Probe(root string, request wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
	result := wsproto.ProbeResult{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension}
	components, diags := s.Detect(root, request)
	result.Diagnostics = append(result.Diagnostics, diags...)
	rootFiles := s.Profile.RootPatterns()
	byPath := map[string]Component{}
	for _, c := range components {
		byPath[c.Path] = c
	}
	for _, c := range components {
		deps := slices.Clone(c.Dependencies)
		// A stack root below the workspace root is a project of its own,
		// whose selector answers the root's files: the edge to it carries
		// them into the component's key.
		if c.Root != wsproto.ProbeRootPath && c.Root != c.Path {
			stackRoot, ok := byPath[c.Root]
			switch {
			case !ok || stackRoot.Root != stackRoot.Path:
				result.Diagnostics = append(result.Diagnostics, diag.Warningf(CodeStackRootUnsupported, c.Path,
					"%s is left out: its stack root %s is no project of the %s stack; run putnami projects sync", c.Path, c.Root, s.Profile.ID))
				continue
			case dependsOn(byPath, c.Root, c.Path):
				result.Diagnostics = append(result.Diagnostics, diag.Warningf(CodeStackRootUnsupported, c.Path,
					"%s is left out: its stack root %s depends on it, so the edge that carries the root's files into its key would close a cycle", c.Path, c.Root))
				continue
			}
			deps = appendNew(deps, c.Root)
			slices.Sort(deps)
		}
		expected, err := s.Profile.Toolchain.ExpectedVersion(root, c.Path, c.Root)
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, diag.Warningf(CodeToolchainUnpinned, c.Path, "the expected toolchain version cannot be read: %v", err))
		}
		// A component whose manifest names no package, such as a Composer
		// application, is named by its directory in the steps: the same on
		// every checkout. Its probe project keeps no source name.
		component := c.Name
		if component == "" && !c.InputOnly {
			component = c.Path
		}
		meta, err := json.Marshal(Metadata{Component: component, Root: c.Root, Toolchain: expected})
		if err != nil {
			return wsproto.ProbeResult{}, err
		}
		var watched []string
		for _, m := range s.Profile.Markers {
			watched = appendNew(watched, joinRel(c.Path, m))
		}
		for _, f := range s.Profile.Toolchain.Files {
			watched = appendNew(watched, joinRel(c.Path, f.Path))
		}
		for _, f := range rootFiles {
			if !strings.ContainsAny(f, "*?[") {
				watched = appendNew(watched, joinRel(c.Root, f))
			}
		}
		slices.Sort(watched)
		result.Projects = append(result.Projects, wsproto.ProbeProject{
			Path:         c.Path,
			SourceName:   c.Name,
			SourceFile:   c.SourceFile,
			Dependencies: deps,
			Extensions:   []string{s.Extension},
			Tags:         []string{s.Profile.ID},
			WatchedFiles: watched,
			Metadata:     meta,
		})
	}
	wsproto.NormalizeProbeResult(&result)
	return result, nil
}

// dependsOn reports whether the component at from reaches the component at
// to over the components' dependencies.
func dependsOn(byPath map[string]Component, from, to string) bool {
	seen := map[string]bool{}
	next := []string{from}
	for len(next) > 0 {
		at := next[len(next)-1]
		next = next[:len(next)-1]
		if at == to {
			return true
		}
		if seen[at] {
			continue
		}
		seen[at] = true
		next = append(next, byPath[at].Dependencies...)
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

// Resolved is the profile of one project, with its override applied.
func (s Stack) Resolved(projectAbs string) (Profile, error) {
	override, err := s.ProjectOverride(projectAbs)
	if err != nil {
		return Profile{}, err
	}
	p, err := s.Profile.Resolve(override)
	if err != nil {
		return Profile{}, fmt.Errorf("%s options[%q]: %w", ProjectConfigFile, s.Extension, err)
	}
	// A stack-root pattern reaches the key only through the manifest's
	// workspace input of its task, which declares the shipped profile's: an
	// override may narrow them, never add one.
	for _, task := range TaskNames {
		_, shipped := s.Profile.TaskPatterns(task)
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
		if _, ok := s.Profile.Tasks[task]; ok {
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
	kinds := s.rootKinds(root, request.Inputs)
	for _, q := range request.Inputs {
		result.Inputs = append(result.Inputs, s.answer(root, q, kinds[q.Project]))
	}
	return result
}

func (s Stack) answer(root string, q wsproto.InputResolveQuery, kind rootKind) wsproto.InputResolveAnswer {
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
	p, err := s.Resolved(projectAbs)
	if err != nil {
		return fail("%v", err)
	}
	projectPatterns, rootPatterns := p.TaskPatterns(task)
	patterns := slices.Clone(projectPatterns)
	// A stack root that holds no component reads its root files alone.
	if kind.rootOnly {
		patterns = nil
	}
	// A component that is its own stack root reads its root files in its
	// own directory; for one under the workspace root's build they are the
	// manifest's workspace input, and for one under a nested root the edge
	// to that root's project carries them.
	if kind.own {
		for _, r := range rootPatterns {
			patterns = appendNew(patterns, r)
		}
	}
	patterns = appendNew(patterns, ProjectConfigFile)
	var files []string
	for _, pattern := range patterns {
		matches, err := globFiles(projectAbs, pattern)
		if err != nil {
			return fail("%v", err)
		}
		for _, m := range matches {
			rel, err := filepath.Rel(projectAbs, m)
			if err != nil {
				continue
			}
			files = appendNew(files, filepath.ToSlash(rel))
		}
	}
	slices.Sort(files)
	return wsproto.InputResolveAnswer{
		Project:  q.Project,
		Selector: q.Selector,
		Files:    files,
		Basis:    wsproto.InputBasis{Content: [][]string{patterns}},
	}
}

// rootKind is what a project is to its stack root.
type rootKind struct {
	// own is true for a project that is its own stack root.
	own bool
	// rootOnly is true for a stack root that holds no component.
	rootOnly bool
}

// rootKinds reports, for each asked project, what it is to its stack root,
// with one detector run for the whole request. The workspace root is always
// its own.
func (s Stack) rootKinds(root string, queries []wsproto.InputResolveQuery) map[string]rootKind {
	out := map[string]rootKind{}
	var paths []string
	for _, q := range queries {
		if q.Project == "." || q.Project == "" {
			out[q.Project] = rootKind{own: true}
			continue
		}
		paths = appendNew(paths, q.Project)
	}
	if len(paths) == 0 {
		return out
	}
	slices.Sort(paths)
	components, _ := s.Detect(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: paths})
	for _, c := range components {
		out[c.Path] = rootKind{own: c.Root == c.Path, rootOnly: c.InputOnly}
	}
	return out
}
