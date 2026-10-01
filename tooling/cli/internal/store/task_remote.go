// Remote travel of a task-owned entry.
//
// # The wire is untouched
//
// protocols/cache stays at ProtocolVersion 1 and gains no field: a cache entry
// on the wire is still (key, ActionResult, Manifest) plus the content-addressed
// blob exchange. What changes is the CONTENT that travels, and this file is the
// whole of it — two decisions, both structural rather than conventional.
//
// # Decision 1: the remote key is the FORMAT-QUALIFIED address, not the cache key
//
// The local store already separates the two entry models by address
// (task_entry.go): a task-owned entry lives at TaskEntryAddress(key), so a CLI
// that only knows the legacy layout computes blobDir(key) and never sees it.
// The remote cache is shared far more widely than the machine-global store —
// across machines, branches and CLI versions at once — so it needs the same
// property or it loses it for everyone: a pre-B4c binary asking the provider for
// key K would receive a format-2 payload, materialize it as a legacy capture,
// and restore a tree laid out by declared-output id into a directory that
// expects the captured shape. That is silent corruption, and no amount of care
// in THIS build can prevent it, because the old binary cannot be taught.
//
// So a task-owned entry is published to the provider under
// RemoteTaskEntryKey(key) = TaskEntryAddress(key). Old binaries never compute
// that string; this build never publishes a format-2 payload under the raw key.
// The provider sees an opaque 64-hex key either way (cache.ValidKey holds), so
// no server change is implied, and bumping CurrentEntryFormat relocates the
// remote namespace exactly as it relocates the local one.
//
// # Decision 2: the entry DESCRIPTOR travels as content, at a reserved path
//
// A remote hit hands back a result and a manifest. Reconstructing the local
// format-2 entry needs one more thing: the descriptor (which declared outputs
// this entry satisfies, and whether each is present or explicitly empty). It is
// deliberately NOT re-derived on the restoring side from the local declaration
// plus the manifest shape — B4a records the descriptor precisely so a restore is
// checked against the contract the entry was WRITTEN for. So it travels, as one
// more content-addressed blob in the manifest, at the reserved path
// RemoteEntryDescriptorPath.
//
// Two consequences, both wanted:
//
//   - The payload is self-describing. A hit whose manifest carries no descriptor
//     is a legacy payload and reads as a MISS; a descriptor that is torn, in
//     another format, records another key, or disagrees with the manifest it
//     arrived with also reads as a MISS. Fail closed, exactly as
//     LookupTaskEntry does locally: a wrongly accepted entry materializes the
//     wrong tree, a wrongly rejected one costs a recompute.
//   - The reserved path cannot collide with a real output. Manifest paths in a
//     task-owned entry are "<id>" (file output) or "<id>/<rel>" (directory
//     output), and validateOutputID refuses RemoteEntryDescriptorPath as an id,
//     so neither shape can produce it.
//
// The descriptor blob is exchange-only: it is staged straight into the blob
// exchange directory on upload and verified-then-written as entry.json on
// restore. It is deliberately never published into the local CAS, because GC
// keeps a blob alive only while a surviving entry's MANIFEST references it
// (gc.go liveBlobs) and the local manifest.json holds payload files only.
// A CAS copy would be swept and the next upload would fail to export it.
package store

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	cache "go.putnami.dev/protocol/cache"
	proto "go.putnami.dev/protocol/extension"
)

// RemoteEntryDescriptorPath is the reserved manifest path a task-owned entry's
// descriptor travels under. It is a valid cache.ValidRelPath (so the existing
// wire validators accept it unchanged) and an id validateOutputID refuses (so no
// declared output can ever produce it).
const RemoteEntryDescriptorPath = ".putnami-entry.json"

// maxRemoteDescriptorBytes bounds the descriptor read from an untrusted
// exchange. A real descriptor is a few hundred bytes; this only stops a hostile
// or corrupt blob from being read into memory unbounded before its digest can
// be checked.
const maxRemoteDescriptorBytes = 1 << 20

// RemoteTaskEntryKey maps a task cache key to the key its task-owned entry is
// stored under in the REMOTE cache: the same format-qualified address the local
// store uses. See this file's header for why the raw key is not usable.
func RemoteTaskEntryKey(key string) string {
	return TaskEntryAddress(key)
}

