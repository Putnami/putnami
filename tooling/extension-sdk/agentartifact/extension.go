package agentartifact

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
)

// BuildExtensionContent builds the agent-content contribution one extension
// declares: the closed authoring layout (`skills/`, `agents/`) beneath
// extensionRoot/source, checked against the content policy the extension's
// project config declares (`options.agent-artifact`), and emitted under the
// extension's own identity and version.
//
// The identity is the extension's, never the project's: the CLI binds the
// content to the extension it resolved, and refuses a tree whose manifest names
// anything else.
func BuildExtensionContent(extensionRoot, source, name, version string) (*Result, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return nil, fmt.Errorf("agent content version is required")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("agent content of %s has no extension name", extensionRoot)
	}
	clean, err := extproto.NormalizeRelativePath(source)
	if err != nil {
		return nil, fmt.Errorf("agent content source %q of %s: %w", source, name, err)
	}
	if clean != source {
		return nil, fmt.Errorf("agent content source %q of %s must be written in canonical form %q", source, name, clean)
	}
	cfg, diags := wsproto.LoadProjectConfigWithDiagnostics(extensionRoot)
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("read the project config of extension %s: %v", name, diags)
	}
	if cfg == nil {
		return nil, fmt.Errorf("extension %s has no readable putnami.json declaring its options.agent-artifact content policy", name)
	}
	policy, err := loadContentPolicy(cfg)
	if err != nil {
		return nil, err
	}
	return build(name, version, policy, filepath.Join(extensionRoot, filepath.FromSlash(source)), source)
}

// ExtensionPackage is one content-only extension packaged for a registry.
type ExtensionPackage struct {
	// Name and Version are the packaged extension's identity.
	Name    string
	Version string
	// Manifest is the stamped putnami.extension.json: the version, the
	// packaged agent-content form, and the contract its vocabulary requires.
	Manifest []byte
	// Content is the built contribution the manifest binds by digest.
	Content *Result
	// Members are every archive member by archive path: the manifest, the
	// content's agent-artifact manifest and every file it declares.
	Members map[string][]byte
	// Archive is the reproducible tar.gz of Members, and ArchiveSHA256 its
	// digest.
	Archive       []byte
	ArchiveSHA256 string
}

