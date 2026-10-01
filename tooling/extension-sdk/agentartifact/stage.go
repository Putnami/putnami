package agentartifact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
)

// Staged agent content: the package step of an extension whose language
// extension also ships an executable (a Go archive, an npm package).
//
// Such a packager stages the extension's files into a directory, stamps the
// version into the staged putnami.extension.json, and archives the directory.
// StageExtensionContent is the one step it adds for agent content: it builds
// the authored contribution with BuildExtensionContent, writes the built tree
// under agentContent.path in the stage, and rewrites the staged manifest to the
// packaged form. VerifyStagedExtensionContent is the matching postcondition the
// packager's contract gate runs before anything is archived.
//
// The content is built from source files only, never from the platform being
// packaged, so every platform archive a packager derives from one stage
// carries identical content bytes.

// Modes of the staged content. They are set explicitly, never inherited from
// the umask, because archive writers record them.
const (
	stagedContentDirMode  fs.FileMode = 0o755
	stagedContentFileMode fs.FileMode = 0o644
)

// StageExtensionContent builds the agent content that the manifest staged in
// stageDir declares and writes it into stageDir. It returns nil, and leaves
// every staged byte untouched, when the manifest declares no agentContent.
//
// extensionRoot is the extension's source directory, which holds the authored
// layout and the content policy (putnami.json options.agent-artifact). name is
// the name the packager publishes the extension under: the content carries the
// manifest's own name when it declares one, which must then be the same name,
// because the CLI binds agent content to the name it resolves. version is the
// version the packager stamped.
//
// It refuses, before writing anything:
//   - a manifest that does not parse strictly, or whose contribution is invalid;
//   - a contribution without an authored source: the digest is written by the
//     package step, never by the author;
//   - a content path the stage already holds, or one whose parent is a symlink
//     or a file: the built tree is never merged into staged files;
//   - any content the declared policy or the closed layout rejects.
func StageExtensionContent(extensionRoot, stageDir, name, version string) (*Result, error) {
	manifestPath := filepath.Join(stageDir, extproto.ManifestFilename)
	staged, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read staged extension manifest: %w", err)
	}
	declared, err := declaresAgentContentField(staged)
	if err != nil {
		return nil, err
	}
	if !declared {
		return nil, nil
	}
	m, diags := extproto.ParseManifest(staged)
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("staged extension manifest does not parse strictly, so its agent content cannot be packaged: %s", formatDiagnosticList(diags))
	}
	if errs := diag.Errors(extproto.ValidateAgentContent(m)); len(errs) > 0 {
		return nil, fmt.Errorf("staged extension manifest declares an invalid agent-content contribution: %s", formatDiagnosticList(errs))
	}
	identity, err := contentIdentity(m.Name, name)
	if err != nil {
		return nil, err
	}
	contribution := *m.AgentContent
	if contribution.Source == "" {
		return nil, fmt.Errorf("extension %s declares agentContent without a source: the package step builds the content from agentContent.source and writes agentContent.manifestSha256 itself", identity)
	}
	version = strings.TrimSpace(version)
	if version == "" {
		return nil, fmt.Errorf("extension %s has no version to package its agent content under", identity)
	}
	if stagedVersion := strings.TrimSpace(m.Version); stagedVersion != "" && stagedVersion != version {
		return nil, fmt.Errorf("staged extension manifest of %s declares version %s, but its agent content is packaged at %s", identity, stagedVersion, version)
	}
	if err := refuseOccupiedContentPath(stageDir, contribution.Path); err != nil {
		return nil, fmt.Errorf("extension %s: %w", identity, err)
	}

	content, err := BuildExtensionContent(extensionRoot, contribution.Source, identity, version)
	if err != nil {
		return nil, err
	}
	rewritten, err := packagedManifest(staged, contribution, content.ManifestSHA256)
	if err != nil {
		return nil, err
	}
	if err := writeContentTree(stageDir, contribution.Path, content); err != nil {
		return nil, fmt.Errorf("stage the agent content of %s: %w", identity, err)
	}
	if err := os.WriteFile(manifestPath, rewritten, stagedContentFileMode); err != nil {
		return nil, fmt.Errorf("rewrite staged extension manifest: %w", err)
	}
	return content, nil
}

