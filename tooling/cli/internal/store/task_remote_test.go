package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	cache "go.putnami.dev/protocol/cache"
	proto "go.putnami.dev/protocol/extension"
)

// exchange is a stand-in for the provider's blob exchange directory: the bytes
// one machine staged, addressed by digest, readable by the other.
type exchange map[string][]byte

func (e exchange) fetcher() BlobFetcher {
	return func(digest string) (io.ReadCloser, error) {
		content, ok := e[digest]
		if !ok {
			return nil, errors.New("blob not on the exchange: " + digest)
		}
		return io.NopCloser(bytes.NewReader(content)), nil
	}
}

// uploadTo exports everything a task-owned entry needs to travel: the payload
// blobs the CAS holds plus the descriptor bytes it does not.
func uploadTo(t *testing.T, s *LocalStore, entry *TaskEntry, ex exchange) *TaskEntryTransfer {
	t.Helper()
	transfer, err := NewTaskEntryTransfer(entry)
	if err != nil {
		t.Fatalf("NewTaskEntryTransfer: %v", err)
	}
	for _, f := range transfer.Payload.Files {
		rc, err := s.OpenBlob(f.Digest)
		if err != nil {
			t.Fatalf("open payload blob %s: %v", f.Digest, err)
		}
		content, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read payload blob %s: %v", f.Digest, err)
		}
		ex[f.Digest] = content
	}
	ex[transfer.DescriptorDigest] = transfer.Descriptor
	return transfer
}

func hitFor(transfer *TaskEntryTransfer, key string) RemoteTaskEntryHit {
	return RemoteTaskEntryHit{
		Key:      key,
		Result:   &EntryResult{Status: "success"},
		Metadata: &EntryMetadata{Extension: "ext", Task: "build~transpile", Project: "pkg", DurationMs: 1500},
		Manifest: transfer.Manifest,
	}
}

// TestRemoteTaskEntryKeyIsFormatQualified pins DECISION 1: a task-owned entry
// never travels under the raw cache key. A pre-B4c binary asks the provider for
// the raw key, so publishing a format-2 payload there would hand it a payload it
// would materialize as a legacy capture — silent corruption no newer build can
// prevent.
func TestRemoteTaskEntryKeyIsFormatQualified(t *testing.T) {
	const key = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	remote := RemoteTaskEntryKey(key)
	if remote == key {
		t.Fatal("the remote key must not be the raw cache key: an older CLI reads that address as a legacy entry")
	}
	if remote != TaskEntryAddress(key) {
		t.Fatalf("remote key = %q, want the local task-owned address %q", remote, TaskEntryAddress(key))
	}
	if !cache.ValidKey(remote) {
		t.Fatalf("remote key %q is not a wire-valid cache key; the provider contract must stay unchanged", remote)
	}
}