// TaskEntryTransfer is one task-owned entry prepared for the blob exchange.
type TaskEntryTransfer struct {
	// Key is the provider-facing key (RemoteTaskEntryKey of the cache key).
	Key string

	// Manifest is what travels on the wire: the payload files plus the reserved
	// descriptor entry, in canonical path order.
	Manifest *cache.Manifest

	// Payload is the CAS-backed subset of Manifest — the entry's own manifest,
	// unchanged. It is what ExportManifestBlobs can serve and what the
	// break-even guard prices, since the descriptor is fixed overhead every
	// task-owned entry pays.
	Payload *cache.Manifest

	// Descriptor is the exact entry.json bytes, and DescriptorDigest their CAS
	// address. The caller stages them with StageExchangeBlob; they are byte-
	// identical to what the publishing store wrote, so a restore-then-reupload
	// round trip is digest-stable.
	Descriptor       []byte
	DescriptorDigest string
}

// NewTaskEntryTransfer prepares a published task-owned entry for upload.
func NewTaskEntryTransfer(entry *TaskEntry) (*TaskEntryTransfer, error) {
	if entry == nil || entry.Key == "" {
		return nil, fmt.Errorf("%w: task entry required", ErrEntryFormat)
	}
	if len(entry.Descriptor) == 0 {
		return nil, fmt.Errorf("%w: entry for %s carries no descriptor bytes", ErrEntryFormat, entry.Key)
	}

	payload := &cache.Manifest{Files: []cache.FileEntry{}}
	if entry.Manifest != nil {
		payload.Files = append(payload.Files, entry.Manifest.Files...)
	}
	wire := &cache.Manifest{Files: make([]cache.FileEntry, 0, len(payload.Files)+1)}
	for _, f := range payload.Files {
		if f.Path == RemoteEntryDescriptorPath {
			// Unreachable through ingest (the id is reserved); refuse rather than
			// ship a manifest whose descriptor slot is ambiguous.
			return nil, fmt.Errorf("%w: entry for %s already claims %s", ErrEntryFormat, entry.Key, RemoteEntryDescriptorPath)
		}
		wire.Files = append(wire.Files, f)
	}

	digest := cache.DigestOf(entry.Descriptor)
	wire.Files = append(wire.Files, cache.FileEntry{
		Path:   RemoteEntryDescriptorPath,
		Digest: digest,
		Mode:   0o644,
		Size:   int64(len(entry.Descriptor)),
	})
	cache.NormalizeManifestFiles(wire)

	return &TaskEntryTransfer{
		Key:              RemoteTaskEntryKey(entry.Key),
		Manifest:         wire,
		Payload:          payload,
		Descriptor:       entry.Descriptor,
		DescriptorDigest: digest,
	}, nil
}

// StageExchangeBlob writes content into the blob exchange directory at its
// content-addressed path. It is the descriptor's counterpart to
// ExportManifestBlobs, which can only serve blobs the CAS already holds.
//
// The write is staged and renamed: concurrent uploads of entries sharing a
// descriptor digest target the same path, and a reader (the provider) must never
// observe a partial file.
func StageExchangeBlob(exchangeDir, digest string, content []byte) error {
	if exchangeDir == "" {
		return fmt.Errorf("stage exchange blob: exchange directory required")
	}
	dst, ok := cache.BlobExchangePath(exchangeDir, digest)
	if !ok {
		return fmt.Errorf("stage exchange blob: invalid digest %q", digest)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		return nil // content-addressed: already staged by a sibling upload
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "entry-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		if _, statErr := os.Stat(dst); statErr == nil {
			return nil // a sibling staged the same bytes first
		}
		return err
	}
	return nil
}

// RemoteTaskEntryHit is a provider hit for a task-owned entry, as it came off
// the wire.
type RemoteTaskEntryHit struct {
	// Key is the LOCAL cache key (not the provider key): the entry is published
	// at TaskEntryAddress(Key), and the descriptor must record exactly this key.
	Key string

	// Result is the restored outcome, converted from the wire ActionResult.
	Result *EntryResult

	// Metadata is the provenance sidecar to record; Hash, Size and OutputFiles
	// are filled in here.
	Metadata *EntryMetadata

	// Manifest is the manifest as received: payload files plus the reserved
	// descriptor entry.
	Manifest *cache.Manifest
}

// MaterializeTaskEntry installs a provider hit as a local task-owned entry and
// returns it, or fails without publishing anything.
//
// It is the remote counterpart of IngestTaskEntry and produces an entry
// indistinguishable from one this machine captured: same address, same
// descriptor bytes, same payload-only manifest.json, so a later local lookup,
// materialize, GC pass or re-upload cannot tell the two apart. Every restore
// then reaches the workspace through the ONE atomic primitive
// (MaterializeTaskOutput) — there is no remote-specific delivery.
//
// A payload this build cannot interpret is an error, and every caller turns it
// into a MISS: no descriptor (a legacy-format hit under this address), a
// descriptor that is torn or in another format, a descriptor recording a
// different key, or a descriptor disagreeing with the manifest it arrived with.
func (cm *CacheManager) MaterializeTaskEntry(hit RemoteTaskEntryHit, fetch BlobFetcher) (*TaskEntry, error) {
	return cm.store.MaterializeTaskEntry(hit, fetch)
}

