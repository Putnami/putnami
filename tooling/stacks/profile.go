// Package stacks is the stack core: the library every stack extension
// (@putnami/rust, @putnami/php, …) builds on. A stack extension is a manifest,
// an embedded stack profile and a call to Main; the core answers the workspace
// probe from the stack's detector, resolves the profile with the project's
// overrides, expands the task input selectors, and runs the lint, test and
// build tasks: the toolchain check, the steps, and the reports.
//
// The design note is doc/01-design.md.
package stacks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Task names: the three tasks every stack extension declares.
const (
	TaskLint  = "lint"
	TaskTest  = "test"
	TaskBuild = "build"
)

// TaskNames are the tasks a profile may declare, in run order.
var TaskNames = []string{TaskLint, TaskTest, TaskBuild}

// Toolchain check modes.
const (
	CheckFail = "fail"
	CheckWarn = "warn"
)

// Coverage report formats the evidence readers read.
var coverageFormats = []string{"lcov", "cobertura", "jacoco", "clover", "gocover"}

// Profile is a stack profile: how one stack's components are linted, tested
// and built. Every path pattern is slash-separated and relative to the project
// directory, or to the stack root when it starts with "{root}/".
type Profile struct {
	// ID is the stack id, such as "rust".
	ID string `json:"id"`
	// Markers are the files that make a directory a candidate project.
	Markers []string `json:"markers"`
	// Toolchain is how the version on the machine is read and compared.
	Toolchain Toolchain `json:"toolchain"`
	// Inputs are named sets of file patterns the tasks read.
	Inputs map[string][]string `json:"inputs"`
	// Tasks are the lint, test and build tasks, by name.
	Tasks map[string]Task `json:"tasks"`
}

// Toolchain is the toolchain check of a profile.
type Toolchain struct {
	// Command reads the version on the machine.
	Command []string `json:"command"`
	// Pattern matches the command's output; its "version" group is the
	// version.
	Pattern string `json:"pattern"`
	// Files state the expected version, nearest first.
	Files []VersionFile `json:"files,omitempty"`
	// Version is the expected version, which outranks the files.
	Version string `json:"version,omitempty"`
	// Check is "fail" (the default) or "warn": what a mismatch does.
	Check string `json:"check,omitempty"`
}

// VersionFile is a file that states the expected toolchain version.
type VersionFile struct {
	// Path is the file name, looked up in the project directory, then in each
	// parent up to the stack root.
	Path string `json:"path"`
	// Key is a dotted key whose string value is the version: a TOML table
	// key ("toolchain.channel") or a JSON object key ("sdk.version"). Without
	// it, the file's first non-empty line is the version.
	Key string `json:"key,omitempty"`
}

// Task is one task of a profile.
type Task struct {
	// Steps are the argument vectors the task runs in order, in the project
	// directory, with no shell.
	Steps [][]string `json:"steps"`
	// Inputs name the input sets the task reads.
	Inputs []string `json:"inputs"`
	// Reports are the reports the task reads back.
	Reports *Reports `json:"reports,omitempty"`
}

// Reports are the report paths of a task.
type Reports struct {
	// JUnit are the JUnit XML report patterns.
	JUnit []string `json:"junit,omitempty"`
	// Coverage are the coverage reports, with their format.
	Coverage []Coverage `json:"coverage,omitempty"`
}

// Coverage is one coverage report pattern set.
type Coverage struct {
	// Format is lcov, cobertura, jacoco, clover or gocover.
	Format string `json:"format"`
	// Paths are the report patterns.
	Paths []string `json:"paths"`
}

// Decode reads a profile with a strict decoder: an unknown member, a repeated
// document or trailing data is an error, and the result is validated.
func Decode(data []byte) (Profile, error) {
	var p Profile
	if err := strictUnmarshal(data, &p); err != nil {
		return Profile{}, err
	}
	if err := p.Validate(); err != nil {
		return Profile{}, err
	}
	return p, nil
}

func strictUnmarshal(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the profile")
	}
	return nil
}

var (
	idPattern       = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	setNamePattern  = regexp.MustCompile(`^[a-z][a-zA-Z0-9-]*$`)
	placeholderExpr = regexp.MustCompile(`\{[^{}]*\}`)
)

// stepPlaceholders are the placeholders a step may name.
var stepPlaceholders = []string{"{root}", "{component}", "{project}", "{output}"}

