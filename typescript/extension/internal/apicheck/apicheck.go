// Package apicheck is the `validate` task that holds the breaking-change
// marker: a stable TypeScript package's public API may break only in a release
// whose commits declare it.
//
// For a project the root putnami.support.json lists as stable, or for every
// project of a workspace with no catalog, the task compares what the entry
// points of the project's package.json export in the working tree with what
// they exported at the last release of the project's version line. An incompatible
// change fails the task unless a commit since that tag that touches the
// project declares a breaking change, with "!" or a "BREAKING CHANGE:" footer:
// that marker is what makes the version bump advance the line past a feature
// release.
//
// The catalog is read exactly as @putnami/go's check reads it
// (go/extension/internal/jobs/apicheck): support.go is that check's own, and
// the two must keep one reading. The last release is read by the reader the
// version bump and that check use (linetag.LastRelease). The marker is read by the
// parser the version bump uses (protocolcli.ParseConventionalCommit).
package apicheck

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	supportproto "go.putnami.dev/protocol/support"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/linetag"
	"go.putnami.dev/typescript/extension/internal/apisurface"
	"go.putnami.dev/typescript/extension/internal/toolchain"
)

// Code is the diagnostic code of an incompatible API change.
const Code = "api-compat"

// TypeChangeCode is the diagnostic code of a change of written type, which
// the check reports as a warning and never fails on.
const TypeChangeCode = "api-compat-type-change"

// typeChangeHint is what a warning says about a change of written type.
const typeChangeHint = "a change of written type does not fail the check; " +
	`if importers must change their code, declare it with "!" or a BREAKING CHANGE: footer`

// NotComparedCode is the diagnostic code of the warning a check gives when it
// cannot compare: the checkout lacks what it needs, or the released package
// cannot be read.
const NotComparedCode = "api-compat-not-compared"

// phase is the phase the task reports.
const phase = "api-compat"

// markerHint is how a failure says to resolve it.
const markerHint = `declare the breaking change with "!" or a BREAKING CHANGE: footer`

// manifestMaxBytes bounds the package.json the check reads at a tag.
const manifestMaxBytes = 1 << 20

// resolveBun finds the bun that runs the reader; a test replaces it.
var resolveBun = toolchain.ResolveBun

// Report is the answer of one check.
type Report struct {
	// Skip says why the project is not checked; empty when it is.
	Skip string
	// Note says why nothing was compared, when nothing was and the project
	// has no released API yet.
	Note string
	// Warning says why nothing was compared when the check cannot read what it
	// needs: no repository, a shallow clone, no tag of the line while the
	// repository has other tags, or a released package it cannot read.
	Warning string
	// Tag is the line tag the API was compared with.
	Tag string
	// EntryPoints is the number of subpaths the package declared at Tag.
	EntryPoints int
	// Compared is the number of those subpaths whose exports were compared.
	Compared int
	// MovedFrom is the directory, relative to the repository root, that held
	// the package at Tag, when it is not the project's.
	MovedFrom string
	// NotCompared says, for each subpath whose exports were not compared, why.
	NotCompared []string
	// Changes are the changes since Tag that break an importer whatever the
	// types are: they fail the check without a breaking marker.
	Changes []apisurface.Change
	// TypeChanges are the changes of written type since Tag: they are
	// reported and never fail the check.
	TypeChanges []apisurface.Change
	// Breaking reports whether a commit since Tag that touches the package
	// declares a breaking change. The report names no commit: a branch and its
	// squash merge that declare the same break with different commits give
	// the same verdict.
	Breaking bool
}

// Run is the task body: it checks the project and reports each incompatible
// change as an error diagnostic, or as information when a commit declares the
// break.
func Run(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	emit.PhaseStart(phase)
	report, err := Check(ctx.WorkspaceRoot, ctx.Project.FullPath, ctx.Project.Name, ctx.Project.Tags)
	if err != nil {
		emit.PhaseEnd(phase, "failed")
		return "FAILED", nil, fmt.Errorf("check the API of %s: %w", ctx.Project.Name, err)
	}
	return render(emit, ctx.WorkspaceRoot, ctx.Project.FullPath, report)
}