// PackageExtension packages the content-only extension whose source lives at
// extensionRoot, at version.
//
// It is the package-time gate for an agent-content contribution, and it has
// the same postcondition as every other gate: what it returns loads under this
// build's loader. In order it
//
//  1. strictly parses the authored manifest and requires the authored
//     contribution form (`agentContent.source`) on an extension that runs
//     nothing — an extension with a runtime, commands, tools, tasks, hooks or
//     a workspace adapter is packaged by its language extension, which ships
//     the executable those need;
//  2. builds the contribution with BuildExtensionContent, which applies the
//     declared content policy;
//  3. stamps the manifest: the version, the packaged form (`path` plus the
//     digest of what was built, never the source), and the contract the
//     manifest's vocabulary requires (extension.RequiredCLIContract), refusing
//     an authored claim above the latest contract this build can certify;
//  4. validates the stamped manifest strictly and negotiates it through the
//     loader, so a package this build could not read is never returned.
//
// The archive is platform independent: the same bytes serve every host.
func PackageExtension(extensionRoot, version string) (*ExtensionPackage, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return nil, fmt.Errorf("extension version is required")
	}
	manifestPath := filepath.Join(extensionRoot, extproto.ManifestFilename)
	authored, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read extension manifest: %w", err)
	}
	m, diags := extproto.ParseManifest(authored)
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("extension manifest %s does not parse strictly: %s", manifestPath, formatDiagnosticList(diags))
	}
	name := strings.TrimSpace(m.Name)
	if name == "" {
		return nil, fmt.Errorf("extension manifest %s declares no name; the agent content is published under it", manifestPath)
	}
	if !m.DeclaresAgentContent() {
		return nil, fmt.Errorf("extension %s declares no agentContent to package", name)
	}
	if surfaces := executableSurfaces(m); len(surfaces) > 0 {
		return nil, fmt.Errorf("extension %s declares %s: only a content-only extension is packaged here; its language extension packages the executable those need",
			name, strings.Join(surfaces, ", "))
	}
	if errs := diag.Errors(extproto.FullValidateManifest(m)); len(errs) > 0 {
		return nil, fmt.Errorf("extension manifest of %s is invalid: %s", name, formatDiagnosticList(errs))
	}
	contribution := *m.AgentContent
	if contribution.Source == "" {
		return nil, fmt.Errorf("extension %s declares no agentContent.source to build its content from", name)
	}
	if m.CLIContract > protocolcli.LatestContract {
		return nil, fmt.Errorf("extension %s claims CLI contract %d, but this build certifies at most %d: upgrade putnami to package it",
			name, m.CLIContract, protocolcli.LatestContract)
	}

	content, err := BuildExtensionContent(extensionRoot, contribution.Source, name, version)
	if err != nil {
		return nil, err
	}

	var raw map[string]any
	if err := json.Unmarshal(authored, &raw); err != nil {
		return nil, fmt.Errorf("decode extension manifest: %w", err)
	}
	raw["version"] = version
	packagedContent := map[string]any{
		"path":           contribution.Path,
		"manifestSha256": content.ManifestSHA256,
	}
	// The superseded identities are the author's statement about what this
	// content replaces; a published release carries them unchanged.
	if len(contribution.Supersedes) > 0 {
		packagedContent["supersedes"] = contribution.Supersedes
	}
	raw["agentContent"] = packagedContent
	raw["cliContract"] = extproto.RequiredCLIContract(m)
	stamped, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode the stamped extension manifest: %w", err)
	}
	stamped = append(stamped, '\n')

	packaged, diags := extproto.ParseManifest(stamped)
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("stamped extension manifest does not parse: %s", formatDiagnosticList(diags))
	}
	if errs := diag.Errors(extproto.FullValidateManifest(packaged)); len(errs) > 0 {
		return nil, fmt.Errorf("stamped extension manifest of %s is invalid and cannot be published: %s", name, formatDiagnosticList(errs))
	}
	if _, err := extproto.NegotiateManifest(extproto.ManifestFilename, stamped); err != nil {
		return nil, fmt.Errorf("stamped extension manifest of %s cannot be published: this putnami's own loader rejects it: %w", name, err)
	}

	// Every member has its own path, and none lies under another: the content
	// tree never overwrites or nests inside the manifest.
	members := map[string][]byte{extproto.ManifestFilename: stamped}
	contentMembers := map[string][]byte{wsproto.AgentArtifactManifestFilename: content.Manifest}
	for file, data := range content.Files {
		contentMembers[file] = data
	}
	for file, data := range contentMembers {
		member := path.Join(contribution.Path, file)
		if _, taken := members[member]; taken {
			return nil, fmt.Errorf("extension %s: agentContent.path %q puts its content at %s, which the package already holds", name, contribution.Path, member)
		}
		members[member] = append([]byte(nil), data...)
	}
	archive, err := archiveMembers(members)
	if err != nil {
		return nil, fmt.Errorf("extension %s: agentContent.path %q: %w", name, contribution.Path, err)
	}
	return &ExtensionPackage{
		Name:          name,
		Version:       version,
		Manifest:      stamped,
		Content:       content,
		Members:       members,
		Archive:       archive,
		ArchiveSHA256: hash(archive),
	}, nil
}

// WriteExtensionArchive atomically writes the packaged extension's archive to
// path. It never publishes to a registry; that remains a separate release
// approval.
func WriteExtensionArchive(path string, pkg *ExtensionPackage) error {
	if pkg == nil || len(pkg.Archive) == 0 {
		return fmt.Errorf("extension package is empty")
	}
	return writeArchiveFile(path, pkg.Archive)
}

// executableSurfaces names every manifest section that needs something other
// than agent content to be shipped, in a fixed order.
func executableSurfaces(m *extproto.Manifest) []string {
	var surfaces []string
	if m.DeclaresRuntime() {
		surfaces = append(surfaces, "a runtime")
	}
	if len(m.Commands) > 0 {
		surfaces = append(surfaces, "commands")
	}
	if len(m.CommandGroups) > 0 {
		surfaces = append(surfaces, "command groups")
	}
	if len(m.Tools) > 0 {
		surfaces = append(surfaces, "MCP tools")
	}
	if len(m.Tasks) > 0 {
		surfaces = append(surfaces, "tasks")
	}
	if m.Hooks != nil {
		surfaces = append(surfaces, "hooks")
	}
	if m.DeclaresWorkspaceAdapter() {
		surfaces = append(surfaces, "a workspace adapter")
	}
	return surfaces
}

func formatDiagnosticList(diags []diag.Diagnostic) string {
	parts := make([]string, 0, len(diags))
	for _, d := range diag.Errors(diags) {
		parts = append(parts, d.String())
	}
	return strings.Join(parts, "; ")
}
