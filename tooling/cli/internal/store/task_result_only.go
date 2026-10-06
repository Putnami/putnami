package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	cache "go.putnami.dev/protocol/cache"
)

// Result-only task entries — "this task, at this key, succeeded with this
// result; its files stayed in the remote cache".
//
// # Why a result-only entry is not a task-owned entry
//
// A run that reads none of a remote hit's files restores the hit result-only:
// the provider returns the result and the manifest and downloads no blob. Core
// then holds a trusted result and no file, which a task-owned entry
// (task_entry.go) cannot represent: its descriptor promises the declared
// outputs, and every restore of it writes them to the workspace.
//
// So the result lives in a blob directory of its own, at an address derived
// from a DISTINCT domain:
//
//	address = sha256("putnami/store/result-only-task-entry\x00" + <format> + "\x00" + key)
//
// The directory holds one record: the format, the key, the result and its
// metadata. It holds no entry.json, no result.json, no meta.json, no manifest
// and no files. The contract:
//
//   - THE KEY IS THE SAME KEY. A result-only entry is valid exactly where the
//     task-owned entry at the same key is, and for the same task.
//   - IT CANNOT BE READ AS ANY OTHER ENTRY. The addresses are disjoint, and the
//     directory carries none of the files the task-owned reader (entry.json) or
//     the legacy reader (meta.json, result.json) requires, so LookupTaskEntry,
//     Get, and an older CLI never interpret it even when they reach the address.
//     Only a caller that does not need the files asks for it, through
//     LookupResultOnlyTaskEntry.
//   - IT CANNOT BE PUBLISHED REMOTELY. The remote path only derives
//     RemoteTaskEntryKey, which is TaskEntryAddress, so nothing uploads it.
//   - PUBLICATION IS FIRST-WRITER-WINS. The directory is staged complete and
//     renamed into place by publishEntry, so a reader sees either no entry or a
//     whole one, and two sessions publishing the same key keep one of them. Both
//     carry the result of the same key, so either is correct.
//
// # GC participates with no special case
//
// The directory lives at blobs/<prefix>/<address>/ with a lastUsed sidecar, so
// scanStore enumerates it like any other blob directory: blobMetaBytes counts
// the record, and idle or budget eviction reclaims it. readManifestBlobs returns
// nil for a directory with no manifest, so it keeps no CAS blob alive.
//
// # Every unreadable entry is a MISS
//
// An absent or torn directory, an unknown format, or a record naming another
// key all read as "no result-only entry". The caller then restores the full
// entry or runs the task, which is always correct.

const (
	// ResultOnlyTaskEntryFormatV1 is the record shape this build writes and the
	// only one it reads. The format is part of the address, so bumping it
	// relocates every entry and the old ones age out through ordinary GC.
	ResultOnlyTaskEntryFormatV1 = 1

	// CurrentResultOnlyTaskEntryFormat is the format PublishResultOnlyTaskEntry
	// writes.
	CurrentResultOnlyTaskEntryFormat = ResultOnlyTaskEntryFormatV1
)

// resultOnlyTaskEntryAddressDomain namespaces the result-only address
// derivation. It differs from taskEntryAddressDomain and
// taskFailureAddressDomain, so no task-owned or negative-entry code path can
// name a result-only directory.
const resultOnlyTaskEntryAddressDomain = "putnami/store/result-only-task-entry"

// resultOnlyRecordFilename is the single file a result-only entry is made of.
const resultOnlyRecordFilename = "result-only.json"

// ResultOnlyTaskEntry is the recorded result of one task at one cache key,
// without its files.
type ResultOnlyTaskEntry struct {
	// Key is the task cache key the entry belongs to.
	Key string

	// Address is the derived store address.
	Address string

	// Result is the task's recorded outcome, in the shape a task-owned entry
	// stores.
	Result *EntryResult

	// Metadata is the provenance sidecar. Size is zero and OutputFiles empty:
	// the entry holds no file.
	Metadata *EntryMetadata
}

// resultOnlyRecord is the content of resultOnlyRecordFilename.
type resultOnlyRecord struct {
	// Format is the record format; always CurrentResultOnlyTaskEntryFormat for
	// a record this build wrote.
	Format int `json:"format"`
	// Key is the task cache key. It is re-checked on read, so an address
	// collision cannot serve another task's result.
	Key string `json:"key"`
	// Result is the task's recorded outcome.
	Result *EntryResult `json:"result"`
	// Metadata is the provenance sidecar.
	Metadata *EntryMetadata `json:"metadata,omitempty"`
}

// ResultOnlyTaskEntryAddress maps a task cache key to the store address its
// result-only entry lives at, binding the record format into the address.
func ResultOnlyTaskEntryAddress(key string) string {
	sum := sha256.Sum256([]byte(
		resultOnlyTaskEntryAddressDomain + "\x00" + strconv.Itoa(CurrentResultOnlyTaskEntryFormat) + "\x00" + key))
	return hex.EncodeToString(sum[:])
}

