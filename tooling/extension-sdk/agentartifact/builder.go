// Package agentartifact builds the agent-artifact tree of an extension's
// agent-content contribution from its closed authoring layout and its declared
// content policy. The extension supplies the identity, the layout content and
// the publication rule.
//
// It lives in the extension SDK because two callers must emit identical bytes:
// the packagers, which ship the built tree inside the extension
// (PackageExtension, StageExtensionContent), and the CLI, which builds the
// authored content of an extension declared by path. One builder is what keeps
// a path-declared workspace and an installed release byte-equal.
package agentartifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
)

// Schema is the published protocol-v1 manifest schema.
const Schema = "https://putnami.dev/schemas/putnami-agent-artifact.json"

// Result is a complete in-memory registry artifact. Files excludes the
// manifest because the manifest describes workspace targets and is never
// materialized into a workspace.
type Result struct {
	// Name is the artifact identity, read from the project that owns the
	// source tree. The builder holds no artifact name of its own.
	Name           string
	Archive        []byte
	ArchiveSHA256  string
	Manifest       []byte
	ManifestSHA256 string
	Files          map[string][]byte
}

// Build reads the project identity, its declared content policy, and its
// closed source tree beneath projectRoot, then returns deterministic
// protocol-v1 bytes for that one artifact.
func Build(projectRoot, version string) (*Result, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return nil, fmt.Errorf("artifact version is required")
	}
	cfg, diags := wsproto.LoadProjectConfigWithDiagnostics(projectRoot)
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("read agent artifact project config: %v", diags)
	}
	if cfg == nil {
		return nil, fmt.Errorf("agent artifact project %s has no readable putnami.json", projectRoot)
	}
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		return nil, fmt.Errorf("agent artifact project %s declares no name", projectRoot)
	}
	policy, err := loadContentPolicy(cfg)
	if err != nil {
		return nil, err
	}
	return build(name, version, policy, filepath.Join(projectRoot, "src"), "src")
}

// build emits the deterministic protocol-v1 artifact for one validated
// authoring layout. Every entry point goes through it, so an agent-artifact
// project and an extension's contribution cannot drift apart in how bytes are
// emitted, checked against the policy, or archived.
func build(name, version string, policy contentPolicy, sourceRoot, label string) (*Result, error) {
	tree, err := readSourceTree(sourceRoot, label)
	if err != nil {
		return nil, err
	}
	files, err := emitFiles(tree)
	if err != nil {
		return nil, err
	}
	if err := policy.apply(files, tree.skills); err != nil {
		return nil, err
	}

	manifest := &wsproto.AgentArtifactManifest{
		Schema:          Schema,
		ProtocolVersion: wsproto.AgentArtifactManifestProtocolVersion,
		Name:            name,
		Version:         version,
		Files:           make([]wsproto.AgentArtifactFile, 0, len(files)),
	}
	for _, path := range sortedFileNames(files) {
		manifest.Files = append(manifest.Files, wsproto.AgentArtifactFile{
			Path:   path,
			SHA256: hash(files[path]),
		})
	}
	manifestBytes, err := wsproto.MarshalAgentArtifactManifest(manifest)
	if err != nil {
		return nil, err
	}
	members := cloneFiles(files)
	members[wsproto.AgentArtifactManifestFilename] = append([]byte(nil), manifestBytes...)
	archiveBytes, err := archiveMembers(members)
	if err != nil {
		return nil, err
	}
	return &Result{
		Name:           name,
		Archive:        archiveBytes,
		ArchiveSHA256:  hash(archiveBytes),
		Manifest:       manifestBytes,
		ManifestSHA256: hash(manifestBytes),
		Files:          cloneFiles(files),
	}, nil
}

// writeArchiveFile atomically replaces path with archive: a synced temporary
// file in the same directory, mode 0644, renamed into place.
func writeArchiveFile(path string, archive []byte) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("artifact output path is required")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create artifact output directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".agent-workflows-*.tmp")
	if err != nil {
		return fmt.Errorf("stage artifact archive: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set artifact archive mode: %w", err)
	}
	if _, err := tmp.Write(archive); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write artifact archive: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync artifact archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close artifact archive: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("publish artifact archive: %w", err)
	}
	return nil
}

