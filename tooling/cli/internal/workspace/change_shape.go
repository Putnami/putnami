package workspace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/git"
)

// ChangeShapeRecordType is the session event type that records one --impacted
// selection's change shape (ChangeShape.Record).
const ChangeShapeRecordType = "selection:change-shape"

// ChangeCategory is one kind of changed file in a change-size report.
type ChangeCategory string

// The four categories a change is split into, in report order.
const (
	// ChangeCode is authored source and configuration: every file no other
	// category claims.
	ChangeCode ChangeCategory = "code"
	// ChangeTests is test sources and their fixtures.
	ChangeTests ChangeCategory = "tests"
	// ChangeDocs is prose: Markdown and other markup, and doc/ or docs/ trees.
	ChangeDocs ChangeCategory = "docs"
	// ChangeGenerated is what a tool wrote: lockfiles, generated clients and
	// sources, files that declare themselves generated, and the agent files
	// Putnami manages.
	ChangeGenerated ChangeCategory = "generated"
)

// ChangeCategories lists every category in report order.
var ChangeCategories = []ChangeCategory{ChangeCode, ChangeTests, ChangeDocs, ChangeGenerated}

// ChangeSize counts the files of a change and the lines they add and delete.
// A binary file counts as a file with no lines.
type ChangeSize struct {
	Files   int `json:"files"`
	Added   int `json:"added"`
	Deleted int `json:"deleted"`
}

func (s *ChangeSize) add(count git.LineCount) {
	s.Files++
	s.Added += count.Added
	s.Deleted += count.Deleted
}

// ChangeShape describes the size and spread of an --impacted diff. It is
// information for the reader of a run: nothing in it passes or fails the run.
type ChangeShape struct {
	// Baseline is the ref the diff was measured against.
	Baseline string
	// Total counts every changed file.
	Total ChangeSize
	// Categories splits Total by ChangeCategory; an absent category is empty.
	Categories map[ChangeCategory]ChangeSize
	// Areas groups the projects whose authored code the change touches. Two
	// projects share an area when one depends on the other, directly or
	// transitively. Tests, docs and generated files follow the code they sit
	// beside and never open an area of their own. Project names, in workspace
	// order.
	Areas [][]string
	// OnEpic is true when the checkout works on a configured epic branch
	// (git.OnEpicBranch): the change declares that it spans several areas.
	// It is looked up only when there are several Areas.
	OnEpic bool
}

// ChangeShapeOptions carries what the workspace model cannot see by itself.
type ChangeShapeOptions struct {
	// Managed are the workspace-relative slash paths Putnami installed and
	// owns, such as the agent files of an extension's agent content. They
	// count as generated.
	Managed map[string]bool
	// Quiet leaves the size line out of Notes; the warning stays.
	Quiet bool
	// PlanOnly marks a run that plans and executes nothing: it records no
	// session and prints no notes, so ChangeShapeEvidence measures nothing.
	PlanOnly bool
}

// ChangeShapeEvidence is ChangeShape as a run records and prints it: the
// session event, and the notes to print after the run. Both are empty when
// nothing changed, the run only plans, or git cannot count the diff, which
// leaves the run as it was: the shape selects nothing and fails nothing.
func (s ImpactedSelection) ChangeShapeEvidence(ws *Workspace, opts ChangeShapeOptions) ([]jobmodel.SessionRecord, string) {
	if opts.PlanOnly {
		return nil, ""
	}
	shape, err := s.ChangeShape(ws, opts)
	if err != nil || shape.Total.Files == 0 {
		return nil, ""
	}
	event := jobmodel.SessionRecord{Type: ChangeShapeRecordType, Data: shape.Record().EventData()}
	return []jobmodel.SessionRecord{event}, shape.Notes(opts.Quiet)
}

// Notes renders the size line, unless quiet, and the mixed-intent warning
// when it applies, one per line with a trailing newline; "" when neither.
func (c ChangeShape) Notes(quiet bool) string {
	var notes strings.Builder
	if line := c.SizeExplanation(); line != "" && !quiet {
		notes.WriteString(line + "\n")
	}
	if line := c.MixedIntentWarning(); line != "" {
		notes.WriteString(line + "\n")
	}
	return notes.String()
}