// LookupResultOnlyTaskEntry returns the result-only entry recorded for key, or
// nil for a miss. A hit stamps the entry's lastUsed sidecar under the shared
// store lock, so a concurrent GC cannot evict it between the read and the use.
func (s *LocalStore) LookupResultOnlyTaskEntry(key string) *ResultOnlyTaskEntry {
	if key == "" {
		return nil
	}
	s.ensureGeneration()
	release := s.lockShared()
	defer release()

	address := ResultOnlyTaskEntryAddress(key)
	blobDir := s.blobDir(address)
	entry := loadResultOnlyTaskEntry(blobDir, address, key)
	if entry == nil {
		return nil
	}
	writeLastUsed(blobDir, time.Now(), s.gen)
	return entry
}

// PublishResultOnlyTaskEntry records the result of a provider hit restored
// without its files, and returns the published entry.
//
// The hit must be a task-owned payload: its manifest carries exactly one entry
// descriptor at RemoteEntryDescriptorPath, with a well-formed digest. The
// descriptor's bytes are not read, because a result-only restore downloads no
// blob, so nothing here checks that the entry records hit.Key: the provider
// guarantees it answers with the entry stored under the requested key. A
// legacy payload, which carries no descriptor, is an error, and the caller
// treats it as a miss.
func (s *LocalStore) PublishResultOnlyTaskEntry(hit RemoteTaskEntryHit) (*ResultOnlyTaskEntry, error) {
	if hit.Key == "" {
		return nil, fmt.Errorf("publish result-only entry: cache key required")
	}
	if hit.Result == nil || hit.Result.Status == "" {
		return nil, fmt.Errorf("publish result-only entry %s: result required", hit.Key)
	}
	if hit.Manifest == nil {
		return nil, fmt.Errorf("publish result-only entry %s: manifest required", hit.Key)
	}
	_, descriptor, ok := splitTaskEntryManifest(hit.Manifest)
	if !ok {
		return nil, fmt.Errorf("%w: hit for %s carries no entry descriptor", ErrEntryFormat, hit.Key)
	}
	if !cache.ValidDigest(descriptor.Digest) {
		return nil, fmt.Errorf("%w: hit for %s has descriptor digest %q", ErrEntryFormat, hit.Key, descriptor.Digest)
	}

	s.ensureGeneration()
	release := s.lockShared()
	defer release()

	tmpDir, err := s.createTmpDir()
	if err != nil {
		return nil, fmt.Errorf("publish result-only entry: create tmp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir) // no-op once renamed; cleanup on every error path

	address := ResultOnlyTaskEntryAddress(hit.Key)
	meta := EntryMetadata{CreatedAt: time.Now()}
	if hit.Metadata != nil {
		meta = *hit.Metadata
		if meta.CreatedAt.IsZero() {
			meta.CreatedAt = time.Now()
		}
	}
	meta.Hash = address
	meta.Size = 0
	meta.OutputFiles = nil

	record := resultOnlyRecord{Format: CurrentResultOnlyTaskEntryFormat, Key: hit.Key, Result: hit.Result, Metadata: &meta}
	if err := writeJSONFile(filepath.Join(tmpDir, resultOnlyRecordFilename), record); err != nil {
		return nil, fmt.Errorf("publish result-only entry: write %s: %w", resultOnlyRecordFilename, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	blobDir := s.blobDir(address)
	if err := os.MkdirAll(filepath.Dir(blobDir), 0o755); err != nil {
		return nil, fmt.Errorf("publish result-only entry: create blob parent: %w", err)
	}
	if err := s.publishEntry(tmpDir, blobDir); err != nil {
		return nil, fmt.Errorf("publish result-only entry: %w", err)
	}
	writeLastUsed(blobDir, time.Now(), s.gen)

	published := loadResultOnlyTaskEntry(blobDir, address, hit.Key)
	if published == nil {
		return nil, fmt.Errorf("publish result-only entry %s: published entry is not readable", hit.Key)
	}
	return published, nil
}

// loadResultOnlyTaskEntry reads a result-only entry, or nil when there is none
// this build can serve for key. The caller holds the store's shared lock.
// Every rejection is silent: an absent, torn, wrongly formatted, foreign-keyed
// or statusless entry is a miss.
func loadResultOnlyTaskEntry(blobDir, address, key string) *ResultOnlyTaskEntry {
	data, err := os.ReadFile(filepath.Join(blobDir, resultOnlyRecordFilename))
	if err != nil {
		return nil
	}
	var record resultOnlyRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil
	}
	if record.Format != CurrentResultOnlyTaskEntryFormat || record.Key != key {
		return nil
	}
	if record.Result == nil || record.Result.Status == "" {
		return nil
	}
	return &ResultOnlyTaskEntry{Key: key, Address: address, Result: record.Result, Metadata: record.Metadata}
}
