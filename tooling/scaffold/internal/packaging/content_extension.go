package packaging

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/agentartifact"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// extensionManifestFilename selects the content-only extension packager when
// the manifest declares agentContent.
const extensionManifestFilename = extproto.ManifestFilename

// contentExtensionPlatforms returns the registry platform keys an extension
// archive is published under: the SDK's distribution matrix, the one the Go
// packager's extension archives cover. The CLI's extension installer asks the
// registry for the host's own os/arch, so a content-only extension, whose
// archive is platform independent, is published once per key with identical
// bytes.
func contentExtensionPlatforms() []string {
	return pkgmeta.ArchivePlatformSuffixes()
}

// declaresAgentContent reports whether the project's extension manifest
// declares agentContent. A project without a manifest, or whose manifest
// declares no agentContent, declares none; a contribution of the wrong shape
// still declares it, and the package step's strict parse reports it.
func declaresAgentContent(projectDir string) (bool, error) {
	path := filepath.Join(projectDir, extensionManifestFilename)
	if !fileExists(path) {
		return false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	var manifest struct {
		AgentContent json.RawMessage `json:"agentContent"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	raw := bytes.TrimSpace(manifest.AgentContent)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null")), nil
}

// encodeArtifactName mirrors the CLI's layout.EncodeName so the published file
// key matches the name the registry and the consumer already agree on.
// @putnami/contributor becomes putnami-contributor.
func encodeArtifactName(name string) string {
	if len(name) > 0 && name[0] == '@' {
		trimmed := name[1:]
		for index := range len(trimmed) {
			if trimmed[index] == '/' {
				return trimmed[:index] + "-" + trimmed[index+1:]
			}
		}
		return trimmed
	}
	return name
}

// ContentExtension packages one content-only extension: an extension that
// declares agentContent and runs nothing. Content dispatches to it for a
// project whose putnami.extension.json declares agentContent.
//
// agentartifact.PackageExtension does the packaging: it builds the authored
// source under the extension's name and version with the declared content
// policy, stamps the packaged form and the contract its vocabulary requires,
// and refuses a manifest this build's loader would not read. An extension with
// a runtime, commands, tools, tasks, hooks or a workspace adapter is refused
// there: its language extension packages it with the executable those need.
//
// The archive is platform independent. It is written once per registry
// platform key, <encoded-name>-<os>-<arch>.tar.gz, into
// .putnami/out/{projectPath}/package/archives/ on the archives channel, the
// same layout and channel the Go packager gives extension archives, so the
// archive uploader publishes it and every host's install resolves it.
func ContentExtension(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	projectDir := filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)
	flags := cli.ParseFlags(args)
	stable := cli.FlagBool(flags, "stable", false)
	dryRun := cli.FlagBool(flags, "dry-run", false)
	version := contentVersion(ctx, stable)

	// Package before the dry-run exit: packaging is what proves the source,
	// the content policy and the loader postcondition still hold.
	pkg, err := agentartifact.PackageExtension(projectDir, version)
	if err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, nil
	}
	artifact := encodeArtifactName(pkg.Name)
	platforms := contentExtensionPlatforms()
	archiveNames := make([]string, 0, len(platforms))
	for _, platform := range platforms {
		archiveNames = append(archiveNames, artifact+"-"+platform+".tar.gz")
	}
	emit.Info(fmt.Sprintf("Packaging content-only extension %s@%s", pkg.Name, version))
	data := map[string]any{
		"name":                    pkg.Name,
		"version":                 version,
		"archiveSha256":           pkg.ArchiveSHA256,
		"manifestSha256":          pkg.Content.ManifestSHA256,
		"extensionManifestSha256": sha256Hex(pkg.Manifest),
	}

	if dryRun {
		emit.Summary(fmt.Sprintf("Dry run: would package %s@%s", pkg.Name, version))
		data["dryRun"] = true
		data["archives"] = archiveNames
		return "OK", data, nil
	}

	outputDir := pkgmeta.PackageOutputDir(ctx.WorkspaceRoot, ctx.Project.Path, "archives")
	emit.PhaseStart("archive")
	archivePaths := make([]string, 0, len(archiveNames))
	for _, name := range archiveNames {
		archivePath := filepath.Join(outputDir, name)
		if err := agentartifact.WriteExtensionArchive(archivePath, pkg); err != nil {
			emit.Diagnostic("error", err.Error(), "", 0)
			emit.PhaseEnd("archive", "failed")
			return "FAILED", nil, nil
		}
		archivePaths = append(archivePaths, archivePath)
	}
	emit.PhaseEnd("archive", "success")

	// Each platform key is its own file, so the publication manifest is not a
	// template: the uploader keys every blob by the platform its name carries.
	metadataPath := pkgmeta.MetadataPath(ctx.WorkspaceRoot, ctx.Project.Path)
	if err := writeArchiveRecords(ctx, outputDir, pkgmeta.PackageMetadata{
		Version:  version,
		Artifact: artifact,
		Stable:   stable,
		Channels: []string{"archives"},
	}, nil); err != nil {
		emit.Diagnostic("error", "Failed to write metadata: "+err.Error(), "", 0)
		return "FAILED", nil, nil
	}

	for index, name := range archiveNames {
		emit.Artifact("archive", name, "extension", archivePaths[index])
	}
	emit.Summary(fmt.Sprintf("Packaged %s@%s → %d platform archives", pkg.Name, version, len(archiveNames)))
	data["archives"] = archivePaths
	data["metadata"] = metadataPath
	return "OK", data, nil
}

// contentVersion is the version a content package is published under: the
// job's resolved version, else the workspace version, with the pre-release
// suffix stripped for a stable package.
func contentVersion(ctx *pctx.Context, stable bool) string {
	version := ctx.Workspace.Version
	if ctx.Version != nil && ctx.Version.Full != "" {
		version = ctx.Version.Full
	}
	if stable {
		version = stripPreRelease(version)
	}
	return version
}

// sha256Hex is the lowercase hex SHA-256 of data: the digest a lock binds an
// extension manifest by.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
