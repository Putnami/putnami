package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// installedTreeDigestSchema tags the installed-tree digest preimage and its
// artifact-store record. Changing what the digest reads changes this tag, so a
// record of the previous derivation is never read as the current one.
const installedTreeDigestSchema = "putnami-installed-extension-tree-v1"

// implementationRecordLockWait bounds how long a key computation waits for the
// artifact store's shared lock before it derives an installed digest without
// recording it.
const implementationRecordLockWait = 2 * time.Second

// extensionImplementationDigest returns CacheKey.ExtensionImplementationDigest
// for the extension that runs job: the identity of the code every task of that
// extension runs, carried by each of them because a runtime-backed preBuild
// hook can affect a task whose own command does not reference
// {extensionRuntime}. When it is non-empty the key does not carry the
// extension version.
//
//   - A workspace-local extension with a declared prepared runtime yields its
//     runtime input digest, the value synchronization computed.
//   - Any other local source yields "": a local extension without a prepared
//     runtime, or one outside the workspace, keeps its version as its
//     identity.
//   - An installed extension yields the content digest of its installed tree
//     without version metadata (installedTreeDigest), so two builds that
//     differ only in the version they stamp share their entries, and a build
//     whose runtime, configuration or any other file differs does not.
func extensionImplementationDigest(
	ws *workspace.Workspace,
	job *ScheduledJob,
	cache *store.CacheManager,
) (string, error) {
	if job == nil || job.Extension == nil {
		return "", nil
	}
	if !job.Extension.LocalSource {
		workspaceRoot := ""
		if ws != nil {
			workspaceRoot = ws.Root
		}
		return installedExtensionDigest(workspaceRoot, job.Extension, cache)
	}
	if !isLocalDevExtensionSource(ws, job) {
		return "", nil
	}
	if job.Extension.Runtime == nil || job.Extension.Runtime.Prepare == nil {
		return "", nil
	}
	if job.Extension.RuntimeDigest != "" {
		return job.Extension.RuntimeDigest, nil
	}
	// Focused callers assembled outside the engine synchronization lifecycle
	// still fail closed on stale cache reuse by hashing the declared runtime.
	return extensionRuntimeDigest(job.Extension)
}

// installedExtensionDigest returns the installed-tree digest of ext, memoized
// for the cache manager's run. An extension whose installed directory does not
// exist has no tree to digest and keeps its version as its identity.
//
// A tree inside the artifact store is immutable: its digest is recorded beside
// it, per archive digest, and later runs read that record. Every other tree,
// such as a node_modules package or a per-worktree install, can change between
// runs, so each run digests it again and nothing records it.
func installedExtensionDigest(
	workspaceRoot string,
	ext *extension.ExtensionDescription,
	cache *store.CacheManager,
) (string, error) {
	if ext.Path == "" {
		return "", nil
	}
	return cache.Digest("installed-extension:"+ext.Path, func() (string, error) {
		root, err := dirlink.Resolve(ext.Path)
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("resolve installed extension %q: %w", ext.Name, err)
		}
		artifacts := artifactstore.New(store.ResolveArtifactStoreRoot(workspaceRoot))
		entry, inStore := artifacts.Entry(root)
		if !inStore {
			digest, err := installedTreeDigest(root, false)
			if err != nil {
				return "", fmt.Errorf("digest installed extension %q: %w", ext.Name, err)
			}
			return digest, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), implementationRecordLockWait)
		defer cancel()
		record, err := artifacts.Implementation(ctx, entry, isInstalledTreeRecord,
			func(dir string) (string, error) {
				digest, err := installedTreeDigest(dir, true)
				if err != nil {
					return "", err
				}
				return installedTreeDigestSchema + " " + digest, nil
			})
		if err != nil {
			return "", fmt.Errorf("digest installed extension %q: %w", ext.Name, err)
		}
		return strings.TrimPrefix(record, installedTreeDigestSchema+" "), nil
	})
}

