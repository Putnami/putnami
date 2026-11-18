package main

import (
	"fmt"
	"path/filepath"
	"strings"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/typescript/extension/internal/pkg"
	"go.putnami.dev/typescript/extension/internal/toolchain"
)

// runPackageArchives packages the project's extension manifest as the
// archive the CLI installs from the registry (ADR 0012), and records the
// archives channel and the publication metadata the archive uploader reads.
// A project without an extension manifest that declares tasks, commands or
// hooks has nothing to package and skips.
func runPackageArchives(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	dryRun := ctx.Params.Bool("dry-run", false, "dryRun")
	version := archiveVersion(ctx)

	bun, err := toolchain.ResolveBun()
	if err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, nil
	}
	outputDir := pkgmeta.PackageOutputDir(ctx.WorkspaceRoot, ctx.Project.Path, pkg.ArchivesChannel)
	result, err := pkg.PackageExtensionArchives(pkg.ExtensionArchiveOptions{
		Bun:           bun,
		ProjectRoot:   filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path),
		WorkspaceRoot: ctx.WorkspaceRoot,
		OutputDir:     outputDir,
		Version:       version,
		DryRun:        dryRun,
	})
	if pkg.IsNoExtensionSurface(err) {
		emit.Summary("Skipped: no putnami.extension.json declaring tasks, commands or hooks")
		return "SKIP", nil, nil
	}
	if err != nil {
		emit.Diagnostic("error", err.Error(), "putnami.extension.json", 0)
		return "FAILED", nil, nil
	}
	if len(result.LeftOutHooks) > 0 {
		emit.Log("info", fmt.Sprintf("The archive leaves out the %s hook(s): the TypeScript extension runs them from the npm package each project installs (ADR 0012)", strings.Join(result.LeftOutHooks, ", ")))
	}
	data := map[string]any{
		"name":         result.Name,
		"version":      result.Version,
		"entries":      result.Entries,
		"leftOutHooks": result.LeftOutHooks,
		"archives":     result.Archives,
	}
	if dryRun {
		emit.Summary(fmt.Sprintf("Dry run: would package %s@%s → %d platform archives", result.Name, result.Version, len(result.Archives)))
		data["dryRun"] = true
		return "OK", data, nil
	}

	metadata := pkgmeta.PackageMetadata{
		Version:  result.Version,
		Artifact: result.Artifact,
		Channels: []string{pkg.ArchivesChannel},
	}
	if err := pkgmeta.WriteChannelRecord(outputDir, pkgmeta.ChannelRecord{
		Version:  metadata.Version,
		Artifact: metadata.Artifact,
		Channels: metadata.Channels,
	}); err != nil {
		emit.Diagnostic("error", "Failed to write the channel record: "+err.Error(), "", 0)
		return "FAILED", nil, nil
	}
	if err := pkgmeta.WritePackageMetadata(ctx.WorkspaceRoot, ctx.Project.Path, metadata); err != nil {
		emit.Diagnostic("error", "Failed to write the archive publication manifest: "+err.Error(), "", 0)
		return "FAILED", nil, nil
	}
	for _, archive := range result.Archives {
		emit.Artifact("archive", filepath.Base(archive), "extension", archive)
	}
	emit.Summary(fmt.Sprintf("Packaged %s@%s → %d platform archives (%s)", result.Name, result.Version, len(result.Archives), strings.Join(result.Entries, ", ")))
	data["metadata"] = pkgmeta.MetadataPath(ctx.WorkspaceRoot, ctx.Project.Path)
	return "OK", data, nil
}

// archiveVersion is the version an extension archive is published under: the
// version the CLI stamped for the job, else the workspace version, as the
// content-only packager of @putnami/scaffold reads it. A release set stamps the
// version its plan gives the member, which the archive uploader checks.
func archiveVersion(ctx *pctx.Context) string {
	if ctx.Version != nil && ctx.Version.Full != "" {
		return ctx.Version.Full
	}
	return ctx.Workspace.Version
}
