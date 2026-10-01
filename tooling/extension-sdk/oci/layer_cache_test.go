package oci

import (
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func TestFileLayerCacheKey_Semantics(t *testing.T) {
	dir := t.TempDir()
	firstSource := writeFile(t, dir, "first", "same bytes")
	secondSource := writeFile(t, dir, "second", "same bytes")
	base := Layer{Files: []File{{Source: firstSource, Path: "/app/file", Mode: 0o644}}}
	baseKey, err := fileLayerCacheKey(base)
	if err != nil {
		t.Fatal(err)
	}

	sameBytes := Layer{Files: []File{{Source: secondSource, Path: "/app/file", Mode: 0o644}}}
	sameKey, err := fileLayerCacheKey(sameBytes)
	if err != nil {
		t.Fatal(err)
	}
	if sameKey != baseKey {
		t.Fatalf("machine-specific source path changed key: %s vs %s", baseKey, sameKey)
	}

	cases := []Layer{
		{Files: []File{{Source: firstSource, Path: "/app/other", Mode: 0o644}}},
		{Files: []File{{Source: firstSource, Path: "/app/file", Mode: 0o755}}},
		sameBytes,
	}
	writeFile(t, dir, "second", "changed bytes")
	for i, changed := range cases {
		key, err := fileLayerCacheKey(changed)
		if err != nil {
			t.Fatal(err)
		}
		if key == baseKey {
			t.Fatalf("semantic change %d did not change layer key", i)
		}
	}
}

func TestLayerCacheRootFromEnv(t *testing.T) {
	t.Setenv("PUTNAMI_OCI_CACHE_ROOT", "  /shared/oci  ")
	if got, want := LayerCacheRootFromEnv(), "/shared/oci"; got != want {
		t.Fatalf("LayerCacheRootFromEnv() = %q, want %q", got, want)
	}
}

func TestLoadOrBuildFileLayer_HitChangeAndCorruption(t *testing.T) {
	dir := t.TempDir()
	cacheRoot := filepath.Join(dir, "cache")
	source := writeFile(t, dir, "payload", "first content")
	input := Layer{Files: []File{{Source: source, Path: "/app/payload", Mode: 0o755}}}

	first, hit, err := loadOrBuildFileLayer(input, filepath.Join(dir, "first.tar.gz"), cacheRoot, types.DockerLayer)
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Fatal("first build unexpectedly hit cache")
	}
	firstDigest, _ := first.Digest()

	secondPath := filepath.Join(dir, "second.tar.gz")
	second, hit, err := loadOrBuildFileLayer(input, secondPath, cacheRoot, types.DockerLayer)
	if err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Fatal("second build missed cache")
	}
	secondDigest, _ := second.Digest()
	if secondDigest != firstDigest {
		t.Fatalf("cache hit digest = %s, first build = %s", secondDigest, firstDigest)
	}
	if staged := second.(*fileLayer).path; staged != secondPath {
		t.Fatalf("cache hit path = %q, want build-owned %q", staged, secondPath)
	}

	key, err := fileLayerCacheKey(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layerCacheEntryDir(cacheRoot, key), "layer.tar.gz"), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A hit is copied into the build work directory before it is returned, so a
	// concurrent collector changing the cache cannot break the active image.
	compressed, err := second.Compressed()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, compressed); err != nil {
		_ = compressed.Close()
		t.Fatalf("build-owned cache hit became unreadable: %v", err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	repaired, hit, err := loadOrBuildFileLayer(input, filepath.Join(dir, "repair.tar.gz"), cacheRoot, types.DockerLayer)
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Fatal("corrupt entry was reported as a hit")
	}
	repairedDigest, _ := repaired.Digest()
	if repairedDigest != firstDigest {
		t.Fatalf("repaired digest = %s, want %s", repairedDigest, firstDigest)
	}

	writeFile(t, dir, "payload", "changed content")
	changed, hit, err := loadOrBuildFileLayer(input, filepath.Join(dir, "changed.tar.gz"), cacheRoot, types.DockerLayer)
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Fatal("changed source unexpectedly hit cache")
	}
	changedDigest, _ := changed.Digest()
	if changedDigest == firstDigest {
		t.Fatal("changed source produced the cached digest")
	}
}

