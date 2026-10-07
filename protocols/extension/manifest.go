package extension

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
)

// DeclaresContractSurface reports whether the manifest exposes anything the
// CLI ↔ extension contract governs: a command, a command group, an
// agent-facing tool, or an agent-content contribution.
//
// It is the loader's half of the package-time stamper's rule
// (gateAndStampManifestContract's manifestDeclaresContractSurface): a hook-only
// manifest — a framework package shipping a preBuild hook with `"commands": {}`
// — declares no flags, no exit-code surface, no tools and no agent content, so
// the packager deliberately leaves it UNSTAMPED. The stamper works on the raw
// JSON (it must gate a malformed surface such as `"tools": []` that never
// parses); here the manifest has already parsed, so the parsed shape is the
// same question asked of a document that survived decoding.
//
// Agent content is a surface even though it runs nothing: it is exactly the
// part of a manifest an older reader would drop without a word, so an
// unstamped manifest that declares it must not load as if it were hook-only.
func DeclaresContractSurface(m *Manifest) bool {
	if m == nil {
		return false
	}
	return len(m.Commands) > 0 || len(m.CommandGroups) > 0 || len(m.Tools) > 0 || m.DeclaresAgentContent()
}

// LoadManifest reads and parses a putnami.extension.json file, negotiating the
// manifest's declared cliContract against the contracts this CLI implements.
// See NegotiateManifest for the ladder.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read extension manifest %s: %w", path, err)
	}
	return NegotiateManifest(path, data)
}

// NegotiateManifest parses manifest bytes read from path and negotiates the
// declared cliContract against protocol/cli.CurrentContract (the base) and
// protocol/cli.LatestContract (the highest this CLI reads).
//
// The ladder has two outcomes, and "adapt" is not one of them:
//
//   - contract HIGHER than LatestContract → hard error: a future contract is
//     never half-interpreted; the remedy is a newer putnami.
//   - contract from RequiredCLIContract up to LatestContract → enforce
//     strictly: the packager stamped compliance, so a reserved global-flag
//     shadow is a hard error.
//   - contract absent (0) or below RequiredCLIContract → hard error naming
//     the fix. The stamp is what proves an extension speaks the contracts its
//     manifest relies on — the four that move together at 3 (task contract
//     v3, job context v2, runtime event protocol v2, lock format v2), the
//     session prerequisites of 4, and, for a manifest that declares agent
//     content, the additive contract 5. Loading a manifest stamped below that
//     would run a task whose outputs the scheduler cannot capture from a
//     declaration, or accept agent content a reader of its stamp never had
//     to represent.
//
// A manifest that declares no contract surface at all is outside the ladder
// (DeclaresContractSurface): the packager never stamps one, so requiring a
// stamp from it would reject every hook-only framework package.
//
// The error return covers unparseable manifests and every contract mismatch;
// nothing recoverable is left, which is why there is no diagnostic return.
func NegotiateManifest(path string, data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse extension manifest %s: %w", path, err)
	}
	if err := validateGoEmbedSelectors(&m); err != nil {
		return nil, fmt.Errorf("extension manifest %s: %w", path, err)
	}
	required := RequiredCLIContract(&m)
	switch {
	case m.CLIContract > protocolcli.LatestContract:
		return nil, fmt.Errorf("extension manifest %s requires a newer putnami (contract %d > %d)",
			path, m.CLIContract, protocolcli.LatestContract)
	case m.CLIContract >= required:
		if diags := ValidateNoReservedFlagShadows(&m); diag.HasErrors(diags) {
			return nil, fmt.Errorf("extension manifest %s has reserved global flag shadows: %s", path, formatDiagnostics(diags))
		}
		return &m, nil
	case !DeclaresContractSurface(&m):
		// Hook-only: nothing the contract governs, and nothing the packager
		// stamps. Loading it is not a tolerated downgrade — the contract simply
		// has no claim on this document.
		return &m, nil
	default:
		reason := ""
		if m.DeclaresAgentContent() {
			reason = " for its agent-content contribution"
		}
		return nil, fmt.Errorf(
			"extension manifest %s declares CLI contract %d but this putnami requires %d%s: "+
				"re-package the extension with putnami %d (or run `putnami extensions update` to pull a build that has been)",
			path, m.CLIContract, required, reason, required)
	}
}

func validateGoEmbedSelectors(m *Manifest) error {
	check := func(pattern, where string, allowed bool) error {
		if !strings.HasPrefix(pattern, "go-embed:") {
			return nil
		}
		if pattern != "go-embed:build" && pattern != "go-embed:test" {
			return fmt.Errorf("unsupported go embed selector %q in %s", pattern, where)
		}
		if !allowed {
			return fmt.Errorf("go embed selector %q is only valid in project task inputs, not %s", pattern, where)
		}
		return nil
	}
	for name, task := range m.Tasks {
		for portName, port := range task.Inputs {
			for _, pattern := range port.Files {
				if err := check(pattern, name+"."+portName, port.From == TaskInputFromProject); err != nil {
					return err
				}
			}
		}
		if task.Cache != nil && task.Cache.Key != nil {
			for _, pattern := range task.Cache.Key.Files {
				if err := check(pattern, name+".cache.key.files", true); err != nil {
					return err
				}
			}
			for _, pattern := range append(task.Cache.Key.ClosureFiles, task.Cache.Key.WorkspaceFiles...) {
				if err := check(pattern, name+".cache.key closure/workspace", false); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func formatDiagnostics(diags []diag.Diagnostic) string {
	errors := diag.Errors(diags)
	parts := make([]string, 0, len(errors))
	for _, d := range errors {
		if d.Field != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", d.Field, d.Message))
			continue
		}
		parts = append(parts, d.Message)
	}
	return strings.Join(parts, "; ")
}

// ExpandTemplateVars replaces template variables in a string.
//
// Supported variables:
//   - {workspaceRoot}  → workspace root path
//   - {projectRoot}    → project root path
//   - {extensionRoot}  → extension root path
//   - {outputRoot}     → output path (.putnami/out/<project>/<command>/<step>)
//   - {cacheRoot}      → per-workspace mutable scratch path (.putnami/cache)
//
// The lifecycle primitives declare three context-specific tokens against the
// same syntax: {extensionRuntime}, {runtimeOutput}, and
// {invocationArtifactRoot} (runtime.go). The orchestrator supplies each only in
// the contexts that own it. They are deliberately absent from
// BuildTemplateVars: a generic task has neither a prepare destination nor an
// invocation-artifact relation.
func ExpandTemplateVars(s string, vars map[string]string) string {
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}

// BuildTemplateVars builds the standard template variable map.
func BuildTemplateVars(workspaceRoot, projectRoot, extensionRoot, outputRoot string) map[string]string {
	return map[string]string{
		"workspaceRoot": workspaceRoot,
		"projectRoot":   projectRoot,
		"extensionRoot": extensionRoot,
		"outputRoot":    outputRoot,
		// cacheRoot is per-workspace mutable scratch, NOT the (now machine-global)
		// content-addressed store; see ResolveScratchRoot in the CLI store package.
		"cacheRoot": filepath.Join(workspaceRoot, ".putnami", "cache"),
	}
}
