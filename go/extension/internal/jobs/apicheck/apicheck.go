// Package apicheck is the `validate` task that holds the breaking-change
// marker: a stable Go project's exported API may break only in a release whose
// commits declare it.
//
// For a project the root putnami.support.json lists as stable, or for every
// project of a workspace with no catalog, the task compares the exported API
// of the working tree with the API at the last tag of the project's version
// line. An incompatible change fails
// the task unless a commit since that tag that touches the project declares a
// breaking change, with "!" or a "BREAKING CHANGE:" footer: that marker is
// what makes the version bump advance the line past a feature release.
//
// A project that ships a CLI also declares its command-surface document
// (option command-surface), and the task holds the commands and flags it
// lists to the same rule. The released document is the one at the path the
// project's putnami.json declared at the tag. A project that declared one at
// the tag and declares none now gets a warning, so removing the option does
// not end the comparison without a trace.
package apicheck

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"go.putnami.dev/go/extension/internal/apisurface"
	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// Code is the diagnostic code of an incompatible API change.
const Code = "api-compat"

// NotComparedCode is the diagnostic code of the warning a check gives when the
// checkout may lack what it needs to compare, so it compared nothing.
const NotComparedCode = "api-compat-not-compared"

// phase is the phase the task reports.
const phase = "api-compat"

// markerHint is how a failure says to resolve it.
const markerHint = `declare the breaking change with "!" or a BREAKING CHANGE: footer`

// Report is the answer of one check.
type Report struct {
	// Skip says why the project is not checked; empty when it is.
	Skip string
	// Note says why nothing was compared, when nothing was and the project
	// has no released API yet.
	Note string
	// Warning says why nothing was compared when the checkout may lack what
	// the check needs: no repository, a shallow clone, or no tag of the line
	// while the repository has other tags.
	Warning string
	// Tag is the line tag the API was compared with.
	Tag string
	// Packages is the number of importable packages at Tag.
	Packages int
	// Changes are the incompatible changes since Tag.
	Changes []apisurface.Change
	// CommandSurface is the slash path, relative to the project, of the
	// command-surface document the project declares; empty when it declares
	// none.
	CommandSurface string
	// ReleasedSurface is the slash path, relative to the project, of the
	// document the project declared at Tag, whether or not it declares one
	// now; empty when it declared none, or declared something that is not
	// the path of a file of the project.
	ReleasedSurface string
	// SurfaceNote says why the command surface was not compared while the
	// exported API was: the project declared no document at Tag.
	SurfaceNote string
	// SurfaceWarning says why the command surface was not compared although
	// the project declared a document at Tag, or may have: the project
	// declares none now, the tag does not hold it, holds it in a protocol
	// version this check does not read, or its putnami.json cannot be read.
	SurfaceWarning string
	// CommandChanges are the incompatible changes to the command surface
	// since Tag.
	CommandChanges []protocolcli.CommandChange
	// Breaking reports whether a commit since Tag that touches the project
	// declares a breaking change. The report names no commit: a branch and its
	// squash merge declare the same break with different commits, and they
	// give the same verdict.
	Breaking bool
}

// surfaceCompared reports whether the command surface was compared with Tag.
func (r *Report) surfaceCompared() bool {
	return r.CommandSurface != "" && r.Tag != "" && r.SurfaceNote == "" && r.SurfaceWarning == ""
}

// surfaceReported reports whether the result data carries the command-surface
// members: the project declares a document, or a warning says why the one it
// declared at Tag, or may have, is not compared.
func (r *Report) surfaceReported() bool {
	return r.CommandSurface != "" || r.SurfaceWarning != ""
}

// Run is the task body: it checks the project and reports each incompatible
// change as an error diagnostic, or as information when a commit declares the
// break.
func Run(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	emit.PhaseStart(phase)
	options, err := optionsFromParams(ctx.Params)
	var report *Report
	if err == nil {
		report, err = Check(ctx.WorkspaceRoot, ctx.Project.FullPath, ctx.Project.Name, options)
	}
	if err != nil {
		emit.PhaseEnd(phase, "failed")
		return "FAILED", nil, fmt.Errorf("check the API of %s: %w", ctx.Project.Name, err)
	}
	return render(emit, ctx.WorkspaceRoot, ctx.Project.FullPath, report)
}

