// Package packaging implements the scaffold extension's template packaging job.
package packaging

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	templateproto "go.putnami.dev/protocol/template"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/treearchive"
)

const (
	// templateManifestFilename is the template protocol's manifest name. It is
	// what selects a directory as a template, both for `putnami package` here
	// and for the CLI's discovery, rendering and install paths.
	templateManifestFilename = "putnami.template.json"
	// templateFileSuffix marks a file the render engine substitutes variables
	// into and then strips the suffix from. Files without it are copied
	// verbatim.
	templateFileSuffix = ".template"
)

// Template packages a template directory into a distributable archive.
//
// It creates a platform-independent, reproducible .tar.gz (see treearchive)
// containing:
//   - putnami.template.json (with stamped version)
//   - All template files (.template and static)
//   - README.md, LICENSE.md (if present)
//
// Output is written to .putnami/out/{projectPath}/package/ so that
// publish-archives (owned by @putnami/cloud) can pick it up.
//
// Content dispatches to it; it is reached only for a project whose
// putnami.template.json selects this packager.
func Template(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	if ctx.Project.Name == "" {
		emit.Summary("Skipped: no project context")
		return "SKIP", nil, nil
	}

	flags := cli.ParseFlags(args)
	stable := cli.FlagBool(flags, "stable", false)
	dryRun := cli.FlagBool(flags, "dry-run", false)

	projectDir := filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)
	manifestPath := filepath.Join(projectDir, templateManifestFilename)
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		emit.Diagnostic("error", "No putnami.template.json found in "+projectDir, "", 0)
		return "FAILED", nil, nil
	}

	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		emit.Diagnostic("error", "Failed to read template manifest: "+err.Error(), "", 0)
		return "FAILED", nil, nil
	}
	var manifest struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		emit.Diagnostic("error", "Failed to parse template manifest: "+err.Error(), "", 0)
		return "FAILED", nil, nil
	}

	version := ""
	if ctx.Version != nil && ctx.Version.Full != "" {
		version = ctx.Version.Full
	} else {
		version = ctx.Workspace.Version
	}
	if stable {
		version = stripPreRelease(version)
	}

	outputDir := pkgmeta.PackageOutputDir(ctx.WorkspaceRoot, ctx.Project.Path, "archives")

	emit.Info(fmt.Sprintf("Packaging template %s@%s", manifest.Name, version))

	if dryRun {
		emit.Summary(fmt.Sprintf("Dry run: would package %s@%s", manifest.Name, version))
		return "OK", map[string]any{"dryRun": true, "name": manifest.Name, "version": version}, nil
	}

	tmpDir, err := os.MkdirTemp("", "putnami-tpl-pkg-")
	if err != nil {
		emit.Diagnostic("error", "Failed to create temp directory: "+err.Error(), "", 0)
		return "FAILED", nil, nil
	}
	defer os.RemoveAll(tmpDir)

	stageDir := filepath.Join(tmpDir, "stage")

	// Stage the template without the source-workspace declarations and local
	// state at its root. The copy refuses a link: every host installs the same
	// archive, and a Windows CLI refuses a link on extraction.
	emit.PhaseStart("stage")
	if err := treearchive.CopyTree(projectDir, stageDir, func(rel string) bool {
		return !strings.Contains(rel, "/") && (rel[0] == '.' || templateproto.IsTemplateRootPackagingExclusion(rel))
	}); err != nil {
		var link *treearchive.LinkError
		if errors.As(err, &link) {
			emit.Diagnostic("error", "Template archive entry "+err.Error()+": template archives carry no links, so replace it with the file it points to", "", 0)
		} else {
			emit.Diagnostic("error", "Failed to stage the template: "+err.Error(), "", 0)
		}
		emit.PhaseEnd("stage", "failed")
		return "FAILED", nil, nil
	}

	// The archive is the only place a consumer learns the template's version, so
	// a stamp that silently did not land would ship a version-less manifest under
	// a filename that claims one. The manifest already parsed above, so a failure
	// here is a staging fault, not a malformed input.
	if err := stampManifestVersion(filepath.Join(stageDir, templateManifestFilename), version); err != nil {
		emit.Diagnostic("error", "Failed to stamp template version: "+err.Error(), "", 0)
		emit.PhaseEnd("stage", "failed")
		return "FAILED", nil, nil
	}
	emit.PhaseEnd("stage", "success")

	emit.PhaseStart("archive")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		emit.Diagnostic("error", "Failed to create output directory: "+err.Error(), "", 0)
		emit.PhaseEnd("archive", "failed")
		return "FAILED", nil, nil
	}

	archiveName := fmt.Sprintf("%s-%s.tar.gz", manifest.Name, version)
	archivePath := filepath.Join(outputDir, archiveName)
	if err := treearchive.WriteTarGz(archivePath, stageDir); err != nil {
		emit.Diagnostic("error", "Failed to create archive: "+err.Error(), "", 0)
		emit.PhaseEnd("archive", "failed")
		return "FAILED", nil, nil
	}
	emit.PhaseEnd("archive", "success")

	// Record the channel for publish-archives to consume: the channel record
	// inside the archives directory this task owns, and the archive publication
	// manifest the archive uploader reads, which carries the template marker it
	// fans one blob out to every platform on.
	metadataPath := pkgmeta.MetadataPath(ctx.WorkspaceRoot, ctx.Project.Path)
	if err := writeArchiveRecords(ctx, outputDir, pkgmeta.PackageMetadata{
		Version:  version,
		Artifact: manifest.Name,
		Stable:   stable,
		Channels: []string{"template-archives"},
		Template: true,
	}, map[string]any{"template": true}); err != nil {
		emit.Diagnostic("error", "Failed to write metadata: "+err.Error(), "", 0)
		return "FAILED", nil, nil
	}

	emit.Artifact("archive", archiveName, "template-archive", archivePath)
	emit.Summary(fmt.Sprintf("Packaged %s@%s → %s", manifest.Name, version, archiveName))

	return "OK", map[string]any{
		"name":     manifest.Name,
		"version":  version,
		"archive":  archivePath,
		"metadata": metadataPath,
	}, nil
}

// stampManifestVersion rewrites the staged putnami.template.json with the
// resolved version. Every failure is reported: the caller has already parsed the
// source manifest, so an unreadable or unparseable staged copy means the stage
// step produced something the archive must not ship.
func stampManifestVersion(path, version string) error {
	data, err := os.ReadFile(path) //nolint:gosec // a caller-staged path inside the job's own temp directory
	if err != nil {
		return fmt.Errorf("read staged manifest: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("parse staged manifest: %w", err)
	}
	m["version"] = version
	rewritten, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode staged manifest: %w", err)
	}
	if err := os.WriteFile(path, append(rewritten, '\n'), 0o644); err != nil {
		return fmt.Errorf("write staged manifest: %w", err)
	}
	return nil
}

func stripPreRelease(version string) string {
	if base, _, found := strings.Cut(version, "-"); found {
		return base
	}
	return version
}
