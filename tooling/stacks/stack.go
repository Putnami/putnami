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
	// path, the package. Steps receive it as {component}.
	Name string
	// SourceFile is the workspace-relative manifest the name was read from.
	SourceFile string
	// Root is the workspace-relative stack root: the directory of the build
	// that holds the component. It is "." or Path.
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
}

// Metadata is what the probe records about a project under
// project.metadata[<extension>]. It reaches every task's cache key.
type Metadata struct {
	// Component is the detector's name for the component.
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
	for _, c := range components {
		if c.Root != wsproto.ProbeRootPath && c.Root != c.Path {
			result.Diagnostics = append(result.Diagnostics, diag.Warningf(CodeStackRootUnsupported, c.Path,
				"%s is left out: its stack root %s is neither the workspace root nor its own directory", c.Path, c.Root))
			continue
		}
		expected, err := s.Profile.Toolchain.ExpectedVersion(root, c.Path, c.Root)
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, diag.Warningf(CodeToolchainUnpinned, c.Path, "the expected toolchain version cannot be read: %v", err))
		}
		meta, err := json.Marshal(Metadata{Component: c.Name, Root: c.Root, Toolchain: expected})
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
			Dependencies: slices.Clone(c.Dependencies),
			Extensions:   []string{s.Extension},
			Tags:         []string{s.Profile.ID},
			WatchedFiles: watched,
			Metadata:     meta,
		})
	}
	wsproto.NormalizeProbeResult(&result)
	return result, nil
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
	for _, q := range request.Inputs {
		result.Inputs = append(result.Inputs, s.answer(root, q))
	}
	return result
}

func (s Stack) answer(root string, q wsproto.InputResolveQuery) wsproto.InputResolveAnswer {
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
	// A component that is its own stack root reads its root files in its
	// own directory; for one under the workspace root's build they are the
	// manifest's workspace input.
	if s.ownRoot(root, q.Project) {
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

// ownRoot reports whether the project at projectRel is its own stack root:
// the detector answers so for the project.
func (s Stack) ownRoot(root, projectRel string) bool {
	if projectRel == "." || projectRel == "" {
		return true
	}
	components, _ := s.Detect(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{projectRel}})
	for _, c := range components {
		if c.Path == projectRel {
			return c.Root == c.Path
		}
	}
	return false
}
