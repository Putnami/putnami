package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/hooks"
)

// Output paths mirror the Go-side configextract job's defaults so manifests
// emitted by either extractor land at the same locations. Resolution uses
// project.options.generate.schema:
//
//	options.generate.schema=false → .gen/config-schema.json     (gitignored)
//	unset                         → schema/config.json          (committed)
const (
	configSchemaDefaultPath  = "schema/config.json"
	configSchemaFallbackPath = ".gen/config-schema.json"
)

// schemaManifestPeek is the minimal subset of the manifest the Go runner
// needs to surface in its JSONL summary. The full schema definition lives
// in protocols/config (Go) and config-schema-extract.ts (TS) —
// both write byte-identical JSON, so peeking at the top-level fields is
// enough for telemetry.
type schemaManifestPeek struct {
	AppName    string           `json:"appName"`
	Version    string           `json:"version"`
	SchemaHash string           `json:"schemaHash"`
	Configs    []map[string]any `json:"configs"`
}

func runConfigExtract(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	projectPath := filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)

	emit.PhaseStart("config-extract")

	bunBin, err := resolveBunBin()
	if err != nil {
		emit.PhaseEnd("config-extract", "failed")
		return "FAILED", nil, fmt.Errorf("resolving bun: %w", err)
	}

	emit.Progress(1, 3, "Discovering configExtract hooks")
	debug := ctx.Params.Bool("debug", false)
	result, err := hooks.RunHooks(ctx.WorkspaceRoot, projectPath, ctx.Project.Name, hooks.HookConfigExtract, "config-extract", bunBin, debug, configExtractHookConfig(ctx))
	if err != nil {
		emit.PhaseEnd("config-extract", "failed")
		return "FAILED", nil, fmt.Errorf("config-extract hook: %w", err)
	}

	emit.Progress(2, 3, "Inspecting emitted schema")
	schemaPath, manifest, err := loadEmittedSchema(projectPath, ctx)
	if err != nil {
		emit.PhaseEnd("config-extract", "failed")
		return "FAILED", nil, err
	}

	// No schema on disk and no hooks ran is the same observable state as
	// `putnami config-extract` on a Go project with no Config[T] calls:
	// the task reports SKIP with a clear reason rather than failing.
	if manifest == nil {
		emit.PhaseEnd("config-extract", "skipped")
		reason := "no config definitions registered"
		if result.HookCount == 0 {
			reason = "no @putnami/application configExtract hook registered (workload has no compatible dependency)"
		}
		emit.Log("info", reason)
		return "SKIP", map[string]any{"reason": reason}, nil
	}

	// A manifest exists on disk. It is only trustworthy when a hook actually
	// (re)generated it this run — reporting a leftover file as "Extracted N
	// config blocks" let stale committed schemas masquerade as fresh output
	// for months. Fail loudly on the two states where the file
	// provably was NOT regenerated: no hook ran at all, or every hook that
	// ran reported it had nothing to extract.
	if result.HookCount == 0 {
		emit.PhaseEnd("config-extract", "failed")
		return "FAILED", nil, fmt.Errorf(
			"a config schema manifest exists at %s but no @putnami/application configExtract hook is registered, "+
				"so it cannot be regenerated from source and may be stale; "+
				"install project dependencies (putnami install), or delete the manifest if this workload no longer declares config blocks",
			schemaPath)
	}
	if stale := staleHookStatus(result.Statuses); stale != "" {
		emit.PhaseEnd("config-extract", "failed")
		return "FAILED", nil, fmt.Errorf(
			"configExtract hook reported status %q but a manifest with %d config block(s) exists at %s, "+
				"so the committed schema was not regenerated from source and may be stale; "+
				"re-run with --debug to see why the hook found no config definitions, "+
				"or delete the manifest if this workload no longer declares config blocks",
			stale, len(manifest.Configs), schemaPath)
	}

	emit.Progress(3, 3, "Done")
	emit.PhaseEnd("config-extract", "success")
	emit.Log(
		"info",
		fmt.Sprintf("Extracted %d config blocks (%s) → %s", len(manifest.Configs), manifest.SchemaHash, schemaPath),
	)

	out := map[string]any{
		"schema":     schemaPath,
		"jsonSchema": jsonSchemaCompanionPath(schemaPath),
		"appName":    manifest.AppName,
		"version":    manifest.Version,
		"schemaHash": manifest.SchemaHash,
		"blocks":     len(manifest.Configs),
	}
	return "OK", out, nil
}

