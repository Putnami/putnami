// Package pkgmeta owns the package→publish file contract
// (.putnami/out/{project}/package/…): the readers the relocated publishers use,
// and the one writer every ecosystem packager records its channel through.
//
// Every packager writes INSIDE the directory it owns. A channel record lives at
// <channel>/channel.json, so the fact "this channel was packaged at version X"
// travels with the artifact it describes: a cache restore of go/ restores that
// channel's record atomically with its bytes, and no packager can erase a
// sibling's. The channel index publishers read is DERIVED from those records
// (ReadChannelIndex), not maintained as a shared file.
//
// metadata.json is the one remaining file at the package root. It is not an
// index and not a merge point: it is the archive publication manifest, the
// single-owner declared output of the archive packager, and its reader is the
// archive uploader — see PackageMetadata.
package pkgmeta

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"go.putnami.dev/sdk/extension/robustio"
)

// PackageMetadata is the ARCHIVE PUBLICATION MANIFEST at
// .putnami/out/{project}/package/metadata.json.
//
// It is written by the archive packager alone — the one task that declares it as
// an owned output — and it describes the archive set sitting in the sibling
// archives/ directory: the version to publish under, the artifact name the
// archive filenames are keyed by, whether the release is stable, the archive
// channel produced, and whether the artifact is a template (which the uploader
// fans out to every platform from one blob).
//
// ITS READER IS OUT OF TREE. `@putnami/cloud`'s `publish-archives`
// (distribution/libs/cli/publish_archives.go) reads exactly these five fields
// from exactly this path, and its task runs with --if-present, so a missing or
// channel-less file is a SILENT SKIP rather than a failed publication. That is
// why the file survived with an owner instead of being replaced by the
// derived channel index: deleting it would have stopped every archive
// publication of this repository without reddening a task. Changing its path,
// its field names or its semantics requires moving that reader first.
type PackageMetadata struct {
	Version  string   `json:"version"`
	Artifact string   `json:"artifact"`
	Stable   bool     `json:"stable"`
	Channels []string `json:"channels"`
	Template bool     `json:"template,omitempty"`
}

// HasChannel returns true if the manifest includes the given channel.
func (m *PackageMetadata) HasChannel(channel string) bool {
	return slices.Contains(m.Channels, channel)
}

// DockerManifest is the package-owned local image candidate contract at
// .putnami/out/{project}/package/docker/manifest.json. Version is advisory: the
// image identity is ContentHash (the package step tags the candidate
// c-<contentHash>), and publish derives release identity from its own session.
// Digest is the locally computed image digest and Layout is the OCI layout
// directory relative to the docker package dir. Neither field is publication
// evidence: only a publish-owned PublishedImageManifest may claim a remote
// immutable reference or registry verification.
type DockerManifest struct {
	Image       string   `json:"image"`
	Tags        []string `json:"tags"`
	Version     string   `json:"version"`
	ContentHash string   `json:"contentHash"`
	Digest      string   `json:"digest"`
	Layout      string   `json:"layout"`
	Platform    string   `json:"platform,omitempty"`
}

// PublishedImageManifest is the publish-owned remote evidence contract at
// .putnami/out/{project}/publish/docker/published-image.json. ImmutableRef is
// present only after the publisher has read the exact digest back from the
// target registry and Verified is therefore always true in a written record.
type PublishedImageManifest struct {
	ProtocolVersion int    `json:"protocolVersion"`
	Image           string `json:"image"`
	ImmutableRef    string `json:"immutableRef"`
	Digest          string `json:"digest"`
	CandidateDigest string `json:"candidateDigest"`
	ContentHash     string `json:"contentHash,omitempty"`
	Platform        string `json:"platform,omitempty"`
	TargetRegistry  string `json:"targetRegistry"`
	Verified        bool   `json:"verified"`
}

