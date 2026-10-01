package template

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	templateproto "go.putnami.dev/protocol/template"
	"go.putnami.dev/sdk/extension/treearchive"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// PackageResult holds the output of packaging a template.
type PackageResult struct {
	// ArchivePath is the absolute path to the created archive.
	ArchivePath string
	// Integrity is the SHA-256 hash of the archive.
	Integrity string
	// Version is the version stamped into the manifest.
	Version string
	// MetadataPath is the path to the written metadata.json.
	MetadataPath string
}

// PackageOptions configures template packaging.
type PackageOptions struct {
	// Version to stamp into the template manifest. If empty, reads from manifest.
	Version string
	// OutputDir overrides the default output directory.
	OutputDir string
	// Stable strips pre-release suffix from the version.
	Stable bool
}

// Package creates a distributable archive from a template directory.
// The archive is a single platform-independent tar.gz containing:
// - putnami.template.json (with stamped version)
// - All template files (.template and static)
// - README.md, LICENSE.md (if present)
//
// The archive is reproducible (see treearchive) and carries no links. ctx is
// checked between the staging and archiving steps, so an aborted command stops
// before it writes an archive.
func Package(ctx context.Context, templateDir string, opts PackageOptions) (*PackageResult, error) {
	// Load and validate manifest
	manifestPath := filepath.Join(templateDir, ManifestFilename)
	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("load template manifest: %w", err)
	}

	// Resolve version
	version := opts.Version
	if version == "" {
		version = manifest.Version
	}
	if version == "" {
		version = "0.0.0"
	}
	if opts.Stable {
		version = stripPreRelease(version)
	}

	// Create staging directory
	tmpDir, err := os.MkdirTemp("", "putnami-tpl-pkg-")
	if err != nil {
		return nil, fmt.Errorf("create temp directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	stageDir := filepath.Join(tmpDir, "stage")

	// Stage template content, excluding source-workspace declarations and local
	// state at the template root. Nested directories are copied as authored.
	// The manifest is the only required archive member; README.md and
	// LICENSE.md are ordinary optional template content.
	if err := treearchive.CopyTree(templateDir, stageDir, func(rel string) bool {
		return !strings.Contains(rel, "/") && (rel[0] == '.' || templateproto.IsTemplateRootPackagingExclusion(rel))
	}); err != nil {
		return nil, stagingError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Verify manifest was staged
	if _, err := os.Stat(filepath.Join(stageDir, ManifestFilename)); os.IsNotExist(err) {
		return nil, fmt.Errorf("no %s found in template directory", ManifestFilename)
	}

	// Stamp version into manifest
	if err := stampVersion(stageDir, version); err != nil {
		return nil, fmt.Errorf("stamp template manifest: %w", err)
	}

	// Determine output directory
	outputDir := opts.OutputDir
	if outputDir == "" {
		// Default: relative to template directory
		outputDir = filepath.Join(templateDir, ".putnami", "out", "package", "archives")
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("create output directory: %w", err)
	}

	// Create single platform-independent archive
	archiveName := fmt.Sprintf("%s-%s.tar.gz", manifest.Name, version)
	archivePath := filepath.Join(outputDir, archiveName)
	if err := treearchive.WriteTarGz(archivePath, stageDir); err != nil {
		return nil, fmt.Errorf("create archive: %w", err)
	}

	// Compute integrity hash
	integrity, err := extension.HashFile(archivePath)
	if err != nil {
		return nil, fmt.Errorf("hash archive: %w", err)
	}

	// Write metadata
	metadataPath := filepath.Join(outputDir, "..", "metadata.json")
	metadata := map[string]any{
		"version":  version,
		"artifact": manifest.Name,
		"channels": []string{"template-archives"},
		"stable":   opts.Stable,
		"template": true,
	}
	metadataData, _ := json.MarshalIndent(metadata, "", "  ")
	if err := os.WriteFile(metadataPath, append(metadataData, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("write metadata: %w", err)
	}

	return &PackageResult{
		ArchivePath:  archivePath,
		Integrity:    integrity,
		Version:      version,
		MetadataPath: metadataPath,
	}, nil
}

func stampVersion(stageDir, version string) error {
	manifestPath := filepath.Join(stageDir, ManifestFilename)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	manifest["version"] = version
	rewritten, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, append(rewritten, '\n'), 0o644)
}

// stagingError reports a failed staging copy. A link gets the reason every
// host installs the same template archive, and a Windows CLI refuses a link on
// extraction, so the archive carries none.
func stagingError(err error) error {
	var link *treearchive.LinkError
	if errors.As(err, &link) {
		return fmt.Errorf("template archive entry %w: template archives carry no links, so replace it with the file it points to", err)
	}
	return fmt.Errorf("stage template: %w", err)
}

func stripPreRelease(version string) string {
	if idx := strings.IndexByte(version, '-'); idx >= 0 {
		return version[:idx]
	}
	return version
}