func TestLoadOrBuildFileLayer_ConcurrentWritersConverge(t *testing.T) {
	dir := t.TempDir()
	cacheRoot := filepath.Join(dir, "cache")
	source := writeFile(t, dir, "payload", strings.Repeat("concurrent", 1024))
	input := Layer{Files: []File{{Source: source, Path: "/payload", Mode: 0o644}}}

	const writers = 8
	digests := make(chan v1.Hash, writers)
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			workDir := filepath.Join(dir, "work", string(rune('a'+i)))
			if err := os.MkdirAll(workDir, 0o755); err != nil {
				errs <- err
				return
			}
			layer, _, err := loadOrBuildFileLayer(input, filepath.Join(workDir, "layer.tar.gz"), cacheRoot, types.DockerLayer)
			if err != nil {
				errs <- err
				return
			}
			digest, err := layer.Digest()
			if err != nil {
				errs <- err
				return
			}
			digests <- digest
		}(i)
	}
	wg.Wait()
	close(errs)
	close(digests)
	for err := range errs {
		t.Fatal(err)
	}
	var want v1.Hash
	for digest := range digests {
		if want == (v1.Hash{}) {
			want = digest
		} else if digest != want {
			t.Fatalf("concurrent digest = %s, want %s", digest, want)
		}
	}

	_, hit, err := loadOrBuildFileLayer(input, filepath.Join(dir, "after.tar.gz"), cacheRoot, types.DockerLayer)
	if err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Fatal("concurrent writers did not leave a usable cache entry")
	}
}

