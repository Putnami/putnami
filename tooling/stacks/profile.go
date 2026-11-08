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
	// Markers are the files that make a directory a candidate project: a
	// file name, or a file one directory down whose directory may be a
	// wildcard, such as "*.xcodeproj/project.pbxproj".
	Markers []string `json:"markers"`
	// Toolchain is how the version on the machine is read and compared.
	Toolchain Toolchain `json:"toolchain"`
	// Inputs are named sets of file patterns the tasks read.
	Inputs map[string][]string `json:"inputs"`
	// Excludes are directory names an input pattern with "**" never walks
	// into: the build outputs and dependency checkouts beside the sources.
	Excludes []string `json:"excludes,omitempty"`
	// Tasks are the lint, test and build tasks, by name.
	Tasks map[string]Task `json:"tasks"`
	// Variants are named parts of the profile, with its shape and every
	// member optional, that the detector picks per component: a stack with
	// several builds, such as Gradle and Maven, declares one per build. A
	// variant merges into the profile like an override. A profile with
	// variants may declare no task of its own.
	Variants map[string]json.RawMessage `json:"variants,omitempty"`
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
	// Provisioned names the environment variable that holds the path of the
	// toolchain the workspace lock pins. A task uses that toolchain when the
	// one on PATH is missing or outside the stated version.
	Provisioned string `json:"provisioned,omitempty"`
	// Home names the environment variable a task sets to the home of the
	// pinned toolchain when it runs it: the parent of the directory that holds
	// its command, such as JAVA_HOME. Build tools read the home before PATH.
	Home string `json:"home,omitempty"`
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
	// Artifacts are the files the task copies into {output}/artifacts after
	// its steps, so a cache hit restores them: patterns under the project or
	// the stack root ("{root}/…") that may name {component}. A pattern that
	// matches no file copies nothing.
	Artifacts []string `json:"artifacts,omitempty"`
	// WithoutCoverage is how the task runs on a toolchain with no coverage
	// support. Without it, such a toolchain fails on its missing report.
	WithoutCoverage *WithoutCoverage `json:"withoutCoverage,omitempty"`
}

// WithoutCoverage replaces a task's steps on a toolchain with no coverage
// support, and leaves its coverage reports unread.
type WithoutCoverage struct {
	// Probe is an argument vector that exits non-zero when the toolchain has
	// no coverage support. It runs in the project directory, with no shell.
	Probe []string `json:"probe"`
	// Steps replace the task's steps.
	Steps [][]string `json:"steps"`
}