// GoModuleMetadata represents .putnami/out/{project}/package/go/module.json.
type GoModuleMetadata struct {
	ModulePath string `json:"modulePath"`
	Version    string `json:"version"`
	SourceDir  string `json:"sourceDir"`
	ZipPath    string `json:"zipPath,omitempty"`
	ModPath    string `json:"modPath,omitempty"`
	InfoPath   string `json:"infoPath,omitempty"`
}

// PackageOutputDir returns the path to .putnami/out/{project}/package/{channel}/.
// projectPath is the project's workspace-relative path (e.g. "typescript/framework/database").
func PackageOutputDir(workspaceRoot, projectPath, channel string) string {
	return filepath.Join(workspaceRoot, ".putnami", "out", projectPath, "package", channel)
}

// MetadataPath returns the path to .putnami/out/{project}/package/metadata.json,
// the archive publication manifest. projectPath is the project's
// workspace-relative path (e.g. "typescript/framework/database").
func MetadataPath(workspaceRoot, projectPath string) string {
	return filepath.Join(workspaceRoot, ".putnami", "out", projectPath, "package", "metadata.json")
}

// ReadPackageMetadata reads and parses the archive publication manifest.
func ReadPackageMetadata(workspaceRoot, projectPath string) (*PackageMetadata, error) {
	path := MetadataPath(workspaceRoot, projectPath)
	return readJSON[PackageMetadata](path)
}

// WritePackageMetadata writes the archive publication manifest atomically.
//
// It OVERWRITES: the archive packager is the manifest's only writer and states
// the whole document every time it runs, so there is nothing of a sibling's to
// preserve and no read-merge-write for two parallel packagers to race on. A
// restore of the packager's cache entry replaces the same bytes.
func WritePackageMetadata(workspaceRoot, projectPath string, metadata PackageMetadata) error {
	return writeJSONAtomic(MetadataPath(workspaceRoot, projectPath), metadata)
}

// ChannelRecordFile is the name of the record a packager writes inside the
// output directory it owns. One name, defined once, so every ecosystem writes
// and every reader derives the index from the same file.
const ChannelRecordFile = "channel.json"

// ChannelRecord is what one packager states about the output it owns: the
// release identity it packaged under, the channels it produced there, and any
// extra key it alone owns (the template packager's "template" marker).
type ChannelRecord struct {
	Version  string         `json:"version,omitempty"`
	Artifact string         `json:"artifact,omitempty"`
	Stable   bool           `json:"stable,omitempty"`
	Channels []string       `json:"channels"`
	Extra    map[string]any `json:"extra,omitempty"`
}

// ChannelIndex is the derived answer to "which channels did packaging produce
// for this project", built by ReadChannelIndex over the per-channel records. It
// is never a file: there is nothing to keep in step, and no writer can erase
// another's contribution because no writer shares a path with another.
type ChannelIndex struct {
	// Channels is every channel name any record reported, in the directory
	// order the records were read in and then record order, without duplicates.
	Channels []string
	// Records is each record keyed by the name of the directory that owns it.
	Records map[string]ChannelRecord
}

// HasChannel returns true if any record reported the given channel.
func (i *ChannelIndex) HasChannel(channel string) bool {
	return slices.Contains(i.Channels, channel)
}

// WriteChannelRecord writes record to <outputDir>/channel.json, atomically.
//
// outputDir is the packager's declared command-output directory —
// PackageOutputDir(ws, project, "go"), ".../archives", ".../docker",
// ".../npm". Writing inside the owned directory is what makes the record
// cacheable: it is captured and restored with the artifact it describes, by the
// same declared output, so a restore cannot leave the artifact present and the
// channel unrecorded.
func WriteChannelRecord(outputDir string, record ChannelRecord) error {
	return writeJSONAtomic(filepath.Join(outputDir, ChannelRecordFile), record)
}