// ChangeShape measures the selection's diff: line counts from git, one
// category per changed file, and the areas its authored code spans. It reads
// the head of each changed file to find a generated-file marker. It returns
// an error only when git cannot count the lines; the run it describes is not
// affected either way.
func (s ImpactedSelection) ChangeShape(ws *Workspace, opts ChangeShapeOptions) (ChangeShape, error) {
	shape := ChangeShape{Baseline: s.Baseline, Categories: map[ChangeCategory]ChangeSize{}}
	if ws == nil || len(s.ChangedFiles) == 0 {
		return shape, nil
	}
	counts, err := git.DiffLineCounts(ws.Root, s.DiffBase)
	if err != nil {
		return shape, err
	}

	dirs := projectDirs(ws)
	codeProjects := map[string]bool{}
	for _, file := range s.ChangedFiles {
		slash := filepath.ToSlash(file)
		absolute := filepath.Join(ws.Root, filepath.FromSlash(slash))
		count := counts[file]
		if count.Untracked {
			count = countUntrackedLines(absolute)
		}
		category := classifyChangedFile(slash, absolute, opts.Managed)
		size := shape.Categories[category]
		size.add(count)
		shape.Categories[category] = size
		shape.Total.add(count)
		if category == ChangeCode {
			if id := owningProject(dirs, slash); id != "" {
				codeProjects[id] = true
			}
		}
	}

	shape.Areas = changeAreas(ws, codeProjects)
	if len(shape.Areas) > 1 && ws.Config != nil {
		shape.OnEpic = git.OnEpicBranch(ws.Root, ws.Config.EpicBranches)
	}
	return shape, nil
}

// MixedIntent reports whether the change spans several unrelated areas
// without a declared epic.
func (c ChangeShape) MixedIntent() bool {
	return len(c.Areas) > 1 && !c.OnEpic
}

// SizeExplanation renders the change size as one line for a human notice
// stream, or "" when nothing changed.
func (c ChangeShape) SizeExplanation() string {
	if c.Total.Files == 0 {
		return ""
	}
	parts := make([]string, 0, len(ChangeCategories))
	for _, category := range ChangeCategories {
		size, ok := c.Categories[category]
		if !ok || size.Files == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s", category, formatChangeSize(size)))
	}
	return fmt.Sprintf("  Change size against %s: %s (%s)", c.Baseline, formatChangeSize(c.Total), strings.Join(parts, " · "))
}

// changeAreasShown and changeAreaProjectsShown bound the mixed-intent warning:
// the areas it names, and the projects it names per area.
const (
	changeAreasShown        = 4
	changeAreaProjectsShown = 3
)

// MixedIntentWarning renders the mixed-intent warning as one line, or "" when
// the change keeps to one area or declares an epic.
func (c ChangeShape) MixedIntentWarning() string {
	if !c.MixedIntent() {
		return ""
	}
	shown := c.Areas
	if len(shown) > changeAreasShown {
		shown = shown[:changeAreasShown]
	}
	names := make([]string, 0, len(shown)+1)
	for _, area := range shown {
		names = append(names, elideList(area, changeAreaProjectsShown))
	}
	if extra := len(c.Areas) - len(shown); extra > 0 {
		names = append(names, fmt.Sprintf("+%d more", extra))
	}
	return fmt.Sprintf("  warning: this change touches %d unrelated areas (no dependency links them): %s. "+
		"Keep one intent per pull request, or deliver the work on an epic branch (workspace epicBranches), one gated phase per commit.",
		len(c.Areas), strings.Join(names, " | "))
}

// ChangeShapeRecord is a ChangeShape as the data of one ChangeShapeRecordType
// session event.
type ChangeShapeRecord struct {
	Baseline    string                `json:"baseline"`
	Total       ChangeSize            `json:"total"`
	Categories  map[string]ChangeSize `json:"categories"`
	Areas       [][]string            `json:"areas"`
	OnEpic      bool                  `json:"onEpic"`
	MixedIntent bool                  `json:"mixedIntent"`
}

// Record is the shape with every category present, so a reader can tell an
// empty category from an unrecorded one.
func (c ChangeShape) Record() ChangeShapeRecord {
	record := ChangeShapeRecord{
		Baseline:    c.Baseline,
		Total:       c.Total,
		Categories:  make(map[string]ChangeSize, len(ChangeCategories)),
		Areas:       c.Areas,
		OnEpic:      c.OnEpic,
		MixedIntent: c.MixedIntent(),
	}
	if record.Areas == nil {
		record.Areas = [][]string{}
	}
	for _, category := range ChangeCategories {
		record.Categories[string(category)] = c.Categories[category]
	}
	return record
}

// EventData is the record as a session event's data.
func (r ChangeShapeRecord) EventData() (data SessionEventData) {
	// Strings, integers and booleans only: the round trip cannot fail.
	encoded, _ := json.Marshal(r)
	_ = json.Unmarshal(encoded, &data)
	return data
}