// Check compares the public API of the package in projectDir with its line's
// last release. name is the project's resolved name: its support-catalog
// identity, and the name its entry points are reported under. tags are the
// project's resolved tags as the job context carries them, nil when it carries
// none: they decide whether the support catalog lists the project as a
// protocol or as a package.
func Check(workspaceRoot, projectDir, name string, tags []string) (*Report, error) {
	skip, err := notChecked(workspaceRoot, name, tags)
	if err != nil {
		return nil, protocolcli.Classify(err, protocolcli.ErrInvalidConfig)
	}
	if skip != "" {
		return &Report{Skip: skip}, nil
	}

	state, err := readRepoState(projectDir)
	if errors.Is(err, errNotARepository) {
		return &Report{Warning: "the workspace is not in a git repository, so it has no tags to compare with " +
			"and the API is not compared; run the check in a git checkout"}, nil
	}
	if err != nil {
		return nil, err
	}
	if !state.hasHead {
		return &Report{Note: "the repository has no commit: there is no tagged API to compare with"}, nil
	}
	pattern := linePattern(workspaceRoot, projectDir)
	if state.shallow {
		return &Report{Warning: fmt.Sprintf("the clone is shallow, so the tags and history of line %s are incomplete "+
			"and the API is not compared; fetch the full history to run the check", pattern)}, nil
	}
	release, ok, err := linetag.LastRelease(projectDir, pattern)
	if err != nil {
		return nil, err
	}
	tag := release.Tag
	if !ok {
		tagged, err := hasTags(projectDir)
		if err != nil {
			return nil, err
		}
		if tagged {
			return &Report{Warning: fmt.Sprintf("no release tag of line %s is reachable from HEAD, though the repository has other tags, "+
				"so the API is not compared; fetch the line's tags if it has released. A tag with a pre-release "+
				"or any other text in place of MAJOR.MINOR.PATCH is not a release", pattern)}, nil
		}
		return &Report{Note: fmt.Sprintf("no release tag of line %s is reachable from HEAD: there is no released API to compare with", pattern)}, nil
	}

	_, statErr := os.Stat(filepath.Join(workspaceRoot, supportproto.CatalogFilename))
	released, moved, report, err := releasedSide(projectDir, tag, name, statErr == nil)
	if err != nil || report != nil {
		return report, err
	}
	if len(released.Entries) == 0 {
		return &Report{Warning: fmt.Sprintf("the %s at %s declares no entry point: no \"exports\", \"main\", \"module\" or \"types\" names a file, "+
			"so the package has no API the check reads and nothing is compared", apisurface.ManifestFilename, tag)}, nil
	}
	current, err := currentSide(projectDir)
	if err != nil {
		return nil, err
	}
	bun, err := resolveBun()
	if err != nil {
		return nil, err
	}
	surfaces, err := apisurface.Read(bun, projectDir, *released, current)
	if errors.Is(err, apisurface.ErrNoTypeScript) {
		return nil, fmt.Errorf("%w; the check reads the API with its parser: run `putnami install`", err)
	}
	if err != nil {
		return nil, err
	}
	old, now := surfaces[0], surfaces[1]
	if len(now.Errors) > 0 {
		first := now.Errors[0]
		return nil, fmt.Errorf("%s:%d does not parse (%s): the API of the working tree cannot be read", first.File, first.Line, first.Message)
	}
	if len(old.Errors) > 0 {
		// No marker could make the released sources parse: compare nothing
		// rather than fail every run until the next tag.
		first := old.Errors[0]
		return &Report{Warning: fmt.Sprintf("%s at %s does not parse with the installed TypeScript (line %d: %s), "+
			"so the API is not compared; update the typescript package to read it", first.File, tag, first.Line, first.Message)}, nil
	}

	comparison := apisurface.Compare(name, old, now)
	report = &Report{Tag: tag, EntryPoints: comparison.Subpaths, Compared: comparison.Compared, MovedFrom: moved.dir,
		NotCompared: notCompared(name, tag, comparison.Skipped)}
	for _, change := range comparison.Changes {
		if change.Warning {
			report.TypeChanges = append(report.TypeChanges, change)
		} else {
			report.Changes = append(report.Changes, change)
		}
	}
	if len(report.Changes) == 0 && len(report.TypeChanges) == 0 {
		return report, nil
	}
	// The commits that touch the package: its directory, and the one that held
	// it at the tag when it moved since.
	commits, err := commitsSince(projectDir, tag, ".")
	if err != nil {
		return nil, err
	}
	if moved.dir != "" {
		before, err := commitsSince(moved.top, tag, moved.dir)
		if err != nil {
			return nil, err
		}
		commits = append(commits, before...)
	}
	report.Breaking = slices.ContainsFunc(commits, func(c commit) bool {
		parsed, ok := protocolcli.ParseConventionalCommit(c.subject, c.body)
		return ok && parsed.Breaking
	})
	return report, nil
}

