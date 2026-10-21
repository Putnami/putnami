package pkg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/agentartifact"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// packageForm is the form a packager publishes an extension in. It decides
// whether the manifest's registry extension dependencies take part in the
// stamp.
type packageForm int

const (
	// putArchive is an archive the CLI installs into its artifact store. The
	// CLI follows the dependencies of an extension it loads from there, so a
	// registry dependency is contract surface: the archive is stamped
	// ExtensionDependenciesContract, even when its manifest declares only
	// hooks and dependencies.
	putArchive packageForm = iota
	// npmPackage is a package the package manager installs into node_modules.
	// CLI discovery finds no extension there, so the stamp leaves the
	// dependency rung out (proto.RequiredLoadContract), and a hook-only
	// framework package stays unstamped.
	npmPackage
)

// gateAndStampManifestContract is the package-time half of the CLI ↔ extension
// contract: it validates the STAGED manifest strictly and, on success, stamps
// into it the lowest contract whose vocabulary covers it for form
// (packageForm.requiredContract): CurrentContract, or an additive contract for
// the vocabulary the manifest uses. The stamp is earned, not claimed —
// strictness lives here, so a manifest with reserved global-flag shadows (or
// any other protocol violation) fails the package job and never reaches a
// registry.
//
// The gate applies only to manifests that declare a contract surface
// (non-empty commands, commandGroups, or tools, or an agentContent or verbs
// section).
// Hook-only manifests (e.g. framework packages shipping a preBuild hook with
// `"commands": {}`) expose neither flags nor agent-facing tools for the
// contract to govern and intentionally fail full strict validation, so they
// are left untouched and unstamped (contract 0 → outside the ladder). A put
// archive whose hook-only manifest names a registry extension dependency is
// the exception: it is stamped without the full validation
// (stampDependencyContract).
//
// The gate has ONE postcondition, and it covers both arms: whatever this job is
// about to ship LOADS under this build's loader (verifyStagedManifestLoads),
// and the agent content its manifest binds is staged byte for byte
// (agentartifact.VerifyStagedExtensionContent). Since contract 3 the loader has
// no tolerant arm — a manifest it rejects is an extension every consumer
// silently skips — so "we validated it" is not the same statement as "it will
// load", and only the second one is worth publishing on.
//
// The caller stages agent content (agentartifact.StageExtensionContent) before
// the gate runs; a manifest still carrying the authored source form fails the
// postcondition.
//
// Only the staged copy is rewritten — the source manifest on disk is never
// touched, mirroring how the version is stamped. The stamp is a deterministic
// function of the manifest and json.MarshalIndent sorts map keys, so the staged
// manifest stays byte-deterministic.
func gateAndStampManifestContract(stageDir string, form packageForm) error {
	manifestPath := filepath.Join(stageDir, "putnami.extension.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read staged extension manifest: %w", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse staged extension manifest: %w", err)
	}
	if !manifestDeclaresContractSurface(raw) {
		if form == putArchive && namesRegistryDependency(raw) {
			return stampDependencyContract(stageDir, manifestPath, data, raw)
		}
		// Hook-only: nothing to gate and nothing to stamp, but the artifact must
		// still load.
		return verifyStagedManifestLoads(manifestPath)
	}
	if err := rejectFutureContractClaim(raw); err != nil {
		return err
	}

	m, diags := proto.ParseManifest(data)
	if m != nil && !diag.HasErrors(diags) {
		diags = append(diags, proto.FullValidateManifest(m)...)
	}
	contract := protocolcli.CurrentContract
	if m != nil {
		contract = form.requiredContract(m)
	}
	if errs := diag.Errors(diags); len(errs) > 0 {
		return fmt.Errorf("staged extension manifest does not conform to CLI contract %d and cannot be published: %s",
			contract, diagnosticMessages(errs))
	}
	return stampAndVerify(stageDir, manifestPath, data, raw, contract)
}

// stampDependencyContract is the stamp-only arm of a put archive whose
// manifest declares only hooks and registry extension dependencies. The CLI
// follows those dependencies, so the archive must carry the contract that
// covers them, or an older CLI would load it without them. The strict
// requirement of a command, tool or agent content does not apply to it; the
// strict decode and the load postcondition do.
func stampDependencyContract(stageDir, manifestPath string, data []byte, raw map[string]any) error {
	if err := rejectFutureContractClaim(raw); err != nil {
		return err
	}
	m, diags := proto.ParseManifest(data)
	if errs := diag.Errors(diags); len(errs) > 0 || m == nil {
		return fmt.Errorf("staged extension manifest names extension dependencies but cannot be published: %s",
			diagnosticMessages(errs))
	}
	return stampAndVerify(stageDir, manifestPath, data, raw, proto.RequiredCLIContract(m))
}