// MaterializeTaskEntry publishes a remote task-owned hit into the local store.
// See CacheManager.MaterializeTaskEntry.
func (s *LocalStore) MaterializeTaskEntry(hit RemoteTaskEntryHit, fetch BlobFetcher) (*TaskEntry, error) {
	if hit.Key == "" {
		return nil, fmt.Errorf("materialize task entry: cache key required")
	}
	if hit.Result == nil {
		return nil, fmt.Errorf("materialize task entry %s: result required", hit.Key)
	}
	if hit.Manifest == nil {
		return nil, fmt.Errorf("materialize task entry %s: manifest required", hit.Key)
	}

	payload, descriptorFile, ok := splitTaskEntryManifest(hit.Manifest)
	if !ok {
		return nil, fmt.Errorf("%w: hit for %s carries no entry descriptor", ErrEntryFormat, hit.Key)
	}
	descriptorBytes, err := fetchEntryDescriptor(descriptorFile, fetch)
	if err != nil {
		return nil, err
	}
	descriptor, err := parseTaskDescriptor(descriptorBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: hit for %s has an unreadable descriptor: %w", ErrEntryFormat, hit.Key, err)
	}
	if descriptor.Key != hit.Key {
		return nil, fmt.Errorf("%w: hit for %s carries a descriptor recording %q", ErrEntryFormat, hit.Key, descriptor.Key)
	}
	if err := validateDescriptorPayload(descriptor, payload); err != nil {
		return nil, err
	}
	cache.NormalizeManifestFiles(payload)

	s.ensureGeneration() // count this build before taking the store lock

	releaseLock := s.lockShared()
	defer releaseLock()

	tmpDir, err := s.createTmpDir()
	if err != nil {
		return nil, fmt.Errorf("materialize task entry: create tmp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir) // no-op once renamed; cleanup on every error path

	total, err := s.materializeFiles(payload, filepath.Join(tmpDir, "files"), fetch)
	if err != nil {
		return nil, fmt.Errorf("materialize task entry %s: %w", hit.Key, err)
	}

	address := TaskEntryAddress(hit.Key)
	meta := hit.Metadata
	if meta == nil {
		meta = &EntryMetadata{CreatedAt: time.Now()}
	}
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now()
	}
	meta.Hash = address
	meta.Size = total
	meta.OutputFiles = manifestFilePaths(payload)

	// The descriptor is written back BYTE FOR BYTE rather than re-marshaled, so
	// the entry this machine publishes has the same descriptor digest as the one
	// that produced it and a restore-then-reupload round trip moves no new blob.
	if err := os.WriteFile(filepath.Join(tmpDir, entryDescriptorFilename), descriptorBytes, 0o644); err != nil {
		return nil, fmt.Errorf("materialize task entry: write %s: %w", entryDescriptorFilename, err)
	}
	if err := writeJSONFile(filepath.Join(tmpDir, "result.json"), hit.Result); err != nil {
		return nil, fmt.Errorf("materialize task entry: write result.json: %w", err)
	}
	if err := writeJSONFile(filepath.Join(tmpDir, "meta.json"), meta); err != nil {
		return nil, fmt.Errorf("materialize task entry: write meta.json: %w", err)
	}
	if err := writeJSONFile(filepath.Join(tmpDir, manifestFilename), payload); err != nil {
		return nil, fmt.Errorf("materialize task entry: write manifest: %w", err)
	}

	// In-process lock for the atomic publish (the cross-process shared lock is
	// already held for the whole call), mirroring IngestTaskEntry.
	s.mu.Lock()
	defer s.mu.Unlock()

	blobDir := s.blobDir(address)
	if err := os.MkdirAll(filepath.Dir(blobDir), 0o755); err != nil {
		return nil, fmt.Errorf("materialize task entry: create blob parent: %w", err)
	}
	if err := s.publishEntry(tmpDir, blobDir); err != nil {
		return nil, fmt.Errorf("materialize task entry: %w", err)
	}
	writeLastUsed(blobDir, time.Now(), s.gen)

	published := loadTaskEntry(blobDir, address)
	if published == nil || published.Key != hit.Key {
		return nil, fmt.Errorf("materialize task entry %s: published entry is not readable", hit.Key)
	}
	return published, nil
}

// splitTaskEntryManifest separates the reserved descriptor entry from the
// payload files. ok is false when the manifest has no descriptor (a legacy
// payload) or more than one (a manifest this build refuses to interpret).
func splitTaskEntryManifest(manifest *cache.Manifest) (*cache.Manifest, cache.FileEntry, bool) {
	payload := &cache.Manifest{Files: make([]cache.FileEntry, 0, len(manifest.Files))}
	var descriptor cache.FileEntry
	found := false
	for _, f := range manifest.Files {
		if f.Path == RemoteEntryDescriptorPath {
			if found {
				return nil, cache.FileEntry{}, false
			}
			descriptor = f
			found = true
			continue
		}
		payload.Files = append(payload.Files, f)
	}
	if !found {
		return nil, cache.FileEntry{}, false
	}
	return payload, descriptor, true
}

// fetchEntryDescriptor pulls the descriptor blob off the exchange and verifies
// it content-addresses to the digest the manifest claimed. It never reaches the
// CAS: the bytes become entry.json directly (see this file's header).
func fetchEntryDescriptor(f cache.FileEntry, fetch BlobFetcher) ([]byte, error) {
	if !cache.ValidDigest(f.Digest) {
		return nil, fmt.Errorf("%w: descriptor has invalid digest %q", ErrEntryFormat, f.Digest)
	}
	if fetch == nil {
		return nil, fmt.Errorf("%w: descriptor blob %s needs a fetcher", ErrEntryFormat, f.Digest)
	}
	rc, err := fetch(f.Digest)
	if err != nil {
		return nil, fmt.Errorf("%w: fetch descriptor %s: %w", ErrEntryFormat, f.Digest, err)
	}
	defer rc.Close()

	data, err := io.ReadAll(io.LimitReader(rc, maxRemoteDescriptorBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read descriptor %s: %w", ErrEntryFormat, f.Digest, err)
	}
	if len(data) > maxRemoteDescriptorBytes {
		return nil, fmt.Errorf("%w: descriptor %s exceeds %d bytes", ErrEntryFormat, f.Digest, maxRemoteDescriptorBytes)
	}
	if got := cache.DigestOf(data); got != f.Digest {
		return nil, fmt.Errorf("%w: descriptor content-addresses to %s, want %s", ErrEntryFormat, got, f.Digest)
	}
	return data, nil
}

// validateDescriptorPayload checks that a descriptor and the manifest it
// arrived with describe the same entry. It is the torn-payload guard: the
// descriptor decides what a restore writes, so a manifest that carries fewer,
// more, or differently-shaped files than the descriptor records is not
// interpretable and must read as a MISS rather than materialize a partial tree.
func validateDescriptorPayload(entry *TaskEntry, payload *cache.Manifest) error {
	counts := make(map[string]int, len(entry.Outputs))
	atSlotRoot := make(map[string]bool, len(entry.Outputs))
	for _, f := range payload.Files {
		id, nested := manifestOutputSlot(f.Path)
		if id == "" {
			return fmt.Errorf("%w: payload path %q is not addressed by a declared output", ErrEntryFormat, f.Path)
		}
		counts[id]++
		if !nested {
			atSlotRoot[id] = true
		}
	}

	for _, out := range entry.Outputs {
		got := counts[out.ID]
		delete(counts, out.ID)
		if !out.Present() {
			if got != 0 {
				return fmt.Errorf("%w: output %q is recorded empty but the payload carries %d files",
					ErrEntryFormat, out.ID, got)
			}
			continue
		}
		if got != out.Files {
			return fmt.Errorf("%w: output %q records %d files, the payload carries %d",
				ErrEntryFormat, out.ID, out.Files, got)
		}
		switch out.Kind {
		case proto.OutputKindFile:
			if got != 1 || !atSlotRoot[out.ID] {
				return fmt.Errorf("%w: file output %q must be one payload file at %q", ErrEntryFormat, out.ID, out.ID)
			}
		case proto.OutputKindDirectory:
			if atSlotRoot[out.ID] {
				return fmt.Errorf("%w: directory output %q has a payload file at its own path", ErrEntryFormat, out.ID)
			}
		}
	}
	for id := range counts {
		return fmt.Errorf("%w: payload carries files for undeclared output %q", ErrEntryFormat, id)
	}
	return nil
}

// manifestOutputSlot returns the declared-output id a manifest path is filed
// under and whether the path is nested inside it. An empty id means the path is
// not a usable slot address.
func manifestOutputSlot(p string) (string, bool) {
	id, rest, nested := strings.Cut(p, "/")
	if id == "" || (nested && rest == "") {
		return "", false
	}
	return id, nested
}
