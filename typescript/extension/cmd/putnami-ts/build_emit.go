package main

import (
	"os"
	"path/filepath"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/gitcandidate"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/emit"
	"go.putnami.dev/typescript/extension/internal/testjob"
)

// emitManifestFile is the command-output file build-emit writes, the artifact
// test-run folds.
const emitManifestFile = "emit-manifest.json"

// runBuildEmit writes the emit manifest of the project's dependency closure:
// each TypeScript module keyed on the JavaScript Bun emits for it, every other
// file on its bytes (internal/emit, ADR 0016). A project whose tests another
// runner than Bun runs keys every module on its bytes.
func runBuildEmit(ctx *pctx.Context, emitter *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	bunBin, err := resolveBunBin()
	if err != nil {
		return "FAILED", nil, err
	}
	members := emitMembers(ctx)
	emitter.PhaseStart("emit")
	manifest, err := emit.Build(emit.Options{
		WorkspaceRoot: ctx.WorkspaceRoot,
		Members:       members,
		ListFiles:     emitFiles(ctx),
		Transpile:     emit.BunTranspiler(bunBin),
		Runner:        emitRunner(filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path), ctx.Params.String("runner")),
	})
	if err != nil {
		emitter.PhaseEnd("emit", "failed")
		return "FAILED", nil, err
	}
	data, err := emit.Encode(manifest)
	if err != nil {
		emitter.PhaseEnd("emit", "failed")
		return "FAILED", nil, err
	}
	if err := os.MkdirAll(ctx.OutputPath, 0o755); err != nil {
		emitter.PhaseEnd("emit", "failed")
		return "FAILED", nil, err
	}
	if err := os.WriteFile(filepath.Join(ctx.OutputPath, emitManifestFile), data, 0o644); err != nil {
		emitter.PhaseEnd("emit", "failed")
		return "FAILED", nil, err
	}
	emitter.PhaseEnd("emit", "success")
	return "OK", map[string]any{"mode": manifest.Mode, "files": len(manifest.Files), "manifest": emitManifestFile}, nil
}

// emitRunner is the runner the project's tests run under, resolved as test-run
// resolves it (testjob.ResolveRunner): the runner parameter, else the runner
// its package.json test script runs, else Bun. A parameter that names no runner
// is returned as it is, which keys every module on its bytes; test-run then
// refuses it.
func emitRunner(projectPath, param string) string {
	runner, err := testjob.ResolveRunner(projectPath, param)
	if err != nil {
		return "unknown runner " + param
	}
	return string(runner)
}

// emitMembers is the project's dependency closure, the project included; a
// context without one names the project alone.
func emitMembers(ctx *pctx.Context) []emit.Member {
	closure := ctx.Project.DependencyClosure
	if len(closure) == 0 {
		closure = []pctx.ProjectRef{{Name: ctx.Project.Name, Path: ctx.Project.Path, FullPath: ctx.Project.FullPath}}
	}
	members := make([]emit.Member, 0, len(closure))
	for _, ref := range closure {
		root := ref.FullPath
		if root == "" {
			root = filepath.Join(ctx.WorkspaceRoot, ref.Path)
		}
		members = append(members, emit.Member{Path: filepath.ToSlash(ref.Path), Root: root})
	}
	return members
}

// emitFiles lists a member's candidate cut, the files Git does not ignore: from
// the job context when it carries the member's cut, which is the cut the key
// holds (ADR 0014), else from Git, and from the disk when Git does not manage
// the member, as the key reads it there.
func emitFiles(ctx *pctx.Context) func(root string) ([]string, error) {
	return func(root string) ([]string, error) {
		tree, err := gitcandidate.OpenFrom(ctx, root)
		if err != nil {
			return nil, err
		}
		if tree == nil {
			return emit.WalkFiles(root)
		}
		return tree.Paths(), nil
	}
}
