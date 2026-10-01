// Package pkg creates distribution-ready artifacts for Go extensions.
//
// Supported channels:
//   - extension-archives: Stages pre-built binaries and creates platform-specific tarballs
//   - docker: Builds Docker image locally (does NOT push)
//   - go: Prepares Go module source for publishing (strips replace directives, sets version)
//
// Cross-compilation is handled by the build job's cross-compile phase.
// This command consumes the pre-built binaries from {OutputPath}/bin/{platform}/.
//
// Output is written to .putnami/out/{project}/package/{channel}/
// This command does NOT upload to any registry — that's the publish step.
package pkg

import (
	"path/filepath"

	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/imagepkg"
	"go.putnami.dev/sdk/extension/jsonl"

	"go.putnami.dev/go/extension/internal/platform"
	"go.putnami.dev/go/extension/internal/releaseplan"
	"go.putnami.dev/go/extension/internal/releaseversion"
)

// Run executes the package job.
func Run(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	flags := cli.ParseFlags(args)

	dryRun := resolveBool(flags, "dry-run", ctx.Params, "dryRun", "dry-run")

	// Determine which artifacts to build. Each package-<artifact> pipeline task
	// passes an explicit --archives/--template-archives/--docker/--go flag so the
	// parallel steps each build exactly one artifact. These are read from args
	// only (not ctx.Params) so the project's injected publish-channel params can
	// gate whether a step runs without widening a scoped step to every artifact.
	// A manual `package` with no such flag falls back to the declared publish channels.
	packageArchives := cli.FlagBool(flags, "archives", false)
	packageTemplateArchives := cli.FlagBool(flags, "template-archives", false)
	packageDocker := cli.FlagBool(flags, "docker", false)
	packageGo := cli.FlagBool(flags, "go", false)

	if !packageArchives && !packageTemplateArchives && !packageDocker && !packageGo {
		packageArchives = ctx.HasPublishChannel("extension-archives")
		packageTemplateArchives = ctx.HasPublishChannel("template-archives")
		packageDocker = ctx.HasPublishChannel("docker")
		packageGo = ctx.HasPublishChannel("go")
	}

	if !packageArchives && !packageTemplateArchives && !packageDocker && !packageGo {
		return "SKIP", nil, nil
	}

	var plannedGo *releaseplan.GoProject
	if packageGo {
		var err error
		plannedGo, err = releaseplan.ResolveGoProject(ctx)
		if err != nil {
			emit.Diagnostic("error", "Invalid Go release-set plan: "+err.Error(), "", 0)
			return "FAILED", nil, err
		}
		if plannedGo != nil && !plannedGo.Member.Selected {
			// An inherited member is never packaged.
			emit.Summary("Skipped: Go module is unchanged in release-set plan")
			packageGo = false
			if !packageArchives && !packageTemplateArchives && !packageDocker {
				return "SKIP", nil, nil
			}
		}
	}
	if packageDocker && ctx.Project.Type == "image" {
		return imagepkg.Package(ctx, emit)
	}

	baseVersion, packageName := resolveVersion(ctx)
	version := releaseversion.Select(ctx.Version, baseVersion)
	if ctx.Version == nil || ctx.Version.Full == "" {
		version = baseVersion + "-" + resolveSuffix(ctx)
	}
	goVersion := version
	if plannedGo != nil {
		// Release-set versions are already exact ecosystem versions. They must
		// not be re-derived locally.
		goVersion = plannedGo.Member.Version
	}

	// Output root: .putnami/out/{project}/package/
	outputRoot := filepath.Join(ctx.WorkspaceRoot, ".putnami", "out", ctx.Project.Path, "package")

	emit.PhaseStart("package")
	overallStatus := "success"

	archiveChannels := []string{}
	if packageArchives {
		if createExtensionArchives(ctx, emit, packageName, version, outputRoot, dryRun) {
			archiveChannels = append(archiveChannels, "archives")
		} else {
			overallStatus = "failed"
		}
	}

	if packageTemplateArchives {
		if createTemplateArchives(ctx, emit, packageName, version, outputRoot, dryRun) {
			archiveChannels = append(archiveChannels, "template-archives")
		} else {
			overallStatus = "failed"
		}
	}

	// The archive packager states its whole record once, naming every archive
	// channel it produced into the directory it owns. Both archive channels land
	// in the same archives/ directory, so one record covers them; the two
	// functions above do not each write one and overwrite the other.
	if overallStatus == "success" && len(archiveChannels) > 0 {
		artifact, err := toResolverArtifactName(packageName)
		if err != nil || artifact == "" {
			artifact = packageName
		}
		if !recordArchiveChannels(emit, ctx, filepath.Join(outputRoot, "archives"), artifact, version, archiveChannels) {
			overallStatus = "failed"
		}
	}

	if packageDocker {
		params := dockerParams{
			Version:               version,
			Tag:                   resolveString(flags, "docker-tag", ctx.Params, "dockerTag", "docker-tag"),
			Platform:              resolveString(flags, "platform", ctx.Params),
			WorkspaceBuilderImage: resolveString(flags, "workspace-builder-image", ctx.Params, "workspaceBuilderImage", "workspace-builder-image"),
			BaseImage:             resolveString(flags, "docker-base-image", ctx.Params, "dockerBaseImage", "docker-base-image"),
			Port:                  resolveString(flags, "port", ctx.Params),
			Load:                  resolveBool(flags, "docker-load", ctx.Params, "dockerLoad"),
			DryRun:                dryRun,
		}
		if params.Platform == "" {
			// The same default the cross-compile step scopes its single image
			// target to, so the platform the image is assembled for and
			// the platform its binary was compiled for cannot drift apart.
			params.Platform = platform.DefaultDockerPlatform
		}
		if params.Port == "" {
			params.Port = "3000"
		}
		if baseProject := resolveString(flags, "docker-base-project", ctx.Params, "dockerBaseProject", "docker-base-project"); baseProject != "" {
			resolvedBase, err := imagepkg.ResolveBaseArtifact(ctx, params.BaseImage, baseProject, params.Platform)
			if err != nil {
				emit.Diagnostic("error", err.Error(), "", 0)
				overallStatus = "failed"
			} else {
				params.BaseImage = resolvedBase.Reference
				params.BaseLayout = resolvedBase.Layout
				params.BaseDigest = resolvedBase.Digest
			}
		}

		if overallStatus == "success" && !buildDockerImage(ctx, emit, params, outputRoot) {
			overallStatus = "failed"
		}
	}

	if packageGo {
		if !prepareGoModule(ctx, emit, goVersion, outputRoot, dryRun) {
			overallStatus = "failed"
		}
	}

	// No trailing shared write: each packager above recorded its own channel
	// inside the directory it owns, at its own version. The version
	// special case this block used to carry — "a go-only run records the
	// release-set version, a mixed run records the archive version" — went with
	// it: a record that lives beside its artifact states that artifact's
	// version and never has to choose between two.

	emit.PhaseEnd("package", overallStatus)

	if overallStatus == "success" {
		return "OK", nil, nil
	}
	return "FAILED", nil, nil
}
