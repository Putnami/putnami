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
	"os"
	"path"
	"path/filepath"
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

// Task directories: where a task's steps run.
const (
	DirProject = "project"
	DirRoot    = "root"
)

// Toolchain check modes.
const (
	CheckFail = "fail"
	CheckWarn = "warn"
)

// Coverage report formats the evidence readers read.
var coverageFormats = []string{"lcov", "cobertura", "jacoco", "clover", "gocover"}

// Profile is a stack profile: how one stack's components are linted, tested
// and built. Every path pattern is slash-separated and relative to the project
// directory, or to the stack root when it starts with "{root}/". An input
// pattern that starts with "!" is an exclusion: "!x" removes what it matches
// from the files the task reads under the project, "!{root}/x" from those it
// reads at the stack root.
type Profile struct {
	// ID is the stack id, such as "rust".
	ID string `json:"id"`
	// Markers are the files that make a directory a candidate project: a
	// file name, or a file one directory down whose directory may be a
	// wildcard, such as "*.xcodeproj/project.pbxproj".
	Markers []string `json:"markers"`
	// Toolchain is how the version on the machine is read and compared.
	Toolchain Toolchain `json:"toolchain"`
	// Inputs are named sets of file patterns the tasks read. A pattern
	// written "!pattern" or "!{root}/pattern" leaves the files it matches
	// out of every task that reads the set.
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
	// Delegated marks a stack whose build tool keeps its own cache: every
	// task of the extension's manifest disables Putnami's cache, and each
	// task result names the stack id in delegatedTo.
	Delegated bool `json:"delegated,omitempty"`
}

// Toolchain is the toolchain check of a profile.
type Toolchain struct {
	// Command reads the version on the machine: the one the build uses.
	Command []string `json:"command"`
	// Fallback, when set, lists the installed versions when Command fails,
	// such as a toolchain that refuses to start outside its pinned version:
	// when none of them satisfies the expected version, the check fails as a
	// mismatch.
	Fallback []string `json:"fallback,omitempty"`
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
	// Ranges names the rules a range a file states follows: "" for Composer's
	// syntax (ADR 0002), "pub-sdk" for a Dart SDK constraint and
	// "pub-flutter" for a Flutter SDK constraint, as pub reads them for a
	// package below language version 3.9 (ADR 0013). Version reads plain.
	Ranges string `json:"ranges,omitempty"`
	// Also are further toolchain checks every task runs, such as the Dart SDK
	// a Flutter SDK bundles. An entry with no check takes this one's.
	Also []Toolchain `json:"also,omitempty"`
}

// Range dialects.
const (
	RangesPubSDK     = "pub-sdk"
	RangesPubFlutter = "pub-flutter"
)

// Checks are the toolchain checks a task runs: this one, then each of Also.
func (t Toolchain) Checks() []Toolchain {
	first := t
	first.Also = nil
	out := []Toolchain{first}
	for _, a := range t.Also {
		if a.Check == "" {
			a.Check = t.Check
		}
		out = append(out, a)
	}
	return out
}

// AllFiles are the version files of every check.
func (t Toolchain) AllFiles() []VersionFile {
	var out []VersionFile
	for _, c := range t.Checks() {
		out = append(out, c.Files...)
	}
	return out
}

// VersionFile is a file that states the expected toolchain version.
type VersionFile struct {
	// Path is the file name, looked up in the project directory, then in each
	// parent up to the stack root.
	Path string `json:"path"`
	// Key is a dotted key whose string value is the version: a TOML table
	// key ("toolchain.channel"), a JSON object key ("sdk.version") or, in a
	// .yaml or .yml file, a YAML mapping key ("environment.sdk"). Without it,
	// the file's first non-empty line is the version.
	Key string `json:"key,omitempty"`
	// Pattern is a regular expression matched against the file's content;
	// its "version" group is the version. Without a key or a pattern, the
	// file's first non-empty line is the version.
	Pattern string `json:"pattern,omitempty"`
}

