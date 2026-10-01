package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	cache "go.putnami.dev/protocol/cache"
	proto "go.putnami.dev/protocol/extension"
)

// Validate-complete ingest of a task-owned entry from a staging root.
//
// The legacy ingest (cas.go ingestFiles) answers "what is in this directory that
// wasn't there before?". This one answers "does the staging root contain exactly
// what the task declared?", and refuses to publish when it does not. The two
// differences that matter:
//
//   - COMPLETENESS IS ENFORCED. A required declared output that is absent from
//     the staging root — or present but empty — is ErrIncompleteCapture, not a
//     smaller entry. A smaller entry is indistinguishable from a complete one at
//     lookup time, so it would restore less than the declaration promises for
//     every future hit, on every machine that pulls it.
//   - EMPTINESS IS STATED. An optionalEmpty output that produced nothing is
//     recorded with State TaskOutputEmpty, so restore knows to leave its
//     destination alone instead of inferring anything from an absent payload.
//
// The staging root is laid out by symbolic root, so the ingest finds each
// declared output at a location derived from the declaration alone and B4b can
// hand the task a redirected tree without renaming anything afterwards:
//
//	<staging>/project/<path>          root: project
//	<staging>/workspace/<path>        root: workspace
//	<staging>/command-output/<path>   root: command-output
//
// Publication reuses the legacy machinery unchanged — CAS dedup, the shared
// store lock, staged directory, single atomic rename, first-writer-wins — so a
// task-owned entry is an ordinary blob to GC, to the CAS sweep, and to the
// remote-cache manifest exchange.

// TaskEntrySpec is everything needed to publish a task-owned entry except the
// bytes, which come from the staging root.
type TaskEntrySpec struct {
	// Key is the task cache key (v5). The entry is published at
	// TaskEntryAddress(Key).
	Key string

	// Result is the structured job result stored as result.json.
	Result *EntryResult

	// Metadata is the provenance sidecar. Hash, Size and OutputFiles are filled
	// in by the ingest; a nil Metadata is created.
	Metadata *EntryMetadata

	// Outputs is the task's declared-output set, RESOLVED for this run: every
	// pathFrom port already turned into a literal path. It is a contract, not a
	// wish list — the ingest fails unless the staging root satisfies all of it.
	Outputs []DeclaredEntryOutput
}

// TaskStagingPath returns where the ingest expects one declared output's bytes
// inside a staging root. Exported so the executor that lays the staging root out
// and the store that reads it derive the same path from the same declaration.
func TaskStagingPath(stagingRoot string, out DeclaredEntryOutput) string {
	return filepath.Join(stagingRoot, out.EffectiveRoot(), filepath.FromSlash(out.Path))
}