// VerifyStagedExtensionContent checks that a staged extension ships exactly the
// agent content its manifest binds. It returns nil when the manifest declares
// no agentContent.
//
// The contribution must be in its packaged form; the content manifest under
// agentContent.path must hash to agentContent.manifestSha256 and pass the
// agent-artifact protocol; every file it declares must be a regular file with
// the declared digest; and the tree must hold nothing else. A staged manifest
// that declares a version must match the content manifest's version.
func VerifyStagedExtensionContent(stageDir string) error {
	staged, err := os.ReadFile(filepath.Join(stageDir, extproto.ManifestFilename))
	if err != nil {
		return fmt.Errorf("read staged extension manifest: %w", err)
	}
	declared, err := declaresAgentContentField(staged)
	if err != nil || !declared {
		return err
	}
	m, diags := extproto.ParseManifest(staged)
	if diag.HasErrors(diags) {
		return fmt.Errorf("staged extension manifest does not parse strictly: %s", formatDiagnosticList(diags))
	}
	if errs := diag.Errors(extproto.ValidateAgentContent(m)); len(errs) > 0 {
		return fmt.Errorf("staged extension manifest declares an invalid agent-content contribution: %s", formatDiagnosticList(errs))
	}
	contribution := *m.AgentContent
	if !contribution.Packaged() {
		return fmt.Errorf("staged extension manifest carries its agent content as source (%s): a package binds the built tree by agentContent.manifestSha256", contribution.Source)
	}
	root := filepath.Join(stageDir, filepath.FromSlash(contribution.Path))
	manifestBytes, err := readRegularFile(filepath.Join(root, wsproto.AgentArtifactManifestFilename))
	if err != nil {
		return fmt.Errorf("staged agent content has no readable %s: %w", wsproto.AgentArtifactManifestFilename, err)
	}
	if got := hash(manifestBytes); got != contribution.ManifestSHA256 {
		return fmt.Errorf("staged agent content manifest hashes to %s, but agentContent.manifestSha256 binds %s", got, contribution.ManifestSHA256)
	}
	contentManifest, diags := wsproto.ParseAndValidateAgentArtifactManifest(manifestBytes)
	if diag.HasErrors(diags) {
		return fmt.Errorf("staged agent content manifest is invalid: %s", formatDiagnosticList(diags))
	}
	if stagedVersion := strings.TrimSpace(m.Version); stagedVersion != "" && contentManifest.Version != stagedVersion {
		return fmt.Errorf("staged agent content is version %s, but the extension manifest declares %s", contentManifest.Version, stagedVersion)
	}
	declaredFiles := make(map[string]string, len(contentManifest.Files)+1)
	declaredFiles[wsproto.AgentArtifactManifestFilename] = contribution.ManifestSHA256
	for _, file := range contentManifest.Files {
		declaredFiles[file.Path] = file.SHA256
	}
	err = filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("staged agent content %s is a symlink", rel)
		}
		if entry.IsDir() {
			return nil
		}
		want, ok := declaredFiles[rel]
		if !ok {
			return fmt.Errorf("staged agent content holds %s, which its manifest does not declare", rel)
		}
		data, err := readRegularFile(current)
		if err != nil {
			return err
		}
		if got := hash(data); got != want {
			return fmt.Errorf("staged agent content %s hashes to %s, but its manifest declares %s", rel, got, want)
		}
		delete(declaredFiles, rel)
		return nil
	})
	if err != nil {
		return err
	}
	if missing := sortedDigestNames(declaredFiles); len(missing) > 0 {
		return fmt.Errorf("staged agent content lacks %s, which its manifest declares", missing[0])
	}
	return nil
}