// movedPackage locates a package whose directory moved since the tag: the
// repository root, and the slash path under it that held the package. Both are
// empty for a package that did not move.
type movedPackage struct {
	top string
	dir string
}

// releasedSide reads the package as tag holds it, and returns the directory
// that held it when that is not the project's. It returns a report instead
// when there is nothing to compare: the tag holds no package.json of the
// package, or one the check cannot read. A package.json the tag holds and the
// working tree does not is compared: every entry point it declared is gone.
//
// A package whose directory moved since the tag is found by its name: the one
// package.json of the tag that declares it. listed says a support catalog
// lists the package as stable; such a package with no baseline is a warning,
// because it may have released from a place the check did not find.
func releasedSide(projectDir, tag, name string, listed bool) (*apisurface.Side, movedPackage, *Report, error) {
	root, prefix := projectDir, ""
	var moved movedPackage
	entries, err := treeAtTag(projectDir, tag, ".", treeListing{recursive: true})
	if err != nil {
		return nil, movedPackage{}, nil, err
	}
	manifest := findManifest(entries)
	if manifest == nil {
		top, dirs, err := packageAtTag(projectDir, tag, name)
		if err != nil {
			return nil, movedPackage{}, nil, err
		}
		if len(dirs) > 1 {
			// The check does not guess which one released.
			return nil, movedPackage{}, &Report{Warning: fmt.Sprintf("the tag %s holds %d %s files that declare %s (%s), "+
				"so the check cannot tell which one released and nothing is compared",
				tag, len(dirs), apisurface.ManifestFilename, name, strings.Join(dirs, ", "))}, nil
		}
		if len(dirs) == 0 {
			if listed {
				return nil, movedPackage{}, &Report{Warning: fmt.Sprintf("%s is stable and the tag %s holds no %s that declares it, in this directory or another, "+
					"so it has no released API to compare with and nothing is compared", name, tag, apisurface.ManifestFilename)}, nil
			}
			return nil, movedPackage{}, &Report{Note: fmt.Sprintf("the project has no %s at %s: there is no released API to compare with",
				apisurface.ManifestFilename, tag)}, nil
		}
		dir := dirs[0]
		root, moved = top, movedPackage{top: top, dir: dir}
		if dir != "." {
			prefix = dir + "/"
		}
		if entries, err = treeAtTag(top, tag, dir, treeListing{recursive: true}); err != nil {
			return nil, movedPackage{}, nil, err
		}
		for i := range entries {
			entries[i].name = strings.TrimPrefix(entries[i].name, prefix)
		}
		manifest = findManifest(entries)
	}
	unreadable := func(reason string) (*apisurface.Side, movedPackage, *Report, error) {
		return nil, movedPackage{}, &Report{Warning: fmt.Sprintf("the project's %s at %s %s, so the API is not compared",
			apisurface.ManifestFilename, tag, reason)}, nil
	}
	if manifest == nil || !manifest.regular {
		return unreadable("is not a regular file")
	}
	sized, err := treeAtTag(root, tag, prefix+apisurface.ManifestFilename, treeListing{sized: true})
	if err != nil {
		return nil, movedPackage{}, nil, err
	}
	if len(sized) == 1 && sized[0].size > manifestMaxBytes {
		return unreadable(fmt.Sprintf("is %d bytes, over the limit of %d", sized[0].size, manifestMaxBytes))
	}
	contents, err := readBlobs(root, []string{manifest.object})
	if err != nil {
		return nil, movedPackage{}, nil, err
	}
	files, err := filesAtTag(root, entries)
	if err != nil {
		return nil, movedPackage{}, nil, err
	}
	points, err := apisurface.EntryPoints(contents[0], paths(files))
	if err != nil {
		return unreadable(err.Error())
	}
	return &apisurface.Side{Files: files, Entries: points, Imports: apisurface.ImportAliases(contents[0])}, moved, nil, nil
}

// findManifest returns the package.json at the top of a listing.
func findManifest(entries []treeEntry) *treeEntry {
	for i := range entries {
		if entries[i].name == apisurface.ManifestFilename {
			return &entries[i]
		}
	}
	return nil
}