// Task is one task of a profile.
type Task struct {
	// Steps are the argument vectors the task runs in order, in the
	// directory Dir names, with no shell.
	Steps [][]string `json:"steps"`
	// Inputs name the input sets the task reads.
	Inputs []string `json:"inputs"`
	// Dir is where the steps run: "project" (the default), the project
	// directory, or "root", the stack root.
	Dir string `json:"dir,omitempty"`
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
	// Targets are what a step that names {target} runs on, once each, listed
	// in the project directory when the task runs.
	Targets []Target `json:"targets,omitempty"`
	// ReportSteps run after the steps whatever their outcome: they write the
	// reports a runner that fails on a failed case leaves to a converter.
	ReportSteps [][]string `json:"reportSteps,omitempty"`
}

// Target is one target of a task, or one per file a pattern matches.
type Target struct {
	// Name is the target's name.
	Name string `json:"name,omitempty"`
	// Each is a pattern under the project: each file it matches is a target
	// named by its base name without its extension.
	Each string `json:"each,omitempty"`
	// When are paths under the project that must all exist.
	When []string `json:"when,omitempty"`
	// Hosts are the host systems (linux, darwin, windows) the target is
	// built on; empty for every host.
	Hosts []string `json:"hosts,omitempty"`
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
	// TRX are the Visual Studio TRX report patterns, read with the JUnit
	// reports into one test summary.
	TRX []string `json:"trx,omitempty"`
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
	if len(p.Tasks) == 0 && len(p.Variants) == 0 {
		return Profile{}, errors.New("tasks: a profile declares at least one task")
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
var stepPlaceholders = []string{"{root}", "{component}", "{project}", "{output}", "{manifest}", "{runtime}", TargetPlaceholder}

// TargetPlaceholder is the placeholder of a step that runs once per target.
const TargetPlaceholder = "{target}"

// targetHosts are the host systems a target may name, as Go names them.
var targetHosts = []string{"linux", "darwin", "windows"}

var targetNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

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
	for i, a := range p.Toolchain.Also {
		if len(a.Also) > 0 {
			return fmt.Errorf("toolchain.also[%d].also: a further check names no checks of its own", i)
		}
		if err := a.validate(); err != nil {
			return fmt.Errorf("toolchain.also[%d].%w", i, err)
		}
	}
	for name, patterns := range p.Inputs {
		if !setNamePattern.MatchString(name) {
			return fmt.Errorf("inputs: %q is not an input set name", name)
		}
		for i, pattern := range patterns {
			var err error
			if excluded, ok := strings.CutPrefix(pattern, "!"); ok {
				err = validatePattern(excluded, true)
			} else {
				err = validatePattern(pattern, true)
			}
			if err != nil {
				return fmt.Errorf("inputs.%s[%d]: %w", name, i, err)
			}
		}
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

// HostDependent reports whether a task of the profile or of a variant names
// a target built on some host systems only: its manifest task declares the
// hostPlatform runtime input.
func (p Profile) HostDependent(task string) bool {
	for _, v := range p.Applied() {
		for _, target := range v.Tasks[task].Targets {
			if len(target.Hosts) > 0 {
				return true
			}
		}
	}
	return false
}

func (t Toolchain) validate() error {
	if len(t.Command) == 0 || t.Command[0] == "" {
		return errors.New("command: the toolchain check names a command")
	}
	if len(t.Fallback) > 0 && t.Fallback[0] == "" {
		return errors.New("fallback: a fallback names a program")
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
		if f.Pattern != "" {
			if f.Key != "" {
				return fmt.Errorf("files[%d]: a version file names a key or a pattern, not both", i)
			}
			re, err := regexp.Compile(f.Pattern)
			if err != nil {
				return fmt.Errorf("files[%d].pattern: %w", i, err)
			}
			if re.SubexpIndex("version") < 0 {
				return fmt.Errorf("files[%d].pattern: the pattern has no (?<version>…) group", i)
			}
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
	switch t.Ranges {
	case "", RangesPubSDK, RangesPubFlutter:
	default:
		return fmt.Errorf("ranges: %q is not %s or %s", t.Ranges, RangesPubSDK, RangesPubFlutter)
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
	if len(t.Inputs) == 0 {
		return errors.New("inputs: a task names at least one input set")
	}
	switch t.Dir {
	case "", DirProject, DirRoot:
	default:
		return fmt.Errorf("dir: %q is not project or root", t.Dir)
	}
	for _, name := range t.Inputs {
		if _, ok := sets[name]; !ok {
			return fmt.Errorf("inputs: %q names no input set", name)
		}
	}
	if err := t.validateTargets(sets); err != nil {
		return err
	}
	if len(t.ReportSteps) > 0 {
		if err := validateSteps("reportSteps", t.ReportSteps); err != nil {
			return err
		}
		for i, step := range t.ReportSteps {
			if namesTarget(step) {
				return fmt.Errorf("reportSteps[%d]: a report step names no %s", i, TargetPlaceholder)
			}
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
		for i, p := range t.Reports.TRX {
			if err := validateReportPath(p); err != nil {
				return fmt.Errorf("reports.trx[%d]: %w", i, err)
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

func (t Task) validateTargets(sets map[string][]string) error {
	for i, target := range t.Targets {
		if err := target.validate(); err != nil {
			return fmt.Errorf("targets[%d].%w", i, err)
		}
	}
	if len(t.Targets) == 0 {
		for i, step := range t.Steps {
			if namesTarget(step) {
				return fmt.Errorf("steps[%d]: names %s, but the task declares no targets", i, TargetPlaceholder)
			}
		}
	}
	// A target appears or disappears with the files it needs: they reach the
	// key only as task inputs.
	var patterns []string
	for _, set := range t.Inputs {
		for _, pattern := range sets[set] {
			if !strings.HasPrefix(pattern, "!") && !strings.HasPrefix(pattern, "{root}/") {
				patterns = append(patterns, pattern)
			}
		}
	}
	covered := func(rel string) bool {
		return slices.ContainsFunc(patterns, func(p string) bool { return MatchPattern(p, rel) || strings.HasPrefix(p, rel+"/") })
	}
	for i, target := range t.Targets {
		for _, w := range target.When {
			if !covered(w) {
				return fmt.Errorf("targets[%d].when: %s is in no input of the task", i, w)
			}
		}
		if target.Each != "" && !covered(strings.NewReplacer("*", "x", "?", "x").Replace(target.Each)) {
			return fmt.Errorf("targets[%d].each: %s is in no input of the task", i, target.Each)
		}
	}
	return nil
}

func (t Target) validate() error {
	switch {
	case (t.Name == "") == (t.Each == ""):
		return errors.New("name: a target names either a name or an each pattern")
	case t.Name != "" && !targetNamePattern.MatchString(t.Name):
		return fmt.Errorf("name: %q is not a target name", t.Name)
	case t.Each != "":
		if err := validatePattern(t.Each, false); err != nil {
			return fmt.Errorf("each: %w", err)
		}
	}
	for i, w := range t.When {
		if err := validatePattern(w, false); err != nil || strings.ContainsAny(w, "*?[") {
			return fmt.Errorf("when[%d]: %q is not a plain relative path", i, w)
		}
	}
	for i, h := range t.Hosts {
		if !slices.Contains(targetHosts, h) {
			return fmt.Errorf("hosts[%d]: %q is not one of %s", i, h, strings.Join(targetHosts, ", "))
		}
	}
	return nil
}

// namesTarget reports whether a step names {target}, so it runs once per
// target.
func namesTarget(step []string) bool {
	return slices.ContainsFunc(step, func(a string) bool { return strings.Contains(a, TargetPlaceholder) })
}

// ListTargets lists the targets of a task in the project directory at
// projectAbs, on the host system goos, in the order the task declares them.
func (t Task) ListTargets(projectAbs, goos string) ([]string, error) {
	var out []string
	for _, target := range t.Targets {
		if len(target.Hosts) > 0 && !slices.Contains(target.Hosts, goos) {
			continue
		}
		present := true
		for _, w := range target.When {
			if _, err := os.Stat(filepath.Join(projectAbs, filepath.FromSlash(w))); err != nil {
				present = false
				break
			}
		}
		if !present {
			continue
		}
		if target.Name != "" {
			out = appendNew(out, target.Name)
			continue
		}
		matches, err := globFiles(projectAbs, target.Each)
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			base := filepath.Base(m)
			out = appendNew(out, strings.TrimSuffix(base, filepath.Ext(base)))
		}
	}
	return out, nil
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
					return fmt.Errorf("%s[%d]: %s is no placeholder; use {root}, {component}, {project}, {output}, {manifest}, {runtime} or {target}", field, i, ph)
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
// merges, null removes the value at its path, such as a task, and any other
// value replaces it whole. An override that names a member the profile schema
// does not know, or that changes id, markers or delegated, fails.
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
	// filePatterns is the workspace's own member of an extension's option
	// layer: it says which files select the project, and no profile has it.
	delete(over, "filePatterns")
	// The graph sync's mark is its own member of the layer too.
	delete(over, MarkMember)
	for _, fixed := range []string{"id", "markers", "variants", "delegated"} {
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

// mergeInto merges src into dst: an object merges, null removes the member,
// any other value replaces it.
func mergeInto(dst, src map[string]any) {
	for k, v := range src {
		if v == nil {
			delete(dst, k)
			continue
		}
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
// names them, then the version files of every toolchain check. A project
// exclusion is left to TaskExcludes; a stack-root exclusion "!{root}/p" is in
// the root list as "!p", which the manifest's workspace input carries. A task
// the profile does not declare reads the version files alone, so an override
// that adds it keeps the same stack-root inputs.
func (p Profile) TaskPatterns(task string) (project, root []string) {
	if !slices.Contains(TaskNames, task) {
		return nil, nil
	}
	return p.setPatterns(p.Tasks[task].Inputs)
}

// SelectorPatterns are the patterns a selector reads, split as TaskPatterns
// splits them: a task's input sets, or the one input set a stack exposes as a
// selector of its own (Stack.SelectorSets).
func (p Profile) SelectorPatterns(name string) (project, root []string) {
	if slices.Contains(TaskNames, name) {
		return p.TaskPatterns(name)
	}
	if _, ok := p.Inputs[name]; !ok {
		return nil, nil
	}
	return p.setPatterns([]string{name})
}

func (p Profile) setPatterns(sets []string) (project, root []string) {
	for _, set := range sets {
		for _, pattern := range p.Inputs[set] {
			if excluded, ok := strings.CutPrefix(pattern, "!"); ok {
				if rest, ok := strings.CutPrefix(excluded, "{root}/"); ok {
					root = appendNew(root, "!"+rest)
				}
				continue
			}
			if rest, ok := strings.CutPrefix(pattern, "{root}/"); ok {
				root = appendNew(root, rest)
			} else {
				project = appendNew(project, pattern)
			}
		}
	}
	for _, f := range p.Toolchain.AllFiles() {
		project = appendNew(project, f.Path)
		root = appendNew(root, f.Path)
	}
	return project, root
}

// TaskExcludes are the project patterns, without their "!", whose files a
// task's input sets leave out: a file the build tool writes, such as a
// generated registrant, under a directory the task reads.
func (p Profile) TaskExcludes(task string) []string {
	return p.setExcludes(p.Tasks[task].Inputs)
}

// SelectorExcludes are the exclusions of a selector, as TaskExcludes: those
// of a task's input sets, or of the input set a stack exposes as a selector.
func (p Profile) SelectorExcludes(name string) []string {
	if slices.Contains(TaskNames, name) {
		return p.TaskExcludes(name)
	}
	return p.setExcludes([]string{name})
}

func (p Profile) setExcludes(sets []string) []string {
	var out []string
	for _, set := range sets {
		for _, pattern := range p.Inputs[set] {
			if excluded, ok := strings.CutPrefix(pattern, "!"); ok && !strings.HasPrefix(excluded, "{root}/") {
				out = appendNew(out, excluded)
			}
		}
	}
	return out
}

// KeptInputs are the project files no exclusion leaves out of a task's
// inputs: the project's putnami.json and the toolchain files.
func (p Profile) KeptInputs() []string {
	kept := []string{ProjectConfigFile}
	for _, f := range p.Toolchain.AllFiles() {
		kept = appendNew(kept, f.Path)
	}
	return kept
}

// Excluded reports whether a project-relative slash path matches one of the
// exclusion patterns.
func Excluded(excludes []string, rel string) bool {
	return slices.ContainsFunc(excludes, func(e string) bool { return MatchPattern(e, rel) })
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