func sortedDigestNames(digests map[string]string) []string {
	names := make([]string, 0, len(digests))
	for name := range digests {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// declaresAgentContentField reports whether a manifest names agentContent with
// a non-null value, reading nothing else, so a manifest without the section is
// never reinterpreted.
func declaresAgentContentField(data []byte) (bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return false, fmt.Errorf("parse staged extension manifest: %w", err)
	}
	raw, ok := fields["agentContent"]
	return ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")), nil
}

// contentIdentity is the name agent content is built under: the manifest's own
// name when it declares one, otherwise the name the packager publishes.
func contentIdentity(manifestName, publishedName string) (string, error) {
	manifestName = strings.TrimSpace(manifestName)
	publishedName = strings.TrimSpace(publishedName)
	switch {
	case manifestName != "" && publishedName != "" && manifestName != publishedName:
		return "", fmt.Errorf("extension manifest names %s, but the package publishes it as %s: the CLI binds agent content to the name it resolves, so the two must agree", manifestName, publishedName)
	case manifestName != "":
		return manifestName, nil
	case publishedName != "":
		return publishedName, nil
	default:
		return "", fmt.Errorf("extension declares agent content but has no name to publish it under")
	}
}

// refuseOccupiedContentPath refuses a content path the stage already holds, or
// one below an entry that is a symlink or a file.
func refuseOccupiedContentPath(stageDir, contentPath string) error {
	current := stageDir
	parts := strings.Split(contentPath, "/")
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect staged %s: %w", path.Join(parts[:index+1]...), err)
		}
		if index == len(parts)-1 {
			return fmt.Errorf("agentContent.path %q is already staged: the package step writes the built content there and never merges it with other files", contentPath)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("agentContent.path %q lies below staged %s, which is a symlink or a file", contentPath, path.Join(parts[:index+1]...))
		}
	}
	return nil
}

// packagedManifest returns the staged manifest with its contribution in the
// packaged form: path, the digest of what was built and, unchanged, the
// superseded identities. Every other field is kept; the encoding is the
// sorted-key indented form packagers stamp.
func packagedManifest(staged []byte, contribution extproto.AgentContentContribution, manifestSHA256 string) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(staged, &raw); err != nil {
		return nil, fmt.Errorf("parse staged extension manifest: %w", err)
	}
	packaged := map[string]any{
		"path":           contribution.Path,
		"manifestSha256": manifestSHA256,
	}
	if len(contribution.Supersedes) > 0 {
		packaged["supersedes"] = contribution.Supersedes
	}
	raw["agentContent"] = packaged
	rewritten, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode the packaged extension manifest: %w", err)
	}
	return append(rewritten, '\n'), nil
}

// writeContentTree writes the built content manifest and every file it
// declares beneath stageDir/contentPath, with fixed modes.
func writeContentTree(stageDir, contentPath string, content *Result) error {
	members := cloneFiles(content.Files)
	members[wsproto.AgentArtifactManifestFilename] = append([]byte(nil), content.Manifest...)
	for _, name := range sortedFileNames(members) {
		rel := path.Join(contentPath, name)
		if err := mkdirAllWithMode(stageDir, path.Dir(rel)); err != nil {
			return err
		}
		target := filepath.Join(stageDir, filepath.FromSlash(rel))
		if err := os.WriteFile(target, members[name], stagedContentFileMode); err != nil {
			return err
		}
		if err := os.Chmod(target, stagedContentFileMode); err != nil {
			return err
		}
	}
	return nil
}

// mkdirAllWithMode creates every missing directory of the slash-separated rel
// beneath base with stagedContentDirMode. A directory that already exists is
// left as it is.
func mkdirAllWithMode(base, rel string) error {
	current := base
	for _, part := range strings.Split(rel, "/") {
		current = filepath.Join(current, part)
		if _, err := os.Lstat(current); err == nil {
			continue
		}
		if err := os.Mkdir(current, stagedContentDirMode); err != nil {
			return err
		}
		if err := os.Chmod(current, stagedContentDirMode); err != nil {
			return err
		}
	}
	return nil
}

// readRegularFile reads path only when it is a regular file.
func readRegularFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return os.ReadFile(path)
}