// staleHookStatus inspects the per-hook summary statuses and returns the
// first explicit "nothing was extracted" status ("empty" or "skipped") when
// no hook reported "ok". Hooks that predate the status convention report ""
// — those are trusted (legacy behavior) so a summary without a status never
// turns a previously-green pipeline red.
func staleHookStatus(statuses []string) string {
	stale := ""
	for _, s := range statuses {
		switch s {
		case "ok":
			return ""
		case "empty", "skipped":
			if stale == "" {
				stale = s
			}
		}
	}
	return stale
}

// loadEmittedSchema looks for the manifest the bun hook just wrote, trying
// the committed path first and the gitignored fallback second. Returns
// (path, nil, nil) when nothing is on disk so the caller can SKIP cleanly.
// ctx may be nil for callers that have no project context to consult.
func loadEmittedSchema(projectPath string, ctx *pctx.Context) (string, *schemaManifestPeek, error) {
	for _, candidate := range schemaCandidatePaths(ctx) {
		full := filepath.Join(projectPath, candidate)
		data, err := os.ReadFile(full)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", nil, fmt.Errorf("reading %s: %w", full, err)
		}
		var manifest schemaManifestPeek
		if err := json.Unmarshal(data, &manifest); err != nil {
			return full, nil, fmt.Errorf("parsing %s: %w", full, err)
		}
		return full, &manifest, nil
	}
	return "", nil, nil
}

// schemaCandidatePaths returns the project-relative paths to probe for the
// manifest, in priority order. Project config determines which location wins
// when both a current and stale copy exist. ctx may be nil.
func schemaCandidatePaths(ctx *pctx.Context) []string {
	switch active := resolveConfigSchemaPath(ctx); active {
	case configSchemaDefaultPath:
		return []string{configSchemaDefaultPath, configSchemaFallbackPath}
	case configSchemaFallbackPath:
		return []string{configSchemaFallbackPath, configSchemaDefaultPath}
	default:
		return []string{active, configSchemaDefaultPath, configSchemaFallbackPath}
	}
}

func resolveConfigSchemaPath(ctx *pctx.Context) string {
	if commit, ok := pctx.GenerateSchemaCommit(ctx); ok {
		if commit {
			return configSchemaDefaultPath
		}
		return configSchemaFallbackPath
	}
	return configSchemaDefaultPath
}

// configExtractHookConfig returns the project's options.generate block to
// forward to the bun config-extract hook as its `config`. The whole block is
// passed (not just the keys this Go runner reads) so the hook is the single
// consumer that decides which generate options it needs; today it uses only
// `schema`. Returns nil when the project declares no generate options.
func configExtractHookConfig(ctx *pctx.Context) map[string]any {
	if ctx == nil {
		return nil
	}
	raw, ok := ctx.Project.Options["generate"]
	if !ok {
		return nil
	}
	var opts map[string]any
	if json.Unmarshal(raw, &opts) != nil || len(opts) == 0 {
		return nil
	}
	return opts
}

// jsonSchemaCompanionPath turns "schema/config.json" into
// "schema/config.jsonschema.json" — same transform the Go-side and TS-side
// extractors use, so the caller can derive one path from the other without
// having to re-stat the disk.
func jsonSchemaCompanionPath(manifestPath string) string {
	ext := filepath.Ext(manifestPath)
	if ext == "" {
		return manifestPath + ".jsonschema"
	}
	return manifestPath[:len(manifestPath)-len(ext)] + ".jsonschema" + ext
}