// Reports are the report paths of a task.
type Reports struct {
	// When names project file patterns: when none matches a file, the task
	// expects no report, such as a module without tests. Empty, the task
	// always expects its reports.
	When []string `json:"when,omitempty"`
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
	// Optional says a missing report is not read and fails nothing: a build
	// that writes coverage only when it is configured to.
	Optional bool `json:"optional,omitempty"`
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
var stepPlaceholders = []string{"{root}", "{component}", "{project}", "{output}", "{runtime}"}

// Validate reports the first rule the profile breaks.
func (p Profile) Validate() error {
	if !idPattern.MatchString(p.ID) {
		return fmt.Errorf("id: %q is not a lowercase stack id", p.ID)
	}
	if len(p.Markers) == 0 {
		return errors.New("markers: a profile names at least one marker")
	}
	for i, m := range p.Markers {
		if err := validateMarker(m); err != nil {
			return fmt.Errorf("markers[%d]: %w", i, err)
		}
	}
	for i, e := range p.Excludes {
		if e == "" || e == "." || e == ".." || strings.ContainsAny(e, "/\\{*?[") {
			return fmt.Errorf("excludes[%d]: %q is not a directory name", i, e)
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
	if len(p.Tasks) == 0 && len(p.Variants) == 0 {
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
	for _, name := range p.VariantNames() {
		if !idPattern.MatchString(name) {
			return fmt.Errorf("variants: %q is not a lowercase variant name", name)
		}
		if _, err := p.Variant(name); err != nil {
			return err
		}
	}
	return nil
}

// validateMarker accepts a file name, or a file one directory down whose
// directory may be a wildcard: "Package.swift", "*.xcodeproj/project.pbxproj".
func validateMarker(m string) error {
	segments := strings.Split(m, "/")
	if len(segments) > 2 || strings.Contains(m, "{") || strings.Contains(m, "\\") {
		return fmt.Errorf("%q is not a file name, or a file one directory down", m)
	}
	for _, seg := range segments {
		if seg == "" || seg == "." || seg == ".." || seg == "**" {
			return fmt.Errorf("%q is not a file name, or a file one directory down", m)
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
	if t.Provisioned != "" && !environmentName.MatchString(t.Provisioned) {
		return fmt.Errorf("provisioned: %q is not an environment variable name", t.Provisioned)
	}
	if t.Home != "" && (t.Provisioned == "" || !environmentName.MatchString(t.Home)) {
		return fmt.Errorf("home: %q is not an environment variable name beside a provisioned toolchain", t.Home)
	}
	return nil
}

// environmentName matches an environment variable name.
var environmentName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func (t Task) validate(sets map[string][]string) error {
	if err := validateSteps("steps", t.Steps); err != nil {
		return err
	}
	if w := t.WithoutCoverage; w != nil {
		if len(w.Probe) == 0 || w.Probe[0] == "" {
			return errors.New("withoutCoverage.probe: the probe names a program")
		}
		if err := validateSteps("withoutCoverage.steps", w.Steps); err != nil {
			return err
		}
	}
	for _, name := range t.Inputs {
		if _, ok := sets[name]; !ok {
			return fmt.Errorf("inputs: %q names no input set", name)
		}
	}
	for i, a := range t.Artifacts {
		if err := validatePattern(strings.ReplaceAll(a, "{component}", "component"), true); err != nil {
			return fmt.Errorf("artifacts[%d]: %w", i, err)
		}
	}
	if t.Reports != nil {
		for i, p := range t.Reports.When {
			if err := validatePattern(p, false); err != nil {
				return fmt.Errorf("reports.when[%d]: %w", i, err)
			}
		}
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

func validateSteps(field string, steps [][]string) error {
	if len(steps) == 0 {
		return fmt.Errorf("%s: a task runs at least one step", field)
	}
	for i, step := range steps {
		if len(step) == 0 || step[0] == "" {
			return fmt.Errorf("%s[%d]: a step names a program", field, i)
		}
		for _, arg := range step {
			for _, ph := range placeholderExpr.FindAllString(arg, -1) {
				if !slices.Contains(stepPlaceholders, ph) {
					return fmt.Errorf("%s[%d]: %s is no placeholder; use {root}, {component}, {project}, {output} or {runtime}", field, i, ph)
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
	return p.merge(override, "override")
}

// VariantNames are the profile's variants, sorted.
func (p Profile) VariantNames() []string {
	var out []string
	for name := range p.Variants {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// Variant returns the profile with the named variant applied. A profile
// without variants is its own only variant, named "".
func (p Profile) Variant(name string) (Profile, error) {
	if name == "" {
		if len(p.Variants) > 0 {
			return Profile{}, fmt.Errorf("variants: the profile declares %s; a component names one", strings.Join(p.VariantNames(), ", "))
		}
		return p, nil
	}
	raw, ok := p.Variants[name]
	if !ok {
		return Profile{}, fmt.Errorf("variants: the profile declares no variant %q", name)
	}
	base := p
	base.Variants = nil
	out, err := base.merge(raw, "variant")
	if err != nil {
		return Profile{}, fmt.Errorf("variants.%s: %w", name, err)
	}
	return out, nil
}

// Applied returns each variant applied to the profile, in name order, or
// the profile alone when it declares none. Invalid variants are left out:
// Decode refuses them.
func (p Profile) Applied() []Profile {
	if len(p.Variants) == 0 {
		return []Profile{p}
	}
	var out []Profile
	for _, name := range p.VariantNames() {
		if v, err := p.Variant(name); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// HasTask reports whether the profile, or any of its variants, declares a
// task.
func (p Profile) HasTask(task string) bool {
	for _, v := range p.Applied() {
		if _, ok := v.Tasks[task]; ok {
			return true
		}
	}
	return false
}

// ShippedRootPatterns are the stack-root patterns a task reads in any
// variant, sorted: what the manifest declares as the task's workspace input.
func (p Profile) ShippedRootPatterns(task string) []string {
	var out []string
	for _, v := range p.Applied() {
		_, root := v.TaskPatterns(task)
		for _, r := range root {
			out = appendNew(out, r)
		}
	}
	slices.Sort(out)
	return out
}

// merge applies a JSON object with the profile's shape to it; kind names it
// in errors.
func (p Profile) merge(override json.RawMessage, kind string) (Profile, error) {
	trimmed := bytes.TrimSpace(override)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return p, nil
	}
	var over map[string]any
	if err := json.Unmarshal(trimmed, &over); err != nil {
		return Profile{}, fmt.Errorf("the %s is not a JSON object: %w", kind, err)
	}
	for _, fixed := range []string{"id", "markers", "variants"} {
		if _, ok := over[fixed]; ok {
			return Profile{}, fmt.Errorf("%s: the %s cannot change the profile's %s", fixed, kind, fixed)
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
	dropStaleFallbacks(merged, over)
	data, err := json.Marshal(merged)
	if err != nil {
		return Profile{}, err
	}
	var out Profile
	if err := strictUnmarshal(data, &out); err != nil {
		return Profile{}, fmt.Errorf("the %s does not fit the profile: %w", kind, err)
	}
	if err := out.Validate(); err != nil {
		return Profile{}, fmt.Errorf("the resolved profile is invalid: %w", err)
	}
	return out, nil
}

// dropStaleFallbacks removes the withoutCoverage of a task whose steps the
// override replaces: those steps are not the ones the fallback stands for.
func dropStaleFallbacks(merged, over map[string]any) {
	overTasks, _ := over["tasks"].(map[string]any)
	mergedTasks, _ := merged["tasks"].(map[string]any)
	for name, value := range overTasks {
		task, _ := value.(map[string]any)
		_, replaced := task["steps"]
		_, restated := task["withoutCoverage"]
		if target, ok := mergedTasks[name].(map[string]any); ok && replaced && !restated {
			delete(target, "withoutCoverage")
		}
	}
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

// RootPatterns are the stack-root patterns of every task of the profile.
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