func formatChangeSize(size ChangeSize) string {
	noun := "files"
	if size.Files == 1 {
		noun = "file"
	}
	return fmt.Sprintf("%d %s +%d -%d", size.Files, noun, size.Added, size.Deleted)
}

func elideList(items []string, shown int) string {
	if len(items) <= shown {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s (+%d)", strings.Join(items[:shown], ", "), len(items)-shown)
}

// projectDirs indexes each project by its directory, slash form, "." for a
// project at the workspace root.
func projectDirs(ws *Workspace) map[string]string {
	dirs := make(map[string]string, len(ws.Projects))
	for _, p := range ws.Projects {
		dirs[path.Clean(filepath.ToSlash(p.Path))] = p.ID
	}
	return dirs
}

// owningProject returns the project whose directory is nearest above the
// file, or "" when none holds it. Where a file lives is what makes it a
// project's code: a declared asset or input elsewhere selects the other
// project without making the file its own.
func owningProject(dirs map[string]string, slash string) string {
	for dir := path.Dir(slash); ; dir = path.Dir(dir) {
		if id, ok := dirs[dir]; ok {
			return id
		}
		if dir == "." || dir == "/" {
			return ""
		}
	}
}

// changeAreas groups the given project IDs into areas: connected sets under
// the relation "depends on, directly or transitively", in either direction.
func changeAreas(ws *Workspace, ids map[string]bool) [][]string {
	var members []*Project
	for _, p := range ws.Projects {
		if ids[p.ID] {
			members = append(members, p)
		}
	}
	if len(members) == 0 {
		return nil
	}
	parent := make([]int, len(members))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	index := make(map[string]int, len(members))
	for i, p := range members {
		index[p.ID] = i
	}
	for i, p := range members {
		for id := range dependencyClosure(ws, p) {
			if j, ok := index[id]; ok {
				if a, b := find(i), find(j); a != b {
					parent[a] = b
				}
			}
		}
	}
	groups := map[int][]string{}
	var order []int
	for i, p := range members {
		root := find(i)
		if _, seen := groups[root]; !seen {
			order = append(order, root)
		}
		name := p.Name
		if name == "" {
			name = p.ID
		}
		groups[root] = append(groups[root], name)
	}
	areas := make([][]string, 0, len(order))
	for _, root := range order {
		areas = append(areas, groups[root])
	}
	return areas
}

// dependencyClosure is the set of project IDs p depends on, directly or
// transitively, over the declared and provider-derived edges. The implicit
// edge an activated scope adds orders the schedule and relates nothing.
func dependencyClosure(ws *Workspace, p *Project) map[string]bool {
	seen := map[string]bool{}
	if ws.Graph == nil {
		return seen
	}
	stack := []string{p.ID}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, dep := range ws.Graph.DependenciesOf(current) {
			if seen[dep] || ws.Graph.ImplicitScopeEdge(dep, current) {
				continue
			}
			seen[dep] = true
			stack = append(stack, dep)
		}
	}
	return seen
}

// lockfileSuffixes end the names package managers and Putnami give their
// lockfiles and checksum files (a `.lock`, `-lock.yaml` or `.lock.json`
// lockfile, a `.sum` checksum list). Suffixes, not names: the workspace model
// names no package manager.
var lockfileSuffixes = []string{".lock", ".lockb", ".lock.json", ".lock.yaml", "-lock.json", "-lock.yaml", ".sum"}

// generatedManifestNames are manifests a Putnami generator writes.
var generatedManifestNames = map[string]bool{
	"client.putnami.json": true,
}

// testDirectoryNames and docDirectoryNames are the path segments that put a
// file in the tests or docs category whatever its name.
var (
	testDirectoryNames = map[string]bool{"test": true, "tests": true, "testdata": true, "__tests__": true, "__snapshots__": true, "fixtures": true}
	docDirectoryNames  = map[string]bool{"doc": true, "docs": true}
	docExtensions      = map[string]bool{".md": true, ".mdx": true, ".rst": true, ".adoc": true}
)

// classifyChangedFile puts one changed file in its category. Generated wins
// over tests and docs: a generated test fixture is still generated.
func classifyChangedFile(slash, absolute string, managed map[string]bool) ChangeCategory {
	if managed[slash] || isGeneratedPath(slash) || carriesGeneratedHeader(absolute) {
		return ChangeGenerated
	}
	if isTestPath(slash) {
		return ChangeTests
	}
	if isDocPath(slash) {
		return ChangeDocs
	}
	return ChangeCode
}

