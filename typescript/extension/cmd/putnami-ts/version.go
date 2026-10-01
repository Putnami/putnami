package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/git"
)

// genVersionInfo is the structure of the .gen/version.json file the CLI writes
// for every project before jobs run (see tooling/cli generateVersionFiles). The
// version command consumes it the same way peers do, rather than re-deriving
// version metadata, so the reported version matches the docker tag / npm
// version exactly.
type genVersionInfo struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Suffix    string `json:"suffix,omitempty"`
	SHA       string `json:"sha"`
	Branch    string `json:"branch"`
	IsDirty   bool   `json:"isDirty"`
	BuildTime string `json:"buildTime"`
}

// runVersion reports the project's version metadata so it is user-queryable for
// support, reproducibility, and CI pinning. It prefers the generated
// .gen/version.json (the authoritative, CLI-stamped source shared by all
// project types); when that file is absent — for example when the command is
// invoked outside a planned run — it falls back to live git metadata.
func runVersion(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	_ = args
	projectPath := filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)

	if info := readGeneratedVersion(projectPath); info != nil {
		data := versionData(info)
		emit.Log("info", ctx.Project.Name+" "+info.Version)
		return "OK", data, nil
	}

	// No generated stamp: derive version metadata directly from git. Unlike the
	// packaging call sites, the error is surfaced (not discarded) so a git
	// failure produces a real diagnostic instead of silently empty version data.
	gitInfo, err := git.GetVersionInfo(ctx.WorkspaceRoot)
	if err != nil {
		return "FAILED", nil, err
	}

	base := ctx.Workspace.Version
	if base == "" {
		base = "0.0.0"
	}
	full := base
	if gitInfo.Suffix != "" {
		full = base + "-" + gitInfo.Suffix
	} else if gitInfo.SHA != "" {
		full = base + "-" + gitInfo.SHA
	}

	data := map[string]any{
		"name":    ctx.Project.Name,
		"version": full,
		"suffix":  gitInfo.Suffix,
		"sha":     gitInfo.SHA,
		"branch":  gitInfo.Branch,
		"isDirty": gitInfo.IsDirty,
	}
	emit.Log("info", ctx.Project.Name+" "+full)
	return "OK", data, nil
}

// readGeneratedVersion reads and parses <projectPath>/.gen/version.json,
// returning nil when the file is missing or unreadable so the caller can fall
// back to live git metadata.
func readGeneratedVersion(projectPath string) *genVersionInfo {
	raw, err := os.ReadFile(filepath.Join(projectPath, ".gen", "version.json"))
	if err != nil {
		return nil
	}
	var info genVersionInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil
	}
	return &info
}

// versionData renders the generated version info as the job's result data.
func versionData(info *genVersionInfo) map[string]any {
	return map[string]any{
		"name":      info.Name,
		"version":   info.Version,
		"suffix":    info.Suffix,
		"sha":       info.SHA,
		"branch":    info.Branch,
		"isDirty":   info.IsDirty,
		"buildTime": info.BuildTime,
	}
}
