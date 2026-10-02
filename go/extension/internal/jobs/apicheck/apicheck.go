// Package apicheck is the `validate` task that holds the breaking-change
// marker: a stable Go project's exported API may break only in a release whose
// commits declare it.
//
// For a project the root putnami.support.json lists as stable (or does not
// list), the task compares the exported API of the working tree with the API
// at the last tag of the project's version line. An incompatible change fails
// the task unless a commit since that tag that touches the project declares a
// breaking change, with "!" or a "BREAKING CHANGE:" footer: that marker is
// what makes the version bump advance the line past a feature release.
package apicheck

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"go.putnami.dev/go/extension/internal/apisurface"
	protocolcli "go.putnami.dev/protocol/cli"
	supportproto "go.putnami.dev/protocol/support"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// Code is the diagnostic code of an incompatible API change.
const Code = "api-compat"

// phase is the phase the task reports.
const phase = "api-compat"

// markerHint is how a failure says to resolve it.
const markerHint = `declare the breaking change with "!" or a BREAKING CHANGE: footer`

// Report is the answer of one check.
type Report struct {
	// Skip says why the project is not checked; empty when it is.
	Skip string
	// Note says why nothing was compared, when nothing was.
	Note string
	// Warning says why nothing was compared when the reason is a gap in the
	// checkout rather than in the project.
	Warning string
	// Tag is the line tag the API was compared with.
	Tag string
	// Packages is the number of importable packages at Tag.
	Packages int
	// Changes are the incompatible changes since Tag.
	Changes []apisurface.Change
	// Breaking is a commit since Tag that declares a breaking change; nil when
	// none does.
	Breaking *Commit
}

// Commit identifies the commit that declares a breaking change.
type Commit struct {
	SHA     string
	Subject string
}

// Run is the task body: it checks the project and reports each incompatible
// change as an error diagnostic, or as information when a commit declares the
// break.
func Run(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	emit.PhaseStart(phase)
	report, err := Check(ctx.WorkspaceRoot, ctx.Project.FullPath, ctx.Project.Name)
	if err != nil {
		emit.PhaseEnd(phase, "failed")
		return "FAILED", nil, fmt.Errorf("check the API of %s: %w", ctx.Project.Name, err)
	}
	return render(emit, ctx.WorkspaceRoot, ctx.Project.FullPath, report)
}

// Check compares the exported API of the project in projectDir with its
// line's last tag. name is the project's resolved name: its support-catalog
// identity, and the module path used when the project has no go.mod.
func Check(workspaceRoot, projectDir, name string) (*Report, error) {
	status, err := supportStatus(workspaceRoot, projectDir, name)
	if err != nil {
		return nil, protocolcli.Classify(err, protocolcli.ErrInvalidConfig)
	}
	if status != supportproto.StatusStable {
		return &Report{Skip: fmt.Sprintf("%s is %s in %s, which promises no compatibility: its API is not checked",
			name, status, supportproto.CatalogFilename)}, nil
	}

	state, err := readRepoState(projectDir)
	if errors.Is(err, errNotARepository) {
		return &Report{Note: "the workspace is not in a git repository: there is no tagged API to compare with"}, nil
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
	report := &Report{Tag: tag, Packages: len(old.Packages), Changes: apisurface.Incompatible(old, current)}
	if len(report.Changes) == 0 {
		return report, nil
	}
	commits, err := commitsSince(projectDir, tag)
	if err != nil {
		return nil, err
	}
	for _, commit := range commits {
		if declaresBreaking(commit.subject, commit.body) {
			report.Breaking = &Commit{SHA: commit.sha, Subject: commit.subject}
			break
		}
	}
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
		emit.Warn(report.Warning)
		emit.PhaseEnd(phase, "success")
		return "OK", map[string]any{"compared": false}, nil
	case report.Note != "":
		emit.Info(report.Note)
		emit.PhaseEnd(phase, "success")
		return "OK", map[string]any{"compared": false}, nil
	}

	data := map[string]any{
		"compared":     true,
		"tag":          report.Tag,
		"packages":     report.Packages,
		"incompatible": len(report.Changes),
	}
	if len(report.Changes) == 0 {
		emit.Info(fmt.Sprintf("the exported API of %d package%s is compatible with %s",
			report.Packages, plural(report.Packages), report.Tag))
		emit.PhaseEnd(phase, "success")
		return "OK", data, nil
	}

	if report.Breaking != nil {
		data["breakingCommit"] = report.Breaking.SHA
		declared := fmt.Sprintf("declared breaking by %s %q", shortSHA(report.Breaking.SHA), report.Breaking.Subject)
		for _, change := range report.Changes {
			file, line := location(workspaceRoot, projectDir, change.Pos)
			emit.DiagnosticWithCode("info", fmt.Sprintf("%s: %s since %s, %s",
				change.Package, change.Message, report.Tag, declared), file, line, 0, Code)
		}
		emit.Info(fmt.Sprintf("%d incompatible API change%s since %s, %s",
			len(report.Changes), plural(len(report.Changes)), report.Tag, declared))
		emit.PhaseEnd(phase, "success")
		return "OK", data, nil
	}

	for _, change := range report.Changes {
		file, line := location(workspaceRoot, projectDir, change.Pos)
		emit.DiagnosticWithCode("error", fmt.Sprintf("%s: %s since %s; %s",
			change.Package, change.Message, report.Tag, markerHint), file, line, 0, Code)
	}
	emit.Metric("api-incompatible", len(report.Changes), "count")
	emit.PhaseEnd(phase, "failed")
	emit.Summary(fmt.Sprintf("%d incompatible API change%s since %s and no commit declares a breaking change",
		len(report.Changes), plural(len(report.Changes)), report.Tag))
	return "FAILED", data, nil
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

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