// Validate reports the first rule the profile breaks.
func (p Profile) Validate() error {
	if !idPattern.MatchString(p.ID) {
		return fmt.Errorf("id: %q is not a lowercase stack id", p.ID)
	}
	if len(p.Markers) == 0 {
		return errors.New("markers: a profile names at least one marker")
	}
	for i, m := range p.Markers {
		if m == "" || strings.Contains(m, "/") || strings.Contains(m, "{") {
			return fmt.Errorf("markers[%d]: %q is not a file name", i, m)
		}
	}
	if err := p.Toolchain.validate(); err != nil {
		return fmt.Errorf("toolchain.%w", err)
	}
	for name, patterns := range p.Inputs {
		if !setNamePattern.MatchString(name) {
			return fmt.Errorf("inputs: %q is not an input set name", name)
		}
		for i, pattern := range patterns {
			if err := validatePattern(pattern, true); err != nil {
				return fmt.Errorf("inputs.%s[%d]: %w", name, i, err)
			}
		}
	}
	if len(p.Tasks) == 0 {
		return errors.New("tasks: a profile declares at least one task")
	}
	for name, task := range p.Tasks {
		if !slices.Contains(TaskNames, name) {
			return fmt.Errorf("tasks: %q is not lint, test or build", name)
		}
		if err := task.validate(p.Inputs); err != nil {
			return fmt.Errorf("tasks.%s.%w", name, err)
		}
	}
	return nil
}

func (t Toolchain) validate() error {
	if len(t.Command) == 0 || t.Command[0] == "" {
		return errors.New("command: the toolchain check names a command")
	}
	re, err := regexp.Compile(t.Pattern)
	if err != nil {
		return fmt.Errorf("pattern: %w", err)
	}
	if re.SubexpIndex("version") < 0 {
		return errors.New("pattern: the pattern has no (?<version>…) group")
	}
	for i, f := range t.Files {
		if f.Path == "" || strings.Contains(f.Path, "/") {
			return fmt.Errorf("files[%d].path: %q is not a file name", i, f.Path)
		}
	}
	switch t.Check {
	case "", CheckFail, CheckWarn:
	default:
		return fmt.Errorf("check: %q is not fail or warn", t.Check)
	}
	return nil
}

func (t Task) validate(sets map[string][]string) error {
	if len(t.Steps) == 0 {
		return errors.New("steps: a task runs at least one step")
	}
	for i, step := range t.Steps {
		if len(step) == 0 || step[0] == "" {
			return fmt.Errorf("steps[%d]: a step names a program", i)
		}
		for _, arg := range step {
			for _, ph := range placeholderExpr.FindAllString(arg, -1) {
				if !slices.Contains(stepPlaceholders, ph) {
					return fmt.Errorf("steps[%d]: %s is no placeholder; use {root}, {component}, {project} or {output}", i, ph)
				}
			}
		}
	}
	for _, name := range t.Inputs {
		if _, ok := sets[name]; !ok {
			return fmt.Errorf("inputs: %q names no input set", name)
		}
	}
	if t.Reports != nil {
		for i, p := range t.Reports.JUnit {
			if err := validateReportPath(p); err != nil {
				return fmt.Errorf("reports.junit[%d]: %w", i, err)
			}
		}
		for i, c := range t.Reports.Coverage {
			if !slices.Contains(coverageFormats, c.Format) {
				return fmt.Errorf("reports.coverage[%d].format: %q is not one of %s", i, c.Format, strings.Join(coverageFormats, ", "))
			}
			if len(c.Paths) == 0 {
				return fmt.Errorf("reports.coverage[%d].paths: a coverage report names a path", i)
			}
			for j, p := range c.Paths {
				if err := validateReportPath(p); err != nil {
					return fmt.Errorf("reports.coverage[%d].paths[%d]: %w", i, j, err)
				}
			}
		}
	}
	return nil
}

// validatePattern checks a slash-separated relative pattern; rooted allows the
// "{root}/" prefix.
func validatePattern(pattern string, rooted bool) error {
	rest := pattern
	if rooted {
		rest = strings.TrimPrefix(pattern, "{root}/")
	}
	if rest == "" || strings.HasPrefix(rest, "/") || strings.HasPrefix(rest, "!") || strings.Contains(rest, "\\") || strings.Contains(rest, "{") {
		return fmt.Errorf("%q is not a relative slash pattern", pattern)
	}
	for _, seg := range strings.Split(rest, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return fmt.Errorf("%q is not a clean relative pattern", pattern)
		}
	}
	return nil
}

