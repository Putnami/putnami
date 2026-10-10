package docslinks

import (
	"fmt"
	"path/filepath"

	protocolcli "go.putnami.dev/protocol/cli"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// Param is the lint flag that turns the check off for a project when false.
const Param = "docs-links"

// Job is the lint task body every language extension registers, so a broken
// link fails the same way in a Go, a TypeScript and a Python project:
//
//	"lint-docs": docslinks.Job(),
//
// It checks the project's documents and fails the task when a link is broken.
// A link may name any file of the workspace, and inside a Git work tree the
// check reads the workspace's candidate cut and nothing else, so the task
// declares the workspace input `git:**` and is cached on it: deleting a link
// target, renaming it or editing a heading an anchor names moves the key, and
// an ignored file does not.
func Job() cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		if !ctx.Params.Bool(Param, true) {
			return "SKIP", nil, nil
		}
		emit.PhaseStart(Code)
		findings, err := CheckProject(ctx.WorkspaceRoot, ctx.Project.FullPath)
		if err != nil {
			emit.PhaseEnd(Code, "failed")
			return "FAILED", nil, protocolcli.Classify(fmt.Errorf("check documentation links: %w", err),
				protocolcli.ErrInvalidConfig)
		}
		files := Emit(emit, ctx.WorkspaceRoot, findings)
		data := map[string]any{"lintSummary": map[string]any{"errors": len(findings)}}
		if len(findings) == 0 {
			emit.PhaseEnd(Code, "success")
			return "OK", data, nil
		}
		emit.PhaseEnd(Code, "failed")
		emit.Summary(fmt.Sprintf("%d broken documentation link%s in %d file%s",
			len(findings), plural(len(findings)), files, plural(files)))
		return "FAILED", data, nil
	}
}

// Inputs returns the input ports every lint-docs task declares, so each
// language extension's manifest test holds its declaration to the one Job
// reads:
//
//   - repository, the workspace input `git:**`, holds the candidate cut the
//     check reads, and the verdict depends on nothing else;
//   - readme, the project input README.md, names the document the check starts
//     from. A project side left empty would key on every file under the
//     project, ignored build output included;
//   - docs-links, the parameter that turns the check off.
func Inputs() map[string]proto.TaskInputPort {
	return map[string]proto.TaskInputPort{
		"readme":     {From: proto.TaskInputFromProject, Files: []string{"README.md"}},
		"repository": {From: proto.TaskInputFromWorkspace, Files: []string{"git:**"}},
		Param:        {From: proto.TaskInputFromParams},
	}
}

// Emit reports each finding as an error diagnostic with a workspace-relative
// path and adds their count to the lint-errors metric. It returns the number
// of files that hold a finding.
func Emit(emit *jsonl.Emitter, workspaceRoot string, findings []Finding) int {
	files := make(map[string]struct{})
	for _, finding := range findings {
		file := filepath.ToSlash(relOrSelf(workspaceRoot, finding.File))
		files[file] = struct{}{}
		emit.DiagnosticWithCode("error", finding.Message+" ("+Code+")", file, finding.Line, finding.Column, Code)
	}
	if len(findings) > 0 {
		emit.Metric("lint-errors", len(findings), "count")
	}
	return len(files)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