// stampAndVerify writes contract into the staged manifest and checks the
// gate's postcondition. On failure it puts the staged input back.
func stampAndVerify(stageDir, manifestPath string, data []byte, raw map[string]any, contract int) error {
	raw["cliContract"] = contract
	rewritten, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("rewrite staged extension manifest: %w", err)
	}
	if err := os.WriteFile(manifestPath, append(rewritten, '\n'), 0o644); err != nil {
		return fmt.Errorf("rewrite staged extension manifest: %w", err)
	}
	postErr := verifyStagedManifestLoads(manifestPath)
	if postErr == nil {
		postErr = verifyStagedAgentContent(stageDir)
	}
	if postErr != nil {
		// A stamp the loader rejects, or one that binds content the stage does
		// not hold, is worse than no stamp: it claims a compliance the artifact
		// does not have. Put the staged input back so the failed package job
		// leaves nothing half-gated behind.
		if restoreErr := os.WriteFile(manifestPath, data, 0o644); restoreErr != nil {
			return fmt.Errorf("%w (and the staged manifest could not be restored: %w)", postErr, restoreErr)
		}
		return postErr
	}
	return nil
}

// requiredContract is the stamp of a manifest packaged in form: every rung its
// vocabulary reaches for a put archive (proto.RequiredCLIContract), and every
// rung but the dependency one for an npm package (proto.RequiredLoadContract).
func (form packageForm) requiredContract(m *proto.Manifest) int {
	if form == npmPackage {
		return proto.RequiredLoadContract(m)
	}
	return proto.RequiredCLIContract(m)
}

// diagnosticMessages joins the messages of diags for a packaging error.
func diagnosticMessages(diags []diag.Diagnostic) string {
	msgs := make([]string, 0, len(diags))
	for _, d := range diags {
		msgs = append(msgs, d.String())
	}
	return strings.Join(msgs, "; ")
}

// verifyStagedAgentContent is the content half of the postcondition: the
// staged manifest binds its agent content by digest, and the stage holds
// exactly those bytes.
func verifyStagedAgentContent(stageDir string) error {
	if err := agentartifact.VerifyStagedExtensionContent(stageDir); err != nil {
		return fmt.Errorf("staged extension manifest cannot be published: its agent content does not match what it binds: %w", err)
	}
	return nil
}