// Check compares the exported API of the project in projectDir with its
// line's last tag, and its command-surface document when options declare
// one. When they declare none, it still reads the declaration at the tag and
// warns when there was one. name is the project's resolved name: its
// support-catalog identity, and the module path used when the project has no
// go.mod.
func Check(workspaceRoot, projectDir, name string, options Options) (*Report, error) {
	var surfacePath string
	if options.CommandSurface != "" {
		var err error
		if surfacePath, err = surfaceDocumentPath(options.CommandSurface); err != nil {
			return nil, err
		}
		if err := surfaceDocumentKeyed(surfacePath); err != nil {
			return nil, err
		}
	}
	skip, err := notChecked(workspaceRoot, projectDir, name)
	if err != nil {
		return nil, protocolcli.Classify(err, protocolcli.ErrInvalidConfig)
	}
	if skip != "" {
		return &Report{Skip: skip}, nil
	}
	// The working tree's document is read before git is: a declared document
	// that is missing or malformed fails the check whatever the history holds.
	var surface *protocolcli.CommandSurface
	if surfacePath != "" {
		if surface, err = readSurfaceInTree(projectDir, surfacePath); err != nil {
			return nil, err
		}
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
	tag, ok, err := lastReachableTag(projectDir, pattern)
	if err != nil {
		return nil, err
	}
	if !ok {
		tagged, err := hasTags(projectDir)
		if err != nil {
			return nil, err
		}
		if tagged {
			return &Report{Warning: fmt.Sprintf("no tag of line %s is reachable from HEAD, though the repository has other tags, "+
				"so the API is not compared; fetch the line's tags if it has released", pattern)}, nil
		}
		return &Report{Note: fmt.Sprintf("no tag of line %s is reachable from HEAD: there is no released API to compare with", pattern)}, nil
	}

	oldFiles, err := filesAtTag(projectDir, tag)
	if err != nil {
		return nil, err
	}
	currentFiles, err := filesInTree(projectDir)
	if err != nil {
		return nil, err
	}
	old, err := apisurface.Read(oldFiles, name)
	if err != nil {
		return nil, fmt.Errorf("read the API at %s: %w", tag, err)
	}
	current, err := apisurface.Read(currentFiles, name)
	if err != nil {
		return nil, fmt.Errorf("read the API of the working tree: %w", err)
	}
	report := &Report{Tag: tag, Packages: len(old.Packages), Changes: apisurface.Incompatible(old, current), CommandSurface: surfacePath}
	if surface != nil {
		released, err := readReleasedSurface(projectDir, tag)
		if err != nil {
			return nil, err
		}
		report.ReleasedSurface, report.SurfaceNote, report.SurfaceWarning = released.path, released.note, released.warning
		if released.surface != nil {
			report.CommandChanges = protocolcli.IncompatibleCommandChanges(*released.surface, *surface)
		}
	} else {
		// The declaration at the tag still counts when the working tree
		// declares none: removing the option warns.
		report.ReleasedSurface, report.SurfaceWarning, err = droppedSurface(projectDir, tag)
		if err != nil {
			return nil, err
		}
	}
	if len(report.Changes) == 0 && len(report.CommandChanges) == 0 {
		return report, nil
	}
	commits, err := commitsSince(projectDir, tag)
	if err != nil {
		return nil, err
	}
	report.Breaking = slices.ContainsFunc(commits, func(c commit) bool { return declaresBreaking(c.subject, c.body) })
	return report, nil
}

// filesInTree returns the Go files and go.mod files of the working tree under
// projectDir, with slash paths relative to it. A directory that holds its own
// go.mod is another module: its go.mod is returned so the surface knows to
// leave it out, and nothing below it is read. Directories the surface never
// reads are not walked.
func filesInTree(projectDir string) ([]apisurface.File, error) {
	var files []apisurface.File
	err := filepath.WalkDir(projectDir, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(projectDir, file)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if rel == "." {
				return nil
			}
			name := entry.Name()
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
				name == "testdata" || name == "vendor" || name == "internal" || name == "node_modules" {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(file, "go.mod")); err == nil {
				files = append(files, apisurface.File{Path: path.Join(rel, "go.mod")})
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || !sourceFile(rel) {
			return nil
		}
		data, err := os.ReadFile(file) //nolint:gosec // a file of the project tree being walked
		if err != nil {
			return err
		}
		files = append(files, apisurface.File{Path: rel, Data: data})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the working tree of %s: %w", projectDir, err)
	}
	return files, nil
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

	incompatible := len(report.Changes) + len(report.CommandChanges)
	data := map[string]any{
		"compared":     true,
		"tag":          report.Tag,
		"packages":     report.Packages,
		"incompatible": incompatible,
	}
	// A command change has no line: it points at the document. A project
	// that declares no document now is named by the one it declared at the
	// tag, and its warning points at its putnami.json, where the option was.
	surfaceFile, warningFile := "", ""
	switch {
	case report.CommandSurface != "":
		surfaceFile, _ = location(workspaceRoot, projectDir, apisurface.Position{File: report.CommandSurface})
		warningFile = surfaceFile
	case report.SurfaceWarning != "":
		surfaceFile, _ = location(workspaceRoot, projectDir, apisurface.Position{File: report.ReleasedSurface})
		warningFile, _ = location(workspaceRoot, projectDir, apisurface.Position{File: wsproto.ConfigFilename})
	}
	if report.surfaceReported() {
		if surfaceFile != "" {
			data["commandSurface"] = surfaceFile
		}
		data["commandSurfaceCompared"] = report.surfaceCompared()
		data["commandSurfaceIncompatible"] = len(report.CommandChanges)
		// A report carries a note or a warning, never both.
		if !report.surfaceCompared() {
			reason := report.SurfaceWarning
			if reason == "" {
				reason = report.SurfaceNote
			}
			data["commandSurfaceNotCompared"] = reason
		}
	}
	if report.SurfaceNote != "" {
		emit.Info(report.SurfaceNote)
	}
	if report.SurfaceWarning != "" {
		emit.DiagnosticWithCode("warning", report.SurfaceWarning, warningFile, 0, 0, NotComparedCode)
	}
	if len(report.Changes) == 0 {
		emit.Info(fmt.Sprintf("the exported API of %d package%s is compatible with %s",
			report.Packages, plural(report.Packages), report.Tag))
	}
	if report.surfaceCompared() && len(report.CommandChanges) == 0 {
		released := report.Tag
		if report.ReleasedSurface != report.CommandSurface {
			released = report.ReleasedSurface + " at " + report.Tag
		}
		emit.Info(fmt.Sprintf("the command surface %s is compatible with %s", report.CommandSurface, released))
	}
	if incompatible == 0 {
		emit.PhaseEnd(phase, "success")
		return "OK", data, nil
	}

	if report.Breaking {
		data["breakingDeclared"] = true
		declared := fmt.Sprintf("a commit since %s declares the breaking change", report.Tag)
		for _, change := range report.Changes {
			file, line := location(workspaceRoot, projectDir, change.Pos)
			emit.DiagnosticWithCode("info", fmt.Sprintf("%s: %s since %s; %s",
				change.Package, change.Message, report.Tag, declared), file, line, 0, Code)
		}
		for _, change := range report.CommandChanges {
			emit.DiagnosticWithCode("info", fmt.Sprintf("%s since %s; %s",
				change.Message, report.Tag, declared), surfaceFile, 0, 0, Code)
		}
		emit.Info(fmt.Sprintf("%s since %s; %s", changeCounts(report), report.Tag, declared))
		emit.PhaseEnd(phase, "success")
		return "OK", data, nil
	}

	for _, change := range report.Changes {
		file, line := location(workspaceRoot, projectDir, change.Pos)
		emit.DiagnosticWithCode("error", fmt.Sprintf("%s: %s since %s; %s",
			change.Package, change.Message, report.Tag, markerHint), file, line, 0, Code)
	}
	for _, change := range report.CommandChanges {
		emit.DiagnosticWithCode("error", fmt.Sprintf("%s since %s; %s",
			change.Message, report.Tag, markerHint), surfaceFile, 0, 0, Code)
	}
	emit.Metric("api-incompatible", len(report.Changes), "count")
	if report.CommandSurface != "" {
		emit.Metric("command-surface-incompatible", len(report.CommandChanges), "count")
	}
	emit.PhaseEnd(phase, "failed")
	emit.Summary(fmt.Sprintf("%s since %s and no commit declares a breaking change", changeCounts(report), report.Tag))
	return "FAILED", data, nil
}

// changeCounts states the incompatible changes of a report by kind, for
// example "1 incompatible API change and 2 incompatible command-surface
// changes".
func changeCounts(report *Report) string {
	var counts []string
	if n := len(report.Changes); n > 0 {
		counts = append(counts, fmt.Sprintf("%d incompatible API change%s", n, plural(n)))
	}
	if n := len(report.CommandChanges); n > 0 {
		counts = append(counts, fmt.Sprintf("%d incompatible command-surface change%s", n, plural(n)))
	}
	return strings.Join(counts, " and ")
}

// location returns the workspace-relative file and line of a position in the
// project; no file when the declaration is gone.
func location(workspaceRoot, projectDir string, pos apisurface.Position) (string, int) {
	if pos.File == "" {
		return "", 0
	}
	file := filepath.Join(projectDir, filepath.FromSlash(pos.File))
	if rel, err := filepath.Rel(workspaceRoot, file); err == nil {
		file = rel
	}
	return filepath.ToSlash(file), pos.Line
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
