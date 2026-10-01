package packaging

import (
	"os"
	"path/filepath"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// writeArchiveRecords writes the two documents package-content owns once its
// archive is on disk: the channel record inside the archives directory, and the
// archive publication manifest at <command-output>/metadata.json.
//
// Both are declared outputs of this task, so a cache restore reproduces them
// with the archive they describe. The manifest is not an index and has
// no second writer — the archive uploader (@putnami/cloud publish-archives)
// reads exactly its five fields — so this states the whole document rather than
// merging into whatever was there.
func writeArchiveRecords(ctx *pctx.Context, outputDir string, metadata pkgmeta.PackageMetadata, extra map[string]any) error {
	channel := ""
	if len(metadata.Channels) > 0 {
		channel = metadata.Channels[0]
	}
	if err := pkgmeta.WriteChannelRecord(outputDir, pkgmeta.ChannelRecord{
		Version:  metadata.Version,
		Artifact: metadata.Artifact,
		Stable:   metadata.Stable,
		Channels: []string{channel},
		Extra:    extra,
	}); err != nil {
		return err
	}
	return pkgmeta.WritePackageMetadata(ctx.WorkspaceRoot, ctx.Project.Path, metadata)
}

// Content packages whichever content form the project declares.
//
// The packagers share one task because they share one output directory, and a
// declared output has exactly one owner. They are mutually exclusive by
// construction: a template is selected by putnami.template.json and a
// content-only extension by a putnami.extension.json that declares
// agentContent, so the dispatch below is a lookup, not a policy. A project
// carrying both forms is a declaration error rather than a silent winner.
// Agent content ships only inside an extension: a bare src/skills/ tree
// selects nothing.
func Content(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	if ctx.Project.Name == "" {
		emit.Summary("Skipped: no project context")
		return "SKIP", nil, nil
	}

	projectDir := filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)
	isTemplate := fileExists(filepath.Join(projectDir, templateManifestFilename))
	isContentExtension, err := declaresAgentContent(projectDir)
	if err != nil && !isTemplate {
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, nil
	}
	if isTemplate && isContentExtension {
		emit.Diagnostic("error", projectDir+" declares "+templateManifestFilename+" and "+extensionManifestFilename+" declaring agentContent; a project packages one content form", "", 0)
		return "FAILED", nil, nil
	}

	switch {
	case isTemplate:
		return Template(ctx, emit, args)
	case isContentExtension:
		return ContentExtension(ctx, emit, args)
	default:
		emit.Summary("Skipped: no " + templateManifestFilename + " and no " + extensionManifestFilename + " declaring agentContent in " + projectDir)
		return "SKIP", nil, nil
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
