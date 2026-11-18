package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/git"
	"go.putnami.dev/typescript/extension/internal/pkg"
)

// runPackageArchives packages the project's extension as the archive the CLI
// installs (pkg.PackageArchives), under the manifest's name and the version
// the orchestrator computed for the project, the version every archive
// packager stamps.
func runPackageArchives(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	projectRoot := filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)
	name, err := extensionManifestName(projectRoot)
	if err != nil {
		emit.Diagnostic("error", err.Error(), "putnami.extension.json", 0)
		return "FAILED", nil, nil
	}
	version, err := archiveVersion(ctx)
	if err != nil {
		return "FAILED", nil, err
	}
	dryRun := ctx.Params.Bool("dry-run", false, "dryRun")
	result, err := pkg.PackageArchives(pkg.ArchivesParams{
		WorkspaceRoot: ctx.WorkspaceRoot,
		ProjectPath:   ctx.Project.Path,
		PackageName:   name,
		Version:       version,
		Stable:        ctx.Params.Bool("stable", false),
		OutputRoot:    wsproto.CommandOutputDir(ctx.WorkspaceRoot, ctx.Project.Path, "package"),
		DryRun:        dryRun,
	})
	if err != nil {
		emit.Diagnostic("error", err.Error(), "putnami.extension.json", 0)
		return "FAILED", nil, nil
	}
	if dryRun {
		emit.Summary(fmt.Sprintf("Dry run: would package %s@%s → %d platform archives", name, version, len(result.Archives)))
	} else {
		for _, archive := range result.Archives {
			emit.Artifact("archive", filepath.Base(archive), "extension", archive)
		}
		emit.Summary(fmt.Sprintf("Packaged %s@%s → %d platform archives", name, version, len(result.Archives)))
	}
	return "OK", map[string]any{
		"name":     name,
		"version":  version,
		"archives": result.Archives,
		"files":    result.Files,
		"dryRun":   dryRun,
	}, nil
}

// extensionManifestName is the name the project's extension manifest declares.
func extensionManifestName(projectRoot string) (string, error) {
	data, err := os.ReadFile(filepath.Join(projectRoot, "putnami.extension.json"))
	if err != nil {
		return "", fmt.Errorf("the archives channel packages an extension: %w", err)
	}
	var manifest struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", fmt.Errorf("parse putnami.extension.json: %w", err)
	}
	if strings.TrimSpace(manifest.Name) == "" {
		return "", fmt.Errorf("putnami.extension.json declares no name, which the archive is published under")
	}
	return strings.TrimSpace(manifest.Name), nil
}

// archiveVersion is the full version the orchestrator computed for the
// project. Run outside the CLI, it is the workspace version's base with the
// git suffix.
func archiveVersion(ctx *pctx.Context) (string, error) {
	if ctx.Version != nil && ctx.Version.Full != "" {
		return ctx.Version.Full, nil
	}
	if ctx.Version != nil && ctx.Version.Base != "" {
		return ctx.Version.Base, nil
	}
	base := ctx.Workspace.Version
	if index := strings.IndexByte(base, '-'); index >= 0 {
		base = base[:index]
	}
	if base == "" {
		base = "0.0.0"
	}
	info, err := git.GetVersionInfo(ctx.WorkspaceRoot)
	if err != nil {
		return "", err
	}
	suffix := info.Suffix
	if suffix == "" {
		suffix = info.SHA
	}
	if suffix == "" {
		return base, nil
	}
	return base + "-" + suffix, nil
}