// isInstalledTreeRecord reports whether record is an artifact-store record of
// the current installed-tree digest schema: the schema tag, one space, and a
// lowercase hex SHA-256, with nothing else.
func isInstalledTreeRecord(record string) bool {
	digest, ok := strings.CutPrefix(record, installedTreeDigestSchema+" ")
	if !ok || len(digest) != sha256.Size*2 {
		return false
	}
	for _, c := range digest {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// installedTreeDigest returns the lowercase hex SHA-256 of an installed
// extension tree: the schema tag, then, in lexical walk order, every regular
// file as its slash path, executable bit and bytes, every symbolic link as its
// slash path and target, and every other file, such as a named pipe, as its
// slash path and file type, never its content. Directories contribute only
// through what they hold.
//
// The version an extension build stamps is not part of the tree's identity,
// so two files are read without it (versionMetadata): the root
// putnami.extension.json without its top-level version and without the
// agentContent.manifestSha256 that binds the agent-content manifest's bytes,
// and that agent-content manifest without its version. Each is re-encoded
// canonically; a file that is not one valid JSON object is hashed raw. Every
// other file, whatever version it carries, is hashed as bytes.
//
// inStore skips the artifact store's bookkeeping files at the root of the
// tree, which the store writes into every entry and which are not part of the
// archive.
func installedTreeDigest(root string, inStore bool) (string, error) {
	versionless := versionMetadata(root)
	h := sha256.New()
	hashField(h, "schema", []byte(installedTreeDigestSchema))
	err := filepath.WalkDir(root, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if inStore && !strings.Contains(rel, "/") && artifactstore.IsBookkeeping(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return err
			}
			hashField(h, "symlink", []byte(rel))
			hashField(h, "target", []byte(filepath.ToSlash(target)))
			return nil
		case info.Mode().IsRegular():
			hashField(h, "file", []byte(rel))
			executable := "-"
			if info.Mode()&0o111 != 0 {
				executable = "x"
			}
			hashField(h, "executable", []byte(executable))
			if drop, ok := versionless[rel]; ok {
				return hashVersionlessJSON(h, full, drop)
			}
			return hashFileBytes(h, full, info.Size())
		default:
			hashField(h, "special", []byte(rel))
			hashField(h, "type", []byte(info.Mode().Type().String()))
			return nil
		}
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// versionMetadata returns the slash paths, relative to root, of the files that
// carry an extension build's version, each with the JSON members that carry
// it: the top-level version of the extension manifest and of the agent-content
// manifest the extension manifest names, and the extension manifest's
// agentContent.manifestSha256, the digest of that agent-content manifest's
// bytes, version included. The agent-content manifest is named only when
// agentContent.path is a relative path inside the tree. Both files are defined
// by the extension protocol; a version in any other file is the extension's own
// content.
func versionMetadata(root string) map[string][][]string {
	files := map[string][][]string{
		extension.ManifestFilename: {{"version"}, {"agentContent", "manifestSha256"}},
	}
	data, err := os.ReadFile(filepath.Join(root, extension.ManifestFilename))
	if err != nil {
		return files
	}
	var manifest struct {
		AgentContent *struct {
			Path string `json:"path"`
		} `json:"agentContent"`
	}
	if json.Unmarshal(data, &manifest) != nil || manifest.AgentContent == nil {
		return files
	}
	dir := path.Clean(strings.ReplaceAll(manifest.AgentContent.Path, `\`, "/"))
	if manifest.AgentContent.Path == "" || path.IsAbs(dir) || dir == ".." || strings.HasPrefix(dir, "../") ||
		filepath.VolumeName(filepath.FromSlash(dir)) != "" {
		return files
	}
	agentManifest := path.Join(dir, wsproto.AgentArtifactManifestFilename)
	if _, named := files[agentManifest]; !named {
		files[agentManifest] = [][]string{{"version"}}
	}
	return files
}

// hashVersionlessJSON hashes the JSON object in full without the members drop
// names, re-encoded canonically (sorted keys, numbers as written, no
// insignificant whitespace). Bytes that are not valid UTF-8 holding exactly one
// JSON object are hashed raw, under a distinct label, so no raw file reads as
// the canonical form of another.
func hashVersionlessJSON(h hash.Hash, full string, drop [][]string) error {
	data, err := os.ReadFile(full)
	if err != nil {
		return err
	}
	if canonical, ok := withoutMembers(data, drop); ok {
		hashField(h, "versionless-json", canonical)
		return nil
	}
	hashField(h, "raw", data)
	return nil
}

// withoutMembers decodes data as one JSON object, deletes each member path in
// drop, and re-encodes it canonically. It reports false for anything else.
func withoutMembers(data []byte, drop [][]string) ([]byte, bool) {
	if !utf8.Valid(data) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil || document == nil {
		return nil, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	for _, member := range drop {
		deleteMember(document, member)
	}
	canonical, err := json.Marshal(document)
	if err != nil {
		return nil, false
	}
	return canonical, true
}

// deleteMember removes the member at the object path member from document,
// when every object on the path exists.
func deleteMember(document map[string]any, member []string) {
	for len(member) > 1 {
		next, ok := document[member[0]].(map[string]any)
		if !ok {
			return
		}
		document, member = next, member[1:]
	}
	delete(document, member[0])
}

// hashFileBytes hashes the size bytes of the regular file at full, and fails
// when the file holds a different number of bytes by the time it is read.
func hashFileBytes(h hash.Hash, full string, size int64) error {
	file, err := os.Open(full)
	if err != nil {
		return err
	}
	defer file.Close()
	counted := &countingReader{reader: io.LimitReader(file, size+1)}
	if err := hashStream(h, "bytes", size, counted); err != nil {
		return err
	}
	if counted.n != size {
		return fmt.Errorf("%s changed while it was hashed", full)
	}
	return nil
}

type countingReader struct {
	reader io.Reader
	n      int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.n += int64(n)
	return n, err
}