// validateStagedRuntimeExecutable is the archive-time half of the declared
// runtime contract. A platform archive for goos that advertises
// runtime.executable must carry a regular file at that path under its file name
// for goos (pkgmeta.ExecutableName: the declared path with ".exe" on windows);
// source prepare declarations never excuse a missing packaged runtime. The
// check never reads the file's execute bits: the archive marks the file
// executable because the manifest declares it, so a machine whose filesystem
// records no execute bit packages the same archive. It returns the file's
// slash-separated archive path, or "" when the manifest declares no runtime.
// This helper is byte-identical in both packager modules so Go-archive and npm
// packaging cannot drift on the rule.
func validateStagedRuntimeExecutable(stageDir, goos string) (string, error) {
	manifestPath := filepath.Join(stageDir, "putnami.extension.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", fmt.Errorf("read staged extension manifest: %w", err)
	}
	var manifest struct {
		Runtime *struct {
			Executable string `json:"executable"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", fmt.Errorf("parse staged extension manifest: %w", err)
	}
	if manifest.Runtime == nil {
		return "", nil
	}
	declared := filepath.FromSlash(manifest.Runtime.Executable)
	if declared == "" || !filepath.IsLocal(declared) || filepath.Clean(declared) == "." {
		return "", fmt.Errorf("staged extension runtime executable %q is not a local archive path", manifest.Runtime.Executable)
	}
	name := pkgmeta.ExecutableName(goos, manifest.Runtime.Executable)
	executable := filepath.Clean(filepath.FromSlash(name))
	currentPath := stageDir
	components := strings.Split(executable, string(filepath.Separator))
	for index, component := range components {
		currentPath = filepath.Join(currentPath, component)
		info, err := os.Lstat(currentPath)
		if err != nil {
			return "", fmt.Errorf("staged extension runtime executable %q is unavailable: %w", name, err)
		}
		if index < len(components)-1 {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				ancestor := filepath.ToSlash(filepath.Join(components[:index+1]...))
				return "", fmt.Errorf(
					"staged extension runtime executable %q has ancestor %q that is a symlink or not a directory",
					name,
					ancestor,
				)
			}
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", fmt.Errorf("staged extension runtime executable %q is not a regular file", name)
		}
	}
	return filepath.ToSlash(executable), nil
}

// rejectFutureContractClaim refuses to package a manifest that declares a
// HIGHER cliContract than the latest one this packager implements.
//
// A claim at or below that is fine and is the whole point of the ratchet: the
// stamp is earned by the validation this job just ran, so an author's stale `2`
// is replaced by the contract it actually passed, and a claim of the agent
// content contract on a manifest without agent content is replaced by the base
// contract its vocabulary needs. A higher claim is not the same shape of
// mistake. This packager cannot run the contract-N checks it does not have, so
// overwriting the claim with its own would publish an artifact asserting a
// compliance nobody verified — and the loader, seeing a contract it reads,
// would accept it. The remedy is an upgrade, not a downgrade.
func rejectFutureContractClaim(raw map[string]any) error {
	claimed, ok := raw["cliContract"].(float64)
	if !ok {
		// Absent, or a non-number the caller's strict parse rejects with a
		// better diagnostic than anything this function could invent.
		return nil
	}
	if int(claimed) <= protocolcli.LatestContract {
		return nil
	}
	return fmt.Errorf(
		"staged extension manifest declares CLI contract %d but this packager implements %d and cannot certify it: "+
			"upgrade putnami to package this extension (the cliContract stamp is earned by the packaging build, never downgraded)",
		int(claimed), protocolcli.LatestContract)
}

// verifyStagedManifestLoads is the gate's postcondition, run through the very
// loader a consumer will use. It is deliberately not a re-statement of the
// validation above: FullValidateManifest answers "is this manifest correct",
// while LoadManifest answers "will this artifact be usable", and only the
// second question has a consumer on the other end of it.
func verifyStagedManifestLoads(manifestPath string) error {
	if _, err := proto.LoadManifest(manifestPath); err != nil {
		return fmt.Errorf(
			"staged extension manifest cannot be published: this putnami's own loader rejects it, "+
				"so every consumer would skip the extension: %w", err)
	}
	return nil
}

// manifestDeclaresContractSurface reports whether the raw manifest exposes a
// command, tool, agent-content or verb-vocabulary surface the contract must
// govern. Only genuinely absent or empty-object commands, commandGroups, and
// tools are treated as hook-only and bypass the gate. A present value of the
// wrong JSON type (e.g. "tools": []) is NOT hook-only — it is a malformed
// contract surface that must be gated, so strict parsing rejects it instead of
// publishing an unloadable manifest unstamped. Any non-null agentContent or
// verbs is gated, an empty one included, because the loader counts it as a
// surface (proto.DeclaresContractSurface) and strict validation refuses an
// empty one.
func manifestDeclaresContractSurface(raw map[string]any) bool {
	return surfaceNeedsGate(raw["commands"]) ||
		surfaceNeedsGate(raw["commandGroups"]) ||
		surfaceNeedsGate(raw["tools"]) ||
		raw["agentContent"] != nil ||
		raw["verbs"] != nil
}

// namesRegistryDependency reports whether the raw manifest's
// extensionDependencies names a registry extension (proto.IsRegistryName). A
// value that does not decode counts, so the strict decode reports it instead
// of the archive shipping unstamped.
func namesRegistryDependency(raw map[string]any) bool {
	value, ok := raw["extensionDependencies"]
	if !ok || value == nil {
		return false
	}
	data, err := json.Marshal(value)
	if err != nil {
		return true
	}
	var deps proto.Dependencies
	if err := json.Unmarshal(data, &deps); err != nil {
		return true
	}
	for name := range deps.List {
		if proto.IsRegistryName(name) {
			return true
		}
	}
	return false
}

// surfaceNeedsGate reports whether a raw command, command-group, or tool value
// requires the contract gate. A missing key (nil) and an empty JSON object are
// hook-only and skip the gate; a non-empty object — or any non-object type —
// must be gated.
func surfaceNeedsGate(v any) bool {
	if v == nil {
		return false
	}
	if obj, ok := v.(map[string]any); ok {
		return len(obj) > 0
	}
	return true
}
