package main

import (
	"fmt"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/imagepkg"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/typescript/extension/internal/git"
	"go.putnami.dev/typescript/extension/internal/pkg"
)

func runPackageNpm(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	stable := ctx.Params.Bool("stable", false)
	dryRun := ctx.Params.Bool("dry-run", false, "dryRun")

	var versionInfo *git.VersionInfo
	if ctx.Version != nil {
		versionInfo = &git.VersionInfo{
			SHA:     ctx.Version.SHA,
			Branch:  ctx.Version.Branch,
			IsDirty: ctx.Version.IsDirty,
			Suffix:  ctx.Version.Suffix,
		}
	} else {
		// A git failure is an error: the package version must not be stamped
		// from missing metadata.
		var err error
		versionInfo, err = git.GetVersionInfo(ctx.WorkspaceRoot)
		if err != nil {
			return "FAILED", nil, err
		}
	}

	releasePlan, err := npmPackageReleaseSetPlan(ctx)
	if err != nil {
		return "FAILED", nil, err
	}
	var result *pkg.NpmResult
	if releasePlan == nil {
		result, err = pkg.PackageNpm(ctx.WorkspaceRoot, ctx.Project.Path, ctx.Project.Name, stable, versionInfo, ctx.Workspace.Version)
	} else {
		result, err = pkg.PackageNpmWithReleaseSet(ctx.WorkspaceRoot, ctx.Project.Path, ctx.Project.Name, stable, versionInfo, ctx.Workspace.Version, releasePlan)
	}
	if err != nil {
		return "FAILED", nil, err
	}
	if result == nil {
		return "SKIP", nil, nil
	}

	if dryRun {
		emit.Log("info", "Dry run: would prepare npm package "+ctx.Project.Name+"@"+result.Version)
	} else {
		emit.Log("info", "Prepared npm package "+ctx.Project.Name+"@"+result.Version)
	}

	return "OK", map[string]any{
		"version":    result.Version,
		"packageDir": result.PackageDir,
		"stable":     result.Stable,
	}, nil
}

func npmPackageReleaseSetPlan(ctx *pctx.Context) (*pkg.NpmReleaseSetPlan, error) {
	plan, err := releaseset.FromContext(ctx)
	if err != nil || plan == nil {
		return nil, err
	}
	member, ok := plan.Member("npm", ctx.Project.Name)
	if !ok {
		return nil, fmt.Errorf("npm release-set plan has no member for %q", ctx.Project.Name)
	}
	if ctx.Identity != nil && member.ProjectID != ctx.Identity.Project.ID {
		return nil, fmt.Errorf("npm release-set member %q belongs to project %q, not %q", ctx.Project.Name, member.ProjectID, ctx.Identity.Project.ID)
	}
	// Package is local preparation and may be reached transitively from a
	// selected downstream member. An unchanged member still needs the immutable
	// version recorded in the plan; only its publish step must be suppressed.
	versions := make(map[string]string)
	for _, planned := range plan.Members {
		if planned.Ecosystem == "npm" {
			versions[planned.Coordinate] = planned.Version
		}
	}
	return &pkg.NpmReleaseSetPlan{Coordinate: member.Coordinate, Version: member.Version, Versions: versions}, nil
}

func runPackageDocker(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	if ctx.Project.Type == "image" {
		return imagepkg.Package(ctx, emit)
	}

	var versionInfo *git.VersionInfo
	if ctx.Version != nil {
		versionInfo = &git.VersionInfo{
			SHA:     ctx.Version.SHA,
			Branch:  ctx.Version.Branch,
			IsDirty: ctx.Version.IsDirty,
			Suffix:  ctx.Version.Suffix,
		}
	} else {
		// As with package-npm, surface a git failure instead of discarding it so
		// the image version is never derived from silently-missing metadata.
		var err error
		versionInfo, err = git.GetVersionInfo(ctx.WorkspaceRoot)
		if err != nil {
			return "FAILED", nil, err
		}
	}

	params := pkg.DockerParams{
		// No Registry: an output target is a publish decision. Local packaging
		// never consumed the docker-registry parameter, so reading it here only
		// carried a value nothing acted on.
		Tag:                   ctx.Params.String("docker-tag", "dockerTag"),
		Platform:              ctx.Params.String("platform"),
		Port:                  ctx.Params.Int("port", 3000),
		WorkspaceBuilderImage: ctx.Params.String("workspace-builder-image", "workspaceBuilderImage"),
		BaseImage:             ctx.Params.String("docker-base-image", "dockerBaseImage"),
		Load:                  ctx.Params.Bool("docker-load", false, "dockerLoad"),
		DryRun:                ctx.Params.Bool("dry-run", false, "dryRun"),
	}
	if params.Platform == "" {
		params.Platform = "linux/amd64"
	}
	if baseProject := ctx.Params.String("docker-base-project", "dockerBaseProject"); baseProject != "" {
		resolvedBase, err := imagepkg.ResolveBaseArtifact(ctx, params.BaseImage, baseProject, params.Platform)
		if err != nil {
			return "FAILED", nil, err
		}
		params.BaseImage = resolvedBase.Reference
		params.BaseLayout = resolvedBase.Layout
		params.BaseDigest = resolvedBase.Digest
	}

	result, err := pkg.PackageDocker(ctx.WorkspaceRoot, ctx.Project.Name, ctx.Project.Path, params, versionInfo, ctx.Workspace.Version)
	if err != nil {
		return "FAILED", nil, err
	}

	return "OK", result, nil
}
