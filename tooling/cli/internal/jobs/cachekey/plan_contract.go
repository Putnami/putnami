package cachekey

import (
	"path"
	"path/filepath"
	"sort"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// resolvedOutputRoot is the root every declared output with a literal path
// carries once the planner has resolved it. Collapsing all symbolic roots onto
// one sentinel is what makes OutputsOverlap compare a workspace-rooted path
// against a project-rooted one: after resolution both are absolute paths in the
// same namespace, and containment answers the question exactly.
const resolvedOutputRoot = "resolved"

// DeclaredOutputOwner is one resolved declared output and the plan node that
// claims it.
type DeclaredOutputOwner struct {
	Job *jobmodel.ScheduledJob
	ID  string
	Ref extension.OutputRef
}

// DeclaredOutputOwners flattens the plan into resolved output claims. Nodes
// without a v3 declaration contribute nothing, which is what makes the whole
// check vacuous for v2 manifests.
func DeclaredOutputOwners(planned []*jobmodel.ScheduledJob, ws *workspace.Workspace) []DeclaredOutputOwner {
	var owners []DeclaredOutputOwner
	for _, job := range planned {
		declaration := jobmodel.TaskDeclarationOf(job)
		if declaration == nil || len(declaration.Outputs) == 0 {
			continue
		}
		ids := make([]string, 0, len(declaration.Outputs))
		for id := range declaration.Outputs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			ref, ok := resolveDeclaredOutput(job, declaration.Outputs[id], ws)
			if !ok {
				continue
			}
			owners = append(owners, DeclaredOutputOwner{Job: job, ID: id, Ref: ref})
		}
	}
	return owners
}

// resolveDeclaredOutput turns a root-relative declaration into the ref the
// planner compares. A literal path becomes an absolute path under the sentinel
// root; a pathFrom output keeps its port and carries the resolved base as its
// root, so the same port under the same base collides and a port never collides
// with a literal path (that comparison is left to resolve time, per the
// contract). An unnormalizable path is skipped — manifest validation reports it,
// and guessing at ownership for it would be worse than saying nothing.
//
// A ceded subpath (DeclaredOutput.Excludes) travels with the ref, resolved
// against the SAME base as the path it sits inside. That is what makes the
// cross-extension check apply the predicate the manifest-local one applies:
// both call extension.OutputsOverlap with the carve-out attached, so a subpath
// one task cedes and another owns is legal in exactly one place, or in neither.
func resolveDeclaredOutput(job *jobmodel.ScheduledJob, output extension.DeclaredOutput, ws *workspace.Workspace) (extension.OutputRef, bool) {
	base, ok := declaredOutputBase(job, output.EffectiveRoot(), ws)
	if !ok {
		return extension.OutputRef{}, false
	}
	if output.PathFrom != "" {
		return extension.OutputRef{Root: base, FromPort: output.PathFrom}, true
	}
	normalized, err := extension.NormalizeOutputPath(output.Path)
	if err != nil {
		return extension.OutputRef{}, false
	}
	decidable := extension.DecidableExcludes(normalized, output.Excludes)
	var excludes []string
	if len(decidable) > 0 {
		excludes = make([]string, 0, len(decidable))
		for _, exclude := range decidable {
			excludes = append(excludes, path.Join(base, exclude))
		}
	}
	return extension.OutputRef{
		Root:     resolvedOutputRoot,
		Path:     path.Join(base, normalized),
		Excludes: excludes,
	}, true
}

// declaredOutputBase resolves a symbolic output root to the concrete directory
// the plan node writes it under. The command-output base is the shared
// per-command directory the scheduler uses, so two steps of one command resolve
// to the same base and two commands never do.
func declaredOutputBase(job *jobmodel.ScheduledJob, root string, ws *workspace.Workspace) (string, bool) {
	wsRoot := ""
	if ws != nil {
		wsRoot = filepath.ToSlash(ws.Root)
	}
	switch root {
	case extension.OutputRootWorkspace:
		return path.Join(wsRoot, "."), true
	case extension.OutputRootProject:
		return path.Join(wsRoot, ProjectRelPath(job)), true
	case extension.OutputRootCommandOutput:
		return path.Join(wsRoot, ".putnami", "out", ProjectRelPath(job), job.CommandName()), true
	default:
		return "", false
	}
}

func ProjectRelPath(job *jobmodel.ScheduledJob) string {
	if job == nil || job.Project == nil || job.Project.Path == "" {
		return "."
	}
	return filepath.ToSlash(job.Project.Path)
}