// ReadChannelIndex derives the project's channel index from every
// <package>/*/channel.json, in sorted directory order.
//
// A record left by an earlier run in a directory this run did not rebuild is
// still reported — exactly as the shared index reported it before — because
// the artifact it describes is still on disk. A directory without a record, or
// with an unreadable or malformed one, contributes nothing rather than failing:
// the answer is derived from what packaging actually produced.
//
// It returns an error wrapping fs.ErrNotExist when no record exists anywhere
// under <package>/, which is the "run package first" answer publish relies on.
func ReadChannelIndex(workspaceRoot, projectPath string) (*ChannelIndex, error) {
	packageDir := filepath.Join(workspaceRoot, ".putnami", "out", projectPath, "package")
	entries, err := os.ReadDir(packageDir)
	if err != nil {
		// A missing package directory already wraps fs.ErrNotExist, so the
		// "run package first" answer needs no special case here.
		return nil, fmt.Errorf("reading %s: %w", packageDir, err)
	}

	index := &ChannelIndex{Records: map[string]ChannelRecord{}}
	seen := map[string]bool{}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		record, err := readJSON[ChannelRecord](filepath.Join(packageDir, name, ChannelRecordFile))
		if err != nil {
			continue
		}
		index.Records[name] = *record
		for _, channel := range record.Channels {
			if channel == "" || seen[channel] {
				continue
			}
			seen[channel] = true
			index.Channels = append(index.Channels, channel)
		}
	}
	if len(index.Records) == 0 {
		return nil, fmt.Errorf("no channel record under %s: %w", packageDir, fs.ErrNotExist)
	}
	return index, nil
}

// ReadDockerManifest reads and parses the Docker manifest.
func ReadDockerManifest(workspaceRoot, projectPath string) (*DockerManifest, error) {
	path := filepath.Join(PackageOutputDir(workspaceRoot, projectPath, "docker"), "manifest.json")
	return readJSON[DockerManifest](path)
}

// PublishedImageManifestPath returns the publish task's owned evidence path.
// publishOutputDir is the job context's command-output directory.
func PublishedImageManifestPath(publishOutputDir string) string {
	return filepath.Join(publishOutputDir, "docker", "published-image.json")
}

// WritePublishedImageManifest atomically writes verified publish evidence.
func WritePublishedImageManifest(publishOutputDir string, manifest PublishedImageManifest) error {
	if manifest.ProtocolVersion == 0 {
		manifest.ProtocolVersion = 1
	}
	if manifest.ImmutableRef == "" || manifest.Digest == "" || !manifest.Verified {
		return fmt.Errorf("published image manifest requires a verified immutable reference")
	}
	filename := PublishedImageManifestPath(publishOutputDir)
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", filename, err)
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(filename), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(filename), ".published-image-*.tmp")
	if err != nil {
		return fmt.Errorf("staging %s: %w", filename, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return robustio.Rename(tmpName, filename)
}

// ReadPublishedImageManifest reads the publish-owned verified image record.
func ReadPublishedImageManifest(publishOutputDir string) (*PublishedImageManifest, error) {
	return readJSON[PublishedImageManifest](PublishedImageManifestPath(publishOutputDir))
}

// ReadGoModuleMetadata reads and parses the Go module metadata.
func ReadGoModuleMetadata(workspaceRoot, projectPath string) (*GoModuleMetadata, error) {
	path := filepath.Join(PackageOutputDir(workspaceRoot, projectPath, "go"), "module.json")
	return readJSON[GoModuleMetadata](path)
}

// writeJSONAtomic writes value as indented JSON to path through a
// same-directory temp file and a rename, so a reader (or a concurrent capture
// of the enclosing declared output) never observes a half-written document.
// The rename goes through robustio because on Windows it fails while a reader
// holds path open.
func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pkgmeta-*.tmp")
	if err != nil {
		return fmt.Errorf("staging %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("staging %s: %w", path, err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := robustio.Rename(tmpName, path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func readJSON[T any](path string) (*T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &v, nil
}