// packageAtTag finds the directories of tag's tree whose package.json
// declares the package name, for a package whose directory moved since the
// tag. It returns the repository root and the slash paths of the directories
// under it, in tree order.
func packageAtTag(projectDir, tag, name string) (top string, found []string, err error) {
	output, err := gitOutput(projectDir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", nil, err
	}
	top = strings.TrimSpace(output)
	entries, err := treeAtTag(top, tag, ".", treeListing{recursive: true})
	if err != nil {
		return "", nil, err
	}
	var dirs, objects []string
	for _, entry := range entries {
		if !entry.regular || path.Base(entry.name) != apisurface.ManifestFilename ||
			slices.Contains(strings.Split(entry.name, "/"), "node_modules") {
			continue
		}
		dirs = append(dirs, path.Dir(entry.name))
		objects = append(objects, entry.object)
	}
	if len(objects) == 0 {
		return top, nil, nil
	}
	contents, err := readBlobs(top, objects)
	if err != nil {
		return "", nil, err
	}
	for i, content := range contents {
		var document struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(content, &document) == nil && document.Name == name {
			found = append(found, dirs[i])
		}
	}
	return top, found, nil
}

// currentSide reads the package in the working tree. A package.json that is
// gone declares no entry point; one that does not parse is an error, because
// the fix is in the tree.
func currentSide(projectDir string) (apisurface.Side, error) {
	files, err := filesInTree(projectDir)
	if err != nil {
		return apisurface.Side{}, err
	}
	side := apisurface.Side{Files: files}
	manifest, err := os.ReadFile(filepath.Join(projectDir, apisurface.ManifestFilename)) //nolint:gosec // the project directory plus one fixed filename
	if errors.Is(err, fs.ErrNotExist) {
		return side, nil
	}
	if err != nil {
		return apisurface.Side{}, fmt.Errorf("read %s: %w", apisurface.ManifestFilename, err)
	}
	if side.Entries, err = apisurface.EntryPoints(manifest, paths(files)); err != nil {
		return apisurface.Side{}, protocolcli.Classify(fmt.Errorf("%s %w", apisurface.ManifestFilename, err), protocolcli.ErrInvalidConfig)
	}
	side.Imports = apisurface.ImportAliases(manifest)
	return side, nil
}

func paths(files []apisurface.File) []string {
	names := make([]string, len(files))
	for i, file := range files {
		names[i] = file.Path
	}
	return names
}

// notCompared says why the exports of each skipped subpath were not compared:
// a condition names a source file the project does not hold, such as build
// output that is not committed; the release did not hold its target; or the
// released target is not an ES module.
func notCompared(name, tag string, skipped []apisurface.Skipped) []string {
	reasons := make([]string, 0, len(skipped))
	for _, skip := range skipped {
		pkg := apisurface.PackageName(name, skip.Subpath)
		switch skip.Reason {
		case apisurface.MissingNow:
			reasons = append(reasons, fmt.Sprintf("%s: the entry point names %s, which the project does not hold, "+
				"so its exports are not compared; name a source file git tracks", pkg, skip.Target))
		case apisurface.MissingThen:
			reasons = append(reasons, fmt.Sprintf("%s: the entry point named %s at %s, which the tag does not hold, "+
				"so its exports are not compared", pkg, skip.Target, tag))
		case apisurface.Script:
			reasons = append(reasons, fmt.Sprintf("%s: %s at %s has no import and no export, or assigns its exports with `export =`, "+
				"so it is a script or a CommonJS module and its exports are not compared; the check reads ES modules", pkg, skip.Target, tag))
		}
	}
	return reasons
}