// validateReportPath accepts a pattern under the project, under the stack root
// ("{root}/…") or under the task output ("{output}/…").
//
// The core removes what a pattern outside "{output}/" matches before the
// steps, so such a pattern names one place: no "**" and no wildcard in its
// first segment.
func validateReportPath(p string) error {
	if rest, ok := strings.CutPrefix(p, "{output}/"); ok {
		return validatePattern(rest, false)
	}
	if err := validatePattern(p, true); err != nil {
		return err
	}
	segments := strings.Split(strings.TrimPrefix(p, "{root}/"), "/")
	if slices.Contains(segments, "**") || strings.ContainsAny(segments[0], "*?[") {
		return fmt.Errorf("%q: a report outside {output}/ is removed before the steps, so it names no \"**\" and no wildcard first directory", p)
	}
	return nil
}

// Resolve applies a project's override to the profile. The override is a JSON
// object with the profile's shape and every member optional: an object member
// merges, any other value replaces the value at its path whole. An override
// that names a member the profile schema does not know, or that changes id or
// markers, fails.
func (p Profile) Resolve(override json.RawMessage) (Profile, error) {
	trimmed := bytes.TrimSpace(override)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return p, nil
	}
	var over map[string]any
	if err := json.Unmarshal(trimmed, &over); err != nil {
		return Profile{}, fmt.Errorf("the override is not a JSON object: %w", err)
	}
	for _, fixed := range []string{"id", "markers"} {
		if _, ok := over[fixed]; ok {
			return Profile{}, fmt.Errorf("%s: an override cannot change the profile's %s", fixed, fixed)
		}
	}
	base, err := json.Marshal(p)
	if err != nil {
		return Profile{}, err
	}
	var merged map[string]any
	if err := json.Unmarshal(base, &merged); err != nil {
		return Profile{}, err
	}
	mergeInto(merged, over)
	data, err := json.Marshal(merged)
	if err != nil {
		return Profile{}, err
	}
	var out Profile
	if err := strictUnmarshal(data, &out); err != nil {
		return Profile{}, fmt.Errorf("the override does not fit the profile: %w", err)
	}
	if err := out.Validate(); err != nil {
		return Profile{}, fmt.Errorf("the resolved profile is invalid: %w", err)
	}
	return out, nil
}

func mergeInto(dst, src map[string]any) {
	for k, v := range src {
		if sv, ok := v.(map[string]any); ok {
			if dv, ok := dst[k].(map[string]any); ok {
				mergeInto(dv, sv)
				continue
			}
		}
		dst[k] = v
	}
}

// TaskPatterns splits the input patterns of a task: the patterns relative to
// the project directory, and those relative to the stack root, without their
// "{root}/" prefix. Each list holds the task's input sets, in the order it
// names them, then the toolchain files. A task the profile does not declare
// reads nothing.
func (p Profile) TaskPatterns(task string) (project, root []string) {
	t, ok := p.Tasks[task]
	if !ok {
		return nil, nil
	}
	for _, set := range t.Inputs {
		for _, pattern := range p.Inputs[set] {
			if rest, ok := strings.CutPrefix(pattern, "{root}/"); ok {
				root = appendNew(root, rest)
			} else {
				project = appendNew(project, pattern)
			}
		}
	}
	for _, f := range p.Toolchain.Files {
		project = appendNew(project, f.Path)
		root = appendNew(root, f.Path)
	}
	return project, root
}

// RootPatterns are the stack-root patterns of every task: what a stack
// extension's manifest declares as its workspace input.
func (p Profile) RootPatterns() []string {
	var out []string
	for _, task := range TaskNames {
		_, root := p.TaskPatterns(task)
		for _, r := range root {
			out = appendNew(out, r)
		}
	}
	slices.Sort(out)
	return out
}

func appendNew(list []string, s string) []string {
	if slices.Contains(list, s) {
		return list
	}
	return append(list, s)
}

// versionDirs are the directories whose toolchain files state the expected
// version: the project directory, then the stack root.
func versionDirs(projectDir, stackRoot string) []string {
	dir, root := path.Clean(projectDir), path.Clean(stackRoot)
	if dir == root {
		return []string{dir}
	}
	return []string{dir, root}
}