// TestTaskEntryRoundTripsThroughTheExchange is the transport happy path: an
// entry published on one machine is materialized on another from the wire
// manifest alone, and the two entries are indistinguishable — same address, same
// descriptor bytes, same payload manifest, same restored bytes.
func TestTaskEntryRoundTripsThroughTheExchange(t *testing.T) {
	producer := NewLocalStore(t.TempDir())
	consumer := NewLocalStore(t.TempDir())
	ex := exchange{}

	staging := t.TempDir()
	dist := dirOutput("dist", "dist")
	lock := DeclaredEntryOutput{ID: "lock", Kind: proto.OutputKindFile, Root: proto.OutputRootWorkspace, Path: "bun.lock"}
	empty := DeclaredEntryOutput{ID: "coverage", Kind: proto.OutputKindFile, Root: proto.OutputRootProject, Path: "coverage.json", Optional: true}
	stage(t, staging, dist, "main.js", "built")
	stage(t, staging, dist, "nested/chunk.js", "chunk")
	stage(t, staging, lock, "", "lockfile")

	published, err := producer.IngestTaskEntry(staging, taskSpec("round-trip-key", dist, lock, empty))
	if err != nil {
		t.Fatalf("IngestTaskEntry: %v", err)
	}
	transfer := uploadTo(t, producer, published, ex)

	// The descriptor rides as ordinary content at the reserved path, so the wire
	// manifest is the payload plus exactly one more file.
	if len(transfer.Manifest.Files) != len(transfer.Payload.Files)+1 {
		t.Fatalf("wire manifest has %d files, payload %d: the descriptor must travel as content",
			len(transfer.Manifest.Files), len(transfer.Payload.Files))
	}

	restored, err := consumer.MaterializeTaskEntry(hitFor(transfer, "round-trip-key"), ex.fetcher())
	if err != nil {
		t.Fatalf("MaterializeTaskEntry: %v", err)
	}
	if restored.Address != published.Address {
		t.Fatalf("restored address %q != published %q", restored.Address, published.Address)
	}
	if !bytes.Equal(restored.Descriptor, published.Descriptor) {
		t.Fatalf("descriptor bytes differ across the wire:\n got %s\nwant %s", restored.Descriptor, published.Descriptor)
	}
	if !reflect.DeepEqual(restored.Outputs, published.Outputs) {
		t.Fatalf("recorded outputs differ: %+v vs %+v", restored.Outputs, published.Outputs)
	}
	// manifest.json holds the PAYLOAD only, so GC accounting and a re-upload see
	// exactly what a locally captured entry has.
	if restored.Manifest == nil || len(restored.Manifest.Files) != len(published.Manifest.Files) {
		t.Fatalf("restored manifest = %+v, want the published payload manifest", restored.Manifest)
	}
	for _, f := range restored.Manifest.Files {
		if f.Path == RemoteEntryDescriptorPath {
			t.Fatal("the descriptor must not be recorded in the local manifest: GC would then keep an exchange-only blob alive")
		}
	}

	// The restored entry materializes byte-identically through the one primitive.
	dest := filepath.Join(t.TempDir(), "dist")
	if wrote, err := consumer.MaterializeTaskOutput(restored, "dist", dest, ""); err != nil || !wrote {
		t.Fatalf("MaterializeTaskOutput = %v, %v", wrote, err)
	}
	if got := readAt(t, filepath.Join(dest, "main.js")); got != "built" {
		t.Fatalf("restored main.js = %q, want %q", got, "built")
	}
	if got := readAt(t, filepath.Join(dest, "nested", "chunk.js")); got != "chunk" {
		t.Fatalf("restored nested/chunk.js = %q, want %q", got, "chunk")
	}

	// Optional-empty survives the wire as a STATE, not as an absence: restoring
	// it must not touch the destination.
	coverage := filepath.Join(t.TempDir(), "coverage.json")
	if err := os.WriteFile(coverage, []byte("sibling bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	wrote, err := consumer.MaterializeTaskOutput(restored, "coverage", coverage, "")
	if err != nil || wrote {
		t.Fatalf("materializing a remotely-restored empty output = %v, %v; want (false, nil)", wrote, err)
	}
	if got := readAt(t, coverage); got != "sibling bytes" {
		t.Fatalf("an empty output overwrote its destination: %q", got)
	}

	// A restored entry re-uploads with the same digests: the round trip is
	// digest-stable, so a warm machine moves no new bytes.
	reupload, err := NewTaskEntryTransfer(restored)
	if err != nil {
		t.Fatalf("NewTaskEntryTransfer(restored): %v", err)
	}
	if reupload.DescriptorDigest != transfer.DescriptorDigest {
		t.Fatalf("descriptor digest changed on re-upload: %s != %s", reupload.DescriptorDigest, transfer.DescriptorDigest)
	}
	if !reflect.DeepEqual(reupload.Manifest, transfer.Manifest) {
		t.Fatalf("wire manifest changed on re-upload:\n got %+v\nwant %+v", reupload.Manifest, transfer.Manifest)
	}
}

// TestMaterializeTaskEntry_FailsClosed is the fail-closed matrix: every payload
// this build cannot interpret is a MISS, and NOTHING is published — a wrongly
// accepted entry materializes the wrong tree, a wrongly rejected one costs a
// recompute.
func TestMaterializeTaskEntry_FailsClosed(t *testing.T) {
	const key = "fail-closed-key"

	cases := []struct {
		name string
		// mutate rewrites the wire hit (and may rewrite the exchange) into the
		// shape under test.
		mutate func(t *testing.T, hit *RemoteTaskEntryHit, ex exchange, transfer *TaskEntryTransfer)
	}{
		{
			name: "legacy payload carries no descriptor",
			mutate: func(_ *testing.T, hit *RemoteTaskEntryHit, _ exchange, transfer *TaskEntryTransfer) {
				// Exactly what a pre-B4 CLI uploaded under this key: files, no
				// descriptor.
				hit.Manifest = transfer.Payload
			},
		},
		{
			name: "two descriptors",
			mutate: func(_ *testing.T, hit *RemoteTaskEntryHit, _ exchange, transfer *TaskEntryTransfer) {
				files := append([]cache.FileEntry{}, transfer.Manifest.Files...)
				for _, f := range transfer.Manifest.Files {
					if f.Path == RemoteEntryDescriptorPath {
						files = append(files, f)
					}
				}
				hit.Manifest = &cache.Manifest{Files: files}
			},
		},
		{
			// Tampering that NOTHING but content addressing can catch: the swapped
			// descriptor is well-formed, records the right key, and agrees with the
			// manifest — it only redirects the output to another path. Accepting it
			// would write the payload somewhere the declaration never named.
			name: "descriptor bytes do not match their digest",
			mutate: func(t *testing.T, hit *RemoteTaskEntryHit, ex exchange, transfer *TaskEntryTransfer) {
				var entry TaskEntry
				if err := json.Unmarshal(transfer.Descriptor, &entry); err != nil {
					t.Fatalf("decode descriptor: %v", err)
				}
				for i := range entry.Outputs {
					if entry.Outputs[i].ID == "dist" {
						entry.Outputs[i].Path = "somewhere-else"
					}
				}
				tampered, err := json.MarshalIndent(&entry, "", "  ")
				if err != nil {
					t.Fatalf("encode descriptor: %v", err)
				}
				// Served under the digest the manifest still claims.
				ex[transfer.DescriptorDigest] = tampered
				_ = hit
			},
		},
		{
			name: "descriptor records another key",
			mutate: func(t *testing.T, hit *RemoteTaskEntryHit, ex exchange, transfer *TaskEntryTransfer) {
				replaceDescriptor(t, hit, ex, transfer, func(entry *TaskEntry) { entry.Key = "another-key" })
			},
		},
		{
			name: "descriptor is in an unreadable format",
			mutate: func(t *testing.T, hit *RemoteTaskEntryHit, ex exchange, transfer *TaskEntryTransfer) {
				replaceDescriptor(t, hit, ex, transfer, func(entry *TaskEntry) { entry.Format = CurrentEntryFormat + 1 })
			},
		},
		{
			name: "payload is torn: a recorded file is missing",
			mutate: func(_ *testing.T, hit *RemoteTaskEntryHit, _ exchange, transfer *TaskEntryTransfer) {
				files := make([]cache.FileEntry, 0, len(transfer.Manifest.Files))
				dropped := false
				for _, f := range transfer.Manifest.Files {
					if !dropped && f.Path != RemoteEntryDescriptorPath {
						dropped = true
						continue
					}
					files = append(files, f)
				}
				hit.Manifest = &cache.Manifest{Files: files}
			},
		},
		{
			name: "payload carries a file for an undeclared output",
			mutate: func(_ *testing.T, hit *RemoteTaskEntryHit, ex exchange, transfer *TaskEntryTransfer) {
				content := []byte("smuggled")
				digest := cache.DigestOf(content)
				ex[digest] = content
				files := append([]cache.FileEntry{}, transfer.Manifest.Files...)
				files = append(files, cache.FileEntry{Path: "smuggled/x.js", Digest: digest, Mode: 0o644, Size: int64(len(content))})
				hit.Manifest = &cache.Manifest{Files: files}
			},
		},
		{
			name: "an output recorded empty arrives with bytes",
			mutate: func(_ *testing.T, hit *RemoteTaskEntryHit, ex exchange, transfer *TaskEntryTransfer) {
				content := []byte("{}")
				digest := cache.DigestOf(content)
				ex[digest] = content
				files := append([]cache.FileEntry{}, transfer.Manifest.Files...)
				files = append(files, cache.FileEntry{Path: "coverage", Digest: digest, Mode: 0o644, Size: int64(len(content))})
				hit.Manifest = &cache.Manifest{Files: files}
			},
		},
		{
			name: "the descriptor blob is not on the exchange",
			mutate: func(_ *testing.T, _ *RemoteTaskEntryHit, ex exchange, transfer *TaskEntryTransfer) {
				delete(ex, transfer.DescriptorDigest)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			producer := NewLocalStore(t.TempDir())
			consumer := NewLocalStore(t.TempDir())
			ex := exchange{}

			staging := t.TempDir()
			dist := dirOutput("dist", "dist")
			empty := DeclaredEntryOutput{ID: "coverage", Kind: proto.OutputKindFile, Root: proto.OutputRootProject, Path: "coverage.json", Optional: true}
			stage(t, staging, dist, "main.js", "built")
			stage(t, staging, dist, "nested/chunk.js", "chunk")
			published, err := producer.IngestTaskEntry(staging, taskSpec(key, dist, empty))
			if err != nil {
				t.Fatalf("IngestTaskEntry: %v", err)
			}
			transfer := uploadTo(t, producer, published, ex)

			hit := hitFor(transfer, key)
			tc.mutate(t, &hit, ex, transfer)

			restored, err := consumer.MaterializeTaskEntry(hit, ex.fetcher())
			if err == nil || restored != nil {
				t.Fatalf("an uninterpretable payload was accepted: entry=%+v err=%v", restored, err)
			}
			// Nothing published: the next lookup is a clean miss, not a partial
			// entry that would restore less than the declaration promises.
			if entry, err := consumer.LookupTaskEntry(key); err != nil || entry != nil {
				t.Fatalf("a rejected payload left an entry behind: %+v (err=%v)", entry, err)
			}
		})
	}

	// Positive control: the same fixture, unmutated, is accepted. Without it
	// every case above would pass on a materialize that never works.
	t.Run("positive control", func(t *testing.T) {
		producer := NewLocalStore(t.TempDir())
		consumer := NewLocalStore(t.TempDir())
		ex := exchange{}
		staging := t.TempDir()
		dist := dirOutput("dist", "dist")
		empty := DeclaredEntryOutput{ID: "coverage", Kind: proto.OutputKindFile, Root: proto.OutputRootProject, Path: "coverage.json", Optional: true}
		stage(t, staging, dist, "main.js", "built")
		stage(t, staging, dist, "nested/chunk.js", "chunk")
		published, err := producer.IngestTaskEntry(staging, taskSpec(key, dist, empty))
		if err != nil {
			t.Fatalf("IngestTaskEntry: %v", err)
		}
		transfer := uploadTo(t, producer, published, ex)
		if _, err := consumer.MaterializeTaskEntry(hitFor(transfer, key), ex.fetcher()); err != nil {
			t.Fatalf("the unmutated fixture must materialize: %v", err)
		}
		if entry, err := consumer.LookupTaskEntry(key); err != nil || entry == nil {
			t.Fatalf("the unmutated fixture published no entry: %+v (err=%v)", entry, err)
		}
	})
}

// TestMaterializeTaskEntry_RejectsALegacyBlobAtTheAddress is the other direction
// of the format boundary: even reached at the task-owned address, a payload
// without a descriptor is never interpreted as declared output.
func TestMaterializeTaskEntry_RejectsALegacyBlobAtTheAddress(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	content := []byte("legacy bytes")
	digest := cache.DigestOf(content)
	ex := exchange{digest: content}

	_, err := s.MaterializeTaskEntry(RemoteTaskEntryHit{
		Key:      "legacy-shaped-key",
		Result:   &EntryResult{Status: "success"},
		Manifest: &cache.Manifest{Files: []cache.FileEntry{{Path: "dist/out.js", Digest: digest, Mode: 0o644, Size: int64(len(content))}}},
	}, ex.fetcher())
	if !errors.Is(err, ErrEntryFormat) {
		t.Fatalf("a descriptor-less payload must be rejected as a format error, got %v", err)
	}
}

// TestReservedDescriptorIDIsRefusedAtIngest keeps the reserved manifest path
// unforgeable: no declared output may own the id the descriptor travels under,
// so a remote payload can never be ambiguous about which file is the descriptor.
func TestReservedDescriptorIDIsRefusedAtIngest(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	staging := t.TempDir()
	out := DeclaredEntryOutput{
		ID: RemoteEntryDescriptorPath, Kind: proto.OutputKindFile,
		Root: proto.OutputRootProject, Path: "descriptor.json",
	}
	stage(t, staging, out, "", "{}")

	if _, err := s.IngestTaskEntry(staging, taskSpec("reserved-id-key", out)); err == nil ||
		!strings.Contains(err.Error(), "reserved") {
		t.Fatalf("an output claiming the reserved descriptor id must be refused, got %v", err)
	}
}

// TestStageExchangeBlob_IsContentAddressedAndAtomic pins that the descriptor is
// handed to the provider the way a real one stages blobs: at its digest path,
// via a rename, and idempotently for a digest a sibling upload already staged.
func TestStageExchangeBlob_IsContentAddressedAndAtomic(t *testing.T) {
	dir := t.TempDir()
	content := []byte(`{"entryFormat":2}`)
	digest := cache.DigestOf(content)

	if err := StageExchangeBlob(dir, digest, content); err != nil {
		t.Fatalf("StageExchangeBlob: %v", err)
	}
	path, ok := cache.BlobExchangePath(dir, digest)
	if !ok {
		t.Fatal("BlobExchangePath rejected a valid digest")
	}
	if got := readAt(t, path); got != string(content) {
		t.Fatalf("staged content = %q, want %q", got, content)
	}
	// Second call is a no-op, not a truncate-and-rewrite a provider could read
	// mid-flight.
	if err := StageExchangeBlob(dir, digest, content); err != nil {
		t.Fatalf("re-staging the same digest: %v", err)
	}
	if err := StageExchangeBlob(dir, "not-a-digest", content); err == nil {
		t.Fatal("an invalid digest must be refused")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("exchange dir holds %d entries, want only the blob (no leftover temp file)", len(entries))
	}
}

// replaceDescriptor rewrites the descriptor on the exchange (and its manifest
// entry) with a mutated one, so a test can ship a descriptor that is internally
// valid JSON but wrong for this hit.
func replaceDescriptor(t *testing.T, hit *RemoteTaskEntryHit, ex exchange, transfer *TaskEntryTransfer, mutate func(*TaskEntry)) {
	t.Helper()
	var entry TaskEntry
	if err := json.Unmarshal(transfer.Descriptor, &entry); err != nil {
		t.Fatalf("decode descriptor: %v", err)
	}
	mutate(&entry)
	data, err := json.MarshalIndent(&entry, "", "  ")
	if err != nil {
		t.Fatalf("encode descriptor: %v", err)
	}
	digest := cache.DigestOf(data)
	ex[digest] = data

	files := make([]cache.FileEntry, 0, len(transfer.Manifest.Files))
	for _, f := range transfer.Manifest.Files {
		if f.Path == RemoteEntryDescriptorPath {
			f.Digest = digest
			f.Size = int64(len(data))
		}
		files = append(files, f)
	}
	hit.Manifest = &cache.Manifest{Files: files}
}

func readAt(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