// IngestTaskEntry publishes a task-owned entry from a staging root, or fails
// without publishing anything.
//
// The whole ingest runs under the store's SHARED lock (coexists with sibling
// publishers, excludes GC), stages into tmp/, and commits with one rename, so a
// concurrent reader observes either no entry or a complete one. Publication is
// first-writer-wins: a sibling worktree that already published this key's entry
// keeps its copy, and the returned entry is read back from what is actually on
// disk rather than from what this call staged.
func (s *LocalStore) IngestTaskEntry(stagingRoot string, spec TaskEntrySpec) (*TaskEntry, error) {
	if spec.Key == "" {
		return nil, fmt.Errorf("ingest task entry: cache key required")
	}
	if spec.Result == nil {
		return nil, fmt.Errorf("ingest task entry: result required")
	}
	declared, err := validateDeclaredOutputs(spec.Outputs)
	if err != nil {
		return nil, fmt.Errorf("ingest task entry %s: %w", spec.Key, err)
	}

	s.ensureGeneration() // count this build before taking the store lock

	releaseLock := s.lockShared()
	defer releaseLock()

	tmpDir, err := s.createTmpDir()
	if err != nil {
		return nil, fmt.Errorf("ingest task entry: create tmp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir) // no-op once renamed; cleanup on every error path

	blobFiles := filepath.Join(tmpDir, "files")
	manifest := &cache.Manifest{}
	recorded := make([]TaskEntryOutput, 0, len(declared))
	var totalSize int64

	for _, out := range declared {
		record := TaskEntryOutput{
			ID:       out.ID,
			Kind:     out.Kind,
			Root:     out.EffectiveRoot(),
			Path:     out.Path,
			Optional: out.Optional,
			State:    TaskOutputEmpty,
		}

		files, size, err := s.ingestStagedOutput(TaskStagingPath(stagingRoot, out), blobFiles, out, manifest)
		if err != nil {
			return nil, fmt.Errorf("ingest task entry %s: %w", spec.Key, err)
		}
		if files > 0 {
			record.State = TaskOutputPresent
			record.Files = files
			record.Size = size
			totalSize += size
		}
		recorded = append(recorded, record)
	}

	cache.NormalizeManifestFiles(manifest)

	address := TaskEntryAddress(spec.Key)
	descriptor := &TaskEntry{
		Format:  CurrentEntryFormat,
		Key:     spec.Key,
		Outputs: recorded,
	}

	meta := spec.Metadata
	if meta == nil {
		meta = &EntryMetadata{CreatedAt: time.Now()}
	}
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now()
	}
	meta.Hash = address
	meta.Size = totalSize
	meta.OutputFiles = manifestFilePaths(manifest)

	if err := writeJSONFile(filepath.Join(tmpDir, entryDescriptorFilename), descriptor); err != nil {
		return nil, fmt.Errorf("ingest task entry: write %s: %w", entryDescriptorFilename, err)
	}
	if err := writeJSONFile(filepath.Join(tmpDir, "result.json"), spec.Result); err != nil {
		return nil, fmt.Errorf("ingest task entry: write result.json: %w", err)
	}
	if err := writeJSONFile(filepath.Join(tmpDir, "meta.json"), meta); err != nil {
		return nil, fmt.Errorf("ingest task entry: write meta.json: %w", err)
	}
	if err := writeJSONFile(filepath.Join(tmpDir, manifestFilename), manifest); err != nil {
		return nil, fmt.Errorf("ingest task entry: write manifest: %w", err)
	}

	// In-process lock for the atomic publish (the cross-process shared lock is
	// already held for the whole call), mirroring put().
	s.mu.Lock()
	defer s.mu.Unlock()

	blobDir := s.blobDir(address)
	if err := os.MkdirAll(filepath.Dir(blobDir), 0o755); err != nil {
		return nil, fmt.Errorf("ingest task entry: create blob parent: %w", err)
	}
	if err := s.publishEntry(tmpDir, blobDir); err != nil {
		return nil, fmt.Errorf("ingest task entry: %w", err)
	}
	writeLastUsed(blobDir, time.Now(), s.gen)

	published := loadTaskEntry(blobDir, address)
	if published == nil || published.Key != spec.Key {
		return nil, fmt.Errorf("ingest task entry %s: published entry is not readable", spec.Key)
	}
	return published, nil
}

// validateDeclaredOutputs checks the declaration the ingest is asked to satisfy
// and returns it in canonical (id-sorted) order, so two captures of one
// declaration produce byte-identical descriptors regardless of map iteration
// order upstream.
func validateDeclaredOutputs(outputs []DeclaredEntryOutput) ([]DeclaredEntryOutput, error) {
	normalized := make([]DeclaredEntryOutput, 0, len(outputs))
	seen := make(map[string]bool, len(outputs))

	for _, out := range outputs {
		if err := validateOutputID(out.ID); err != nil {
			return nil, err
		}
		if seen[out.ID] {
			return nil, fmt.Errorf("duplicate declared output id %q", out.ID)
		}
		seen[out.ID] = true

		switch out.Kind {
		case proto.OutputKindFile, proto.OutputKindDirectory:
		default:
			return nil, fmt.Errorf("declared output %q has unknown kind %q", out.ID, out.Kind)
		}
		switch out.EffectiveRoot() {
		case proto.OutputRootProject, proto.OutputRootWorkspace, proto.OutputRootCommandOutput:
		default:
			return nil, fmt.Errorf("declared output %q has unknown root %q", out.ID, out.Root)
		}
		path, err := proto.NormalizeOutputPath(out.Path)
		if err != nil {
			return nil, fmt.Errorf("declared output %q has invalid path %q: %w", out.ID, out.Path, err)
		}
		out.Root = out.EffectiveRoot()
		out.Path = path
		normalized = append(normalized, out)
	}

	sort.Slice(normalized, func(i, j int) bool { return normalized[i].ID < normalized[j].ID })
	return normalized, nil
}

// ingestStagedOutput ingests one declared output from the staging root into the
// blob's files/ tree and appends its files to manifest. It returns the number of
// files captured and their total size; (0, 0) means the output legitimately
// produced nothing, which is only allowed for an optional output.
//
// The completeness rule is applied here, once, for every shape of "nothing":
// the path is missing, the path is an empty directory, or the path holds only
// throwaway intermediates. All three mean the entry cannot honor the
// declaration unless the declaration allows it.
func (s *LocalStore) ingestStagedOutput(
	srcPath, blobFiles string,
	out DeclaredEntryOutput,
	manifest *cache.Manifest,
) (int, int64, error) {
	info, err := os.Lstat(srcPath)
	switch {
	case err != nil && os.IsNotExist(err):
		if !out.Optional {
			return 0, 0, fmt.Errorf("%w: %q (%s %s/%s) was not produced",
				ErrIncompleteCapture, out.ID, out.Kind, out.Root, out.Path)
		}
		return 0, 0, nil
	case err != nil:
		return 0, 0, fmt.Errorf("stat declared output %q: %w", out.ID, err)
	}

	// A declared kind is a promise about the restore shape, so a mismatch is an
	// error rather than a best-effort capture: restoring a directory where a
	// consumer expects a file (or the reverse) fails later and further away.
	if out.Kind == proto.OutputKindDirectory && !info.IsDir() {
		return 0, 0, fmt.Errorf("declared output %q is declared a directory but staging holds a file", out.ID)
	}
	if out.Kind == proto.OutputKindFile && info.IsDir() {
		return 0, 0, fmt.Errorf("declared output %q is declared a file but staging holds a directory", out.ID)
	}

	dst := filepath.Join(blobFiles, out.ID)
	var files int
	var total int64

	if out.Kind == proto.OutputKindFile {
		size, err := s.ingestOneFile(srcPath, dst, info, out.ID, manifest)
		if err != nil {
			return 0, 0, err
		}
		files, total = 1, size
	} else {
		files, total, err = s.ingestOutputTree(srcPath, dst, out.ID, manifest)
		if err != nil {
			return 0, 0, err
		}
	}

	if files == 0 && !out.Optional {
		return 0, 0, fmt.Errorf("%w: %q (%s %s/%s) produced no files",
			ErrIncompleteCapture, out.ID, out.Kind, out.Root, out.Path)
	}
	return files, total, nil
}

// ingestOutputTree walks one declared directory output, ingesting every regular
// file into the CAS under "<id>/<rel>". Empty directories are not represented in
// cache manifests (see dirMatchesManifest), so a tree of only empty directories
// counts as no files.
func (s *LocalStore) ingestOutputTree(srcDir, dstDir, id string, manifest *cache.Manifest) (int, int64, error) {
	var files int
	var total int64

	err := filepath.Walk(srcDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		if uncacheableArtifact(rel) {
			return nil
		}
		size, err := s.ingestOneFile(p, filepath.Join(dstDir, rel), info, id+"/"+filepath.ToSlash(rel), manifest)
		if err != nil {
			return err
		}
		files++
		total += size
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("capture declared output %q: %w", id, err)
	}
	return files, total, nil
}

// ingestOneFile publishes one file's bytes into the CAS, hardlinks them into the
// staged files/ tree, and records the manifest entry under manifestPath. It is
// the per-file half of ingestFiles, without the baseline subtraction the
// declared model replaces.
func (s *LocalStore) ingestOneFile(
	srcPath, dstPath string,
	info os.FileInfo,
	manifestPath string,
	manifest *cache.Manifest,
) (int64, error) {
	digest, size, err := digestFile(srcPath)
	if err != nil {
		return 0, fmt.Errorf("digest %s: %w", manifestPath, err)
	}

	s.mu.Lock()
	err = s.ensureBlobLocked(srcPath, digest)
	if err == nil {
		err = s.linkBlob(digest, dstPath, info.Mode())
	}
	s.mu.Unlock()
	if err != nil {
		return 0, fmt.Errorf("store %s: %w", manifestPath, err)
	}

	manifest.Files = append(manifest.Files, cache.FileEntry{
		Path:   manifestPath,
		Digest: digest,
		Mode:   uint32(info.Mode().Perm()),
		Size:   size,
	})
	return size, nil
}

// manifestFilePaths lists a manifest's paths, for the advisory meta.json field the
// legacy model also carries.
func manifestFilePaths(manifest *cache.Manifest) []string {
	if manifest == nil || len(manifest.Files) == 0 {
		return nil
	}
	paths := make([]string, 0, len(manifest.Files))
	for _, f := range manifest.Files {
		paths = append(paths, f.Path)
	}
	return paths
}

// writeJSONFile writes an indented JSON document into the staging directory.
// Staged files need no atomic write of their own: the whole directory is
// committed by one rename.
func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
