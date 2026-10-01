package store

import (
	"bytes"
	"io"
	"testing"

	cache "go.putnami.dev/protocol/cache"
)

func TestCacheManagerRemoteFacade(t *testing.T) {
	local := NewLocalStore(t.TempDir())
	cm := NewCacheManager(local)

	entry, digest := hitEntry("dist/out.txt", "artifact")
	f := &inmemFetcher{blobs: map[string][]byte{digest: []byte("artifact")}}

	if err := cm.Materialize(hashA, entry, f.fetch); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	copied, err := cm.Lookup(hashA)
	if err != nil {
		t.Fatalf("Lookup materialized entry: %v", err)
	}
	if copied == nil || copied.Result.Status != "success" {
		t.Fatalf("materialized entry = %#v", copied)
	}

	reader, err := cm.OpenBlob(digest)
	if err != nil {
		t.Fatalf("OpenBlob materialized digest: %v", err)
	}
	reader.Close()
}

func TestCacheTrustAcceptsProviderChannels(t *testing.T) {
	tests := []struct {
		name    string
		trust   CacheTrust
		channel cache.Channel
		want    bool
	}{
		{"any trusted", CacheTrustAny, cache.ChannelTrusted, true},
		{"any hint", CacheTrustAny, cache.ChannelHint, true},
		{"any legacy", CacheTrustAny, "", true},
		{"ci trusted", CacheTrustCI, cache.ChannelTrusted, true},
		{"ci hint", CacheTrustCI, cache.ChannelHint, false},
		{"ci legacy", CacheTrustCI, "", false},
		{"none trusted", CacheTrustNone, cache.ChannelTrusted, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.trust.Accepts(tt.channel); got != tt.want {
				t.Errorf("Accepts(%q) = %t, want %t", tt.channel, got, tt.want)
			}
		})
	}
	for _, trust := range []CacheTrust{CacheTrustAny, CacheTrustCI, CacheTrustNone} {
		if !trust.Valid() {
			t.Errorf("%q should be valid", trust)
		}
	}
	if CacheTrust("unsafe").Valid() {
		t.Error("unknown trust policy should be invalid")
	}
	if !CacheTrustAny.AllowsRemoteRunMarkers() {
		t.Error("any should allow unproven remote run markers")
	}
	for _, trust := range []CacheTrust{CacheTrustCI, CacheTrustNone, CacheTrust("unsafe")} {
		if trust.AllowsRemoteRunMarkers() {
			t.Errorf("%q should reject unproven remote run markers", trust)
		}
	}
}

func TestCacheManagerPrefetchBlobsWarmsCASWithoutActionEntry(t *testing.T) {
	local := NewLocalStore(t.TempDir())
	cm := NewCacheManager(local)
	content := []byte("hint artifact")
	digest := cache.DigestOf(content)
	manifest := &cache.Manifest{Files: []cache.FileEntry{{
		Path: "dist/hint.txt", Digest: digest, Mode: 0o644, Size: int64(len(content)),
	}}}

	if err := cm.PrefetchBlobs(manifest, func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(content)), nil
	}); err != nil {
		t.Fatalf("PrefetchBlobs: %v", err)
	}
	if got, err := cm.Lookup(hashA); err != nil || got != nil {
		t.Fatalf("hint prefetch published action entry: entry=%#v err=%v", got, err)
	}
	r, err := cm.OpenBlob(digest)
	if err != nil {
		t.Fatalf("prefetched blob not present in CAS: %v", err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if string(got) != string(content) {
		t.Fatalf("prefetched bytes = %q, want %q", got, content)
	}
}