func BenchmarkFileLayerCacheHit32MiB(b *testing.B) {
	const fixtureSize = int64(32 << 20)
	dir := b.TempDir()
	source := filepath.Join(dir, "payload")
	f, err := os.Create(source)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := io.CopyN(f, rand.New(rand.NewSource(3323)), fixtureSize); err != nil {
		_ = f.Close()
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	input := Layer{Files: []File{{Source: source, Path: "/app/payload", Mode: 0o755}}}
	cacheRoot := filepath.Join(dir, "cache")
	if _, _, err := loadOrBuildFileLayer(input, filepath.Join(dir, "cold.tar.gz"), cacheRoot, types.DockerLayer); err != nil {
		b.Fatal(err)
	}

	b.SetBytes(fixtureSize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		layer, hit, err := loadOrBuildFileLayer(input, filepath.Join(dir, "unused.tar.gz"), cacheRoot, types.DockerLayer)
		if err != nil {
			b.Fatal(err)
		}
		if !hit {
			b.Fatal("cache miss")
		}
		compressed, err := layer.Compressed()
		if err != nil {
			b.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, compressed); err != nil {
			_ = compressed.Close()
			b.Fatal(err)
		}
		if err := compressed.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFileLayerCacheMiss32MiB(b *testing.B) {
	const fixtureSize = int64(32 << 20)
	dir := b.TempDir()
	source := filepath.Join(dir, "payload")
	f, err := os.Create(source)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := io.CopyN(f, rand.New(rand.NewSource(3323)), fixtureSize); err != nil {
		_ = f.Close()
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	input := Layer{Files: []File{{Source: source, Path: "/app/payload", Mode: 0o755}}}

	b.SetBytes(fixtureSize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		suffix := strconv.Itoa(i)
		if _, hit, err := loadOrBuildFileLayer(
			input,
			filepath.Join(dir, "cold-"+suffix+".tar.gz"),
			filepath.Join(dir, "cache-"+suffix),
			types.DockerLayer,
		); err != nil {
			b.Fatal(err)
		} else if hit {
			b.Fatal("cache hit")
		}
	}
}

// BenchmarkImagePlanCacheHit32MiB measures the complete identity+layer lookup
// boundary. The legacy composition hashes the source once for ContentHash and
// again to discover the layer key. ImagePlan shares the first fingerprint;
// a trustworthy upstream fingerprint removes the source read entirely.
func BenchmarkImagePlanCacheHit32MiB(b *testing.B) {
	const fixtureSize = int64(32 << 20)
	dir := b.TempDir()
	source := filepath.Join(dir, "payload")
	f, err := os.Create(source)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := io.CopyN(f, rand.New(rand.NewSource(3323)), fixtureSize); err != nil {
		_ = f.Close()
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	spec := Spec{Layers: []Layer{{Files: []File{{Source: source, Path: "/app/payload", Mode: 0o755}}}}}
	seedPlan, err := NewImagePlan(spec, PlanOptions{})
	if err != nil {
		b.Fatal(err)
	}
	cacheRoot := filepath.Join(dir, "cache")
	if _, _, err := loadOrBuildPlannedFileLayer(spec.Layers[0], &seedPlan.Layers[0], filepath.Join(dir, "cold.tar.gz"), cacheRoot, types.DockerLayer); err != nil {
		b.Fatal(err)
	}
	fingerprint := *seedPlan.Layers[0].Files[0].Fingerprint

	b.Run("legacy-content-hash", func(b *testing.B) {
		b.SetBytes(fixtureSize)
		b.ReportAllocs()
		b.ReportMetric(2, "source-passes/op")
		for i := 0; i < b.N; i++ {
			if _, err := ContentHash(spec); err != nil {
				b.Fatal(err)
			}
			if _, hit, err := loadOrBuildFileLayer(spec.Layers[0], filepath.Join(dir, "legacy-hit.tar.gz"), cacheRoot, types.DockerLayer); err != nil {
				b.Fatal(err)
			} else if !hit {
				b.Fatal("cache miss")
			}
		}
	})
	b.Run("shared-image-plan", func(b *testing.B) {
		b.SetBytes(fixtureSize)
		b.ReportAllocs()
		b.ReportMetric(1, "source-passes/op")
		for i := 0; i < b.N; i++ {
			plan, err := NewImagePlan(spec, PlanOptions{})
			if err != nil {
				b.Fatal(err)
			}
			if _, hit, err := loadOrBuildPlannedFileLayer(spec.Layers[0], &plan.Layers[0], filepath.Join(dir, "planned-hit.tar.gz"), cacheRoot, types.DockerLayer); err != nil {
				b.Fatal(err)
			} else if !hit {
				b.Fatal("cache miss")
			}
		}
	})
	b.Run("trusted-fingerprint", func(b *testing.B) {
		b.SetBytes(fixtureSize)
		b.ReportAllocs()
		b.ReportMetric(0, "source-passes/op")
		trusted := map[string]SourceFingerprint{source: fingerprint}
		for i := 0; i < b.N; i++ {
			plan, err := NewImagePlan(spec, PlanOptions{TrustedFingerprints: trusted})
			if err != nil {
				b.Fatal(err)
			}
			if _, hit, err := loadOrBuildPlannedFileLayer(spec.Layers[0], &plan.Layers[0], filepath.Join(dir, "trusted-hit.tar.gz"), cacheRoot, types.DockerLayer); err != nil {
				b.Fatal(err)
			} else if !hit {
				b.Fatal("cache miss")
			}
		}
	})
}

func BenchmarkImagePlanCacheMiss32MiB(b *testing.B) {
	const fixtureSize = int64(32 << 20)
	dir := b.TempDir()
	source := filepath.Join(dir, "payload")
	f, err := os.Create(source)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := io.CopyN(f, rand.New(rand.NewSource(3323)), fixtureSize); err != nil {
		_ = f.Close()
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	spec := Spec{Layers: []Layer{{Files: []File{{Source: source, Path: "/app/payload", Mode: 0o755}}}}}
	seedPlan, err := NewImagePlan(spec, PlanOptions{})
	if err != nil {
		b.Fatal(err)
	}
	fingerprint := *seedPlan.Layers[0].Files[0].Fingerprint

	b.Run("unshared-identities", func(b *testing.B) {
		runDir := b.TempDir()
		b.SetBytes(fixtureSize)
		b.ReportAllocs()
		b.ReportMetric(3, "source-passes/op")
		for i := 0; i < b.N; i++ {
			suffix := strconv.Itoa(i)
			if _, err := ContentHash(spec); err != nil {
				b.Fatal(err)
			}
			if _, hit, err := loadOrBuildFileLayer(spec.Layers[0], filepath.Join(runDir, "legacy-miss-"+suffix+".tar.gz"), filepath.Join(runDir, "legacy-cache-"+suffix), types.DockerLayer); err != nil {
				b.Fatal(err)
			} else if hit {
				b.Fatal("cache hit")
			}
		}
	})
	b.Run("shared-image-plan", func(b *testing.B) {
		runDir := b.TempDir()
		b.SetBytes(fixtureSize)
		b.ReportAllocs()
		b.ReportMetric(2, "source-passes/op")
		for i := 0; i < b.N; i++ {
			suffix := strconv.Itoa(i)
			plan, err := NewImagePlan(spec, PlanOptions{})
			if err != nil {
				b.Fatal(err)
			}
			if _, hit, err := loadOrBuildPlannedFileLayer(spec.Layers[0], &plan.Layers[0], filepath.Join(runDir, "planned-miss-"+suffix+".tar.gz"), filepath.Join(runDir, "planned-cache-"+suffix), types.DockerLayer); err != nil {
				b.Fatal(err)
			} else if hit {
				b.Fatal("cache hit")
			}
		}
	})
	b.Run("trusted-fingerprint", func(b *testing.B) {
		runDir := b.TempDir()
		b.SetBytes(fixtureSize)
		b.ReportAllocs()
		b.ReportMetric(1, "source-passes/op")
		trusted := map[string]SourceFingerprint{source: fingerprint}
		for i := 0; i < b.N; i++ {
			suffix := strconv.Itoa(i)
			plan, err := NewImagePlan(spec, PlanOptions{TrustedFingerprints: trusted})
			if err != nil {
				b.Fatal(err)
			}
			if _, hit, err := loadOrBuildPlannedFileLayer(spec.Layers[0], &plan.Layers[0], filepath.Join(runDir, "trusted-miss-"+suffix+".tar.gz"), filepath.Join(runDir, "trusted-cache-"+suffix), types.DockerLayer); err != nil {
				b.Fatal(err)
			} else if hit {
				b.Fatal("cache hit")
			}
		}
	})
}