// archiveMembers writes members into a reproducible tar.gz: sorted names,
// regular files only, a fixed mode, and every timestamp at the epoch, so equal
// members always produce equal bytes. It refuses members that no directory
// tree can hold: one whose name is a directory of another.
func archiveMembers(members map[string][]byte) ([]byte, error) {
	for name := range members {
		for end := strings.LastIndexByte(name, '/'); end > 0; end = strings.LastIndexByte(name[:end], '/') {
			if _, file := members[name[:end]]; file {
				return nil, fmt.Errorf("archive member %s lies under %s, which is itself a member: one path cannot be a file and a directory", name, name[:end])
			}
		}
	}
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	gzipWriter.Header.ModTime = time.Unix(0, 0).UTC()
	gzipWriter.Header.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)
	for _, name := range sortedFileNames(members) {
		content := members[name]
		header := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			ModTime:  time.Unix(0, 0).UTC(),
			Typeflag: tar.TypeReg,
			Format:   tar.FormatUSTAR,
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			return nil, fmt.Errorf("write archive header %s: %w", name, err)
		}
		if _, err := tarWriter.Write(content); err != nil {
			return nil, fmt.Errorf("write archive member %s: %w", name, err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		return nil, fmt.Errorf("close tar archive: %w", err)
	}
	if err := gzipWriter.Close(); err != nil {
		return nil, fmt.Errorf("close gzip archive: %w", err)
	}
	return buffer.Bytes(), nil
}

func sortedFileNames(files map[string][]byte) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func cloneFiles(files map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(files))
	for name, content := range files {
		out[name] = append([]byte(nil), content...)
	}
	return out
}

func hash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// ReadArchive is kept small and deterministic for release tooling and tests.
// It rejects duplicate, non-regular, and undeclared members.
func ReadArchive(content []byte) (map[string][]byte, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(content))
	if err != nil {
		return nil, fmt.Errorf("open gzip archive: %w", err)
	}
	defer func() { _ = gzipReader.Close() }()
	tarReader := tar.NewReader(gzipReader)
	members := make(map[string][]byte)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar archive: %w", err)
		}
		if !header.FileInfo().Mode().IsRegular() {
			return nil, fmt.Errorf("archive member %q is not a regular file", header.Name)
		}
		if _, exists := members[header.Name]; exists {
			return nil, fmt.Errorf("duplicate archive member %q", header.Name)
		}
		member, err := io.ReadAll(tarReader)
		if err != nil {
			return nil, fmt.Errorf("read archive member %q: %w", header.Name, err)
		}
		members[header.Name] = member
	}
	manifestBytes, ok := members[wsproto.AgentArtifactManifestFilename]
	if !ok {
		return nil, fmt.Errorf("archive has no %s", wsproto.AgentArtifactManifestFilename)
	}
	manifest, diags := wsproto.ParseAndValidateAgentArtifactManifest(manifestBytes)
	if len(diags) > 0 {
		return nil, fmt.Errorf("archive has an invalid agent artifact manifest: %v", diags)
	}
	declared := make(map[string]string, len(manifest.Files))
	for _, file := range manifest.Files {
		declared[file.Path] = file.SHA256
	}
	for name, member := range members {
		if name == wsproto.AgentArtifactManifestFilename {
			continue
		}
		want, ok := declared[name]
		if !ok {
			return nil, fmt.Errorf("undeclared archive member %q", name)
		}
		if got := hash(member); got != want {
			return nil, fmt.Errorf("archive member %q hashes to %s, manifest declares %s", name, got, want)
		}
	}
	if len(members)-1 != len(declared) {
		return nil, fmt.Errorf("archive contains %d workflow files, manifest declares %d", len(members)-1, len(declared))
	}
	return members, nil
}
