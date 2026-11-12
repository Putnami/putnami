package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/typescript/extension/internal/build"
	"go.putnami.dev/typescript/extension/internal/serve"
)

// nodeBundleDir is where a serve or run of a project on the node runtime
// writes the bundle it starts: under the command's output directory.
func nodeBundleDir(ctx *pctx.Context, command string) string {
	if ctx.OutputPath != "" {
		return filepath.Join(ctx.OutputPath, "node")
	}
	return filepath.Join(wsproto.CommandOutputDir(ctx.WorkspaceRoot, ctx.Project.Path, command), "node")
}

// nodeBundler returns the bundler of a serve or run on the node runtime. It
// writes the Node bundle of entrypoint, as the compile step does, into a
// staging directory beside outDir and moves it to outDir only once bun
// succeeded, so a bundle bun refuses leaves the previous one in place. It
// then links outDir/node_modules to the project's node_modules (a junction on
// Windows), so the bundle resolves the packages it leaves as imports from the
// project first, then from each directory above outDir up to the workspace
// root.
func nodeBundler(bunBin, projectPath, outDir, entrypoint string) serve.NodeBundler {
	return func() (serve.NodeBuild, []string, error) {
		stage := outDir + ".next"
		bundle, bundleErrors, err := build.RunNodeBundle(bunBin, projectPath, stage, entrypoint)
		if err != nil || len(bundleErrors) > 0 {
			_ = os.RemoveAll(stage)
			return serve.NodeBuild{}, bundleErrors, err
		}
		if err := os.RemoveAll(outDir); err != nil {
			return serve.NodeBuild{}, nil, err
		}
		if err := os.Rename(stage, outDir); err != nil {
			return serve.NodeBuild{}, nil, err
		}
		modules := filepath.Join(projectPath, "node_modules")
		if _, err := os.Stat(modules); err == nil {
			if err := dirlink.Create(modules, filepath.Join(outDir, "node_modules")); err != nil && !errors.Is(err, fs.ErrExist) {
				return serve.NodeBuild{}, nil, err
			}
		}
		bundle.Inputs = append(bundle.Inputs, filepath.Join(projectPath, "package.json"))
		return serve.NodeBuild{Entry: filepath.Join(outDir, filepath.FromSlash(bundle.Entrypoint)), Inputs: bundle.Inputs}, nil, nil
	}
}