func isGeneratedPath(slash string) bool {
	for _, segment := range strings.Split(path.Dir(slash), "/") {
		if segment == ".gen" || segment == "__generated__" {
			return true
		}
	}
	base := strings.ToLower(path.Base(slash))
	if generatedManifestNames[base] {
		return true
	}
	for _, suffix := range lockfileSuffixes {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	stem := strings.TrimSuffix(base, path.Ext(base))
	return strings.HasSuffix(stem, ".gen") || strings.HasSuffix(stem, ".pb") || strings.HasSuffix(stem, "_pb2")
}

func isTestPath(slash string) bool {
	for _, segment := range strings.Split(path.Dir(slash), "/") {
		if testDirectoryNames[segment] {
			return true
		}
	}
	base := path.Base(slash)
	stem := strings.TrimSuffix(base, path.Ext(base))
	switch {
	case strings.HasSuffix(stem, "_test"), strings.HasSuffix(stem, ".test"), strings.HasSuffix(stem, ".spec"):
		return true
	case strings.HasSuffix(base, ".py") && (strings.HasPrefix(base, "test_") || base == "conftest.py"):
		return true
	}
	return false
}

func isDocPath(slash string) bool {
	if docExtensions[strings.ToLower(path.Ext(slash))] {
		return true
	}
	for _, segment := range strings.Split(path.Dir(slash), "/") {
		if docDirectoryNames[segment] {
			return true
		}
	}
	return false
}

// generatedHeaderBytes and generatedHeaderLines bound the header read for a
// generated-file marker.
// Go places its marker anywhere above the package clause, so the bounds
// leave room for a license block.
const (
	generatedHeaderBytes = 4096
	generatedHeaderLines = 40
)

// generatedHeaderCommentPrefixes open a comment line in the languages a
// workspace holds. The header scan stops at the first line that is neither
// blank nor a comment: a marker below code is prose about generation, not a
// declaration.
var generatedHeaderCommentPrefixes = []string{"//", "#", "/*", "*", "<!--", "--", ";", `"""`, "'''"}

// goGeneratedLine is Go's generated-file marker, matched exactly as the Go
// convention states it (https://go.dev/s/generatedcode).
var goGeneratedLine = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)

// carriesGeneratedHeader reports whether a file declares itself generated:
// in a Go file, Go's "// Code generated … DO NOT EDIT." line anywhere above
// the package clause, so a license block may come first; or, in the leading
// comment lines, an @generated tag or a comment that opens with "Generated
// by" or "Auto-generated by", as Putnami's client, CODEOWNERS and contract
// generators write.
func carriesGeneratedHeader(absolute string) bool {
	file, err := os.Open(absolute)
	if err != nil {
		return false
	}
	defer file.Close()
	head := make([]byte, generatedHeaderBytes)
	n, _ := io.ReadFull(file, head)
	lines := strings.SplitN(string(head[:n]), "\n", generatedHeaderLines+1)
	if len(lines) > generatedHeaderLines {
		lines = lines[:generatedHeaderLines]
	}
	goSource := strings.HasSuffix(absolute, ".go")
	leading := true
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if goSource && strings.HasPrefix(trimmed, "package ") {
			return false
		}
		if goSource && goGeneratedLine.MatchString(strings.TrimRight(line, "\r")) {
			return true
		}
		if !leading || trimmed == "" {
			continue
		}
		if !hasAnyPrefix(trimmed, generatedHeaderCommentPrefixes) {
			leading = false
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.Contains(lower, "@generated") ||
			(strings.Contains(lower, "code generated") && strings.Contains(lower, "do not edit")) {
			return true
		}
		text := strings.TrimLeft(lower, " \t#/*!<-;\"'")
		if strings.HasPrefix(text, "generated by ") || strings.HasPrefix(text, "auto-generated by ") {
			return true
		}
	}
	return false
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// untrackedLinesLimit bounds the untracked file a line count reads; a larger
// one counts as a file with no lines, as a binary file does.
const untrackedLinesLimit = 8 << 20

// countUntrackedLines counts the lines an untracked file adds. Git reports no
// numstat for it, so the file is read the way git would count it.
func countUntrackedLines(absolute string) git.LineCount {
	info, err := os.Stat(absolute)
	if err != nil || !info.Mode().IsRegular() || info.Size() > untrackedLinesLimit {
		return git.LineCount{Binary: err == nil}
	}
	content, err := os.ReadFile(absolute)
	if err != nil {
		return git.LineCount{}
	}
	probe := content
	if len(probe) > 8000 {
		probe = probe[:8000]
	}
	if bytes.IndexByte(probe, 0) >= 0 {
		return git.LineCount{Binary: true}
	}
	lines := bytes.Count(content, []byte{'\n'})
	if len(content) > 0 && content[len(content)-1] != '\n' {
		lines++
	}
	return git.LineCount{Added: lines}
}