// render reports a check on the event stream and returns the task's status
// and result data.
func render(emit *jsonl.Emitter, workspaceRoot, projectDir string, report *Report) (string, map[string]any, error) {
	switch {
	case report.Skip != "":
		emit.Info(report.Skip)
		emit.PhaseEnd(phase, "skipped")
		return "SKIP", nil, nil
	case report.Warning != "":
		emit.DiagnosticWithCode("warning", report.Warning, "", 0, 0, NotComparedCode)
		emit.PhaseEnd(phase, "success")
		return "OK", map[string]any{"compared": false}, nil
	case report.Note != "":
		emit.Info(report.Note)
		emit.PhaseEnd(phase, "success")
		return "OK", map[string]any{"compared": false}, nil
	}

	data := map[string]any{
		"compared":            true,
		"tag":                 report.Tag,
		"entryPoints":         report.EntryPoints,
		"comparedEntryPoints": report.Compared,
		"incompatible":        len(report.Changes),
		"notCompared":         len(report.NotCompared),
	}
	manifest, _ := location(workspaceRoot, projectDir, apisurface.ManifestFilename, 0)
	for _, reason := range report.NotCompared {
		emit.DiagnosticWithCode("warning", reason, manifest, 0, 0, NotComparedCode)
	}
	if report.MovedFrom != "" {
		emit.Info(fmt.Sprintf("the package was in %s at %s: the API is compared with that directory", report.MovedFrom, report.Tag))
	}
	data["typeChanges"] = len(report.TypeChanges)
	declared := fmt.Sprintf("a commit since %s declares the breaking change", report.Tag)
	if report.Breaking {
		data["breakingDeclared"] = true
	}
	// A change of written type never fails: a warning, or information once a
	// commit declares the release breaking.
	for _, change := range report.TypeChanges {
		file, line := location(workspaceRoot, projectDir, change.File, change.Line)
		if report.Breaking {
			emit.DiagnosticWithCode("info", fmt.Sprintf("%s: %s since %s; %s",
				change.Package, change.Message, report.Tag, declared), file, line, 0, TypeChangeCode)
			continue
		}
		emit.DiagnosticWithCode("warning", fmt.Sprintf("%s: %s since %s; %s",
			change.Package, change.Message, report.Tag, typeChangeHint), file, line, 0, TypeChangeCode)
	}
	typed := ""
	if n := len(report.TypeChanges); n > 0 {
		typed = fmt.Sprintf("; %d change%s of written type %s reported and %s not fail the check", n, plural(n), isOrAre(n), doesOrDo(n))
	}

	if len(report.Changes) == 0 {
		others := report.EntryPoints - report.Compared
		switch {
		case report.Compared == 0:
			// Nothing was compared: do not say what holds.
			data["compared"] = false
			emit.Info(fmt.Sprintf("the %d entry point%s declared at %s %s still declared; none has exports the check reads, so no API is compared",
				report.EntryPoints, plural(report.EntryPoints), report.Tag, isOrAre(report.EntryPoints)))
		case others > 0:
			emit.Info(fmt.Sprintf("%d entry point%s %s every export and member of %s; %d other%s %s still declared and %s no exports the check reads%s",
				report.Compared, plural(report.Compared), keepsOrKeep(report.Compared), report.Tag,
				others, plural(others), isOrAre(others), hasOrHave(others), typed))
		default:
			emit.Info(fmt.Sprintf("%d entry point%s %s every export and member of %s%s",
				report.Compared, plural(report.Compared), keepsOrKeep(report.Compared), report.Tag, typed))
		}
		emit.PhaseEnd(phase, "success")
		return "OK", data, nil
	}

	if report.Breaking {
		for _, change := range report.Changes {
			file, line := location(workspaceRoot, projectDir, change.File, change.Line)
			emit.DiagnosticWithCode("info", fmt.Sprintf("%s: %s since %s; %s",
				change.Package, change.Message, report.Tag, declared), file, line, 0, Code)
		}
		emit.Info(fmt.Sprintf("%s since %s; %s", changeCount(report), report.Tag, declared))
		emit.PhaseEnd(phase, "success")
		return "OK", data, nil
	}

	for _, change := range report.Changes {
		file, line := location(workspaceRoot, projectDir, change.File, change.Line)
		emit.DiagnosticWithCode("error", fmt.Sprintf("%s: %s since %s; %s",
			change.Package, change.Message, report.Tag, markerHint), file, line, 0, Code)
	}
	emit.Metric("api-incompatible", len(report.Changes), "count")
	emit.PhaseEnd(phase, "failed")
	emit.Summary(fmt.Sprintf("%s since %s and no commit declares a breaking change", changeCount(report), report.Tag))
	return "FAILED", data, nil
}

func keepsOrKeep(n int) string {
	if n == 1 {
		return "keeps"
	}
	return "keep"
}

func doesOrDo(n int) string {
	if n == 1 {
		return "does"
	}
	return "do"
}

func changeCount(report *Report) string {
	n := len(report.Changes)
	return fmt.Sprintf("%d incompatible API change%s", n, plural(n))
}

// location returns the workspace-relative path and line of a file of the
// project; no file when the declaration is gone.
func location(workspaceRoot, projectDir, file string, line int) (string, int) {
	if file == "" {
		return "", 0
	}
	full := filepath.Join(projectDir, filepath.FromSlash(file))
	if rel, err := filepath.Rel(workspaceRoot, full); err == nil {
		full = rel
	}
	return filepath.ToSlash(full), line
}

func isOrAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func hasOrHave(n int) string {
	if n == 1 {
		return "has"
	}
	return "have"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
