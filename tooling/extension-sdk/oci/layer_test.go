package oci

import (
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func TestBuildFileLayer_DeterministicAndReopenable(t *testing.T) {
	dir := t.TempDir()
	source := writeFile(t, dir, "payload", "deterministic payload")
	input := Layer{Files: []File{{Source: source, Path: "/app/payload", Mode: 0o755}}}

	first, err := buildFileLayer(input, filepath.Join(dir, "first.tar.gz"), types.DockerLayer)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildFileLayer(input, filepath.Join(dir, "second.tar.gz"), types.DockerLayer)
	if err != nil {
		t.Fatal(err)
	}

	firstDigest, _ := first.Digest()
	secondDigest, _ := second.Digest()
	if firstDigest != secondDigest {
		t.Fatalf("compressed digest changed: %s vs %s", firstDigest, secondDigest)
	}
	firstDiffID, _ := first.DiffID()
	secondDiffID, _ := second.DiffID()
	if firstDiffID != secondDiffID {
		t.Fatalf("diffID changed: %s vs %s", firstDiffID, secondDiffID)
	}

	compressed, err := first.Compressed()
	if err != nil {
		t.Fatal(err)
	}
	compressedDigest, compressedSize, hashErr := v1.SHA256(compressed)
	closeErr := compressed.Close()
	if hashErr != nil {
		t.Fatal(hashErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	descriptorSize, _ := first.Size()
	if compressedDigest != firstDigest || compressedSize != descriptorSize {
		t.Fatalf("compressed blob = (%s, %d), descriptor = (%s, %d)", compressedDigest, compressedSize, firstDigest, descriptorSize)
	}

	uncompressed, err := first.Uncompressed()
	if err != nil {
		t.Fatal(err)
	}
	uncompressedDigest, _, hashErr := v1.SHA256(uncompressed)
	closeErr = uncompressed.Close()
	if hashErr != nil {
		t.Fatal(hashErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if uncompressedDigest != firstDiffID {
		t.Fatalf("uncompressed digest = %s, diffID = %s", uncompressedDigest, firstDiffID)
	}
}

func TestBuildFileLayer_RemovesPartialBlobOnFailure(t *testing.T) {
	blobPath := filepath.Join(t.TempDir(), "partial.tar.gz")
	input := Layer{Files: []File{{Source: filepath.Join(t.TempDir(), "missing"), Path: "/missing", Mode: 0o644}}}
	if _, err := buildFileLayer(input, blobPath, types.DockerLayer); err == nil {
		t.Fatal("expected missing source to fail")
	}
	if _, err := os.Stat(blobPath); !os.IsNotExist(err) {
		t.Fatalf("partial blob was not removed: %v", err)
	}
}

func BenchmarkBuildFileLayer32MiB(b *testing.B) {
	const fixtureSize = int64(32 << 20)
	source := filepath.Join(b.TempDir(), "payload")
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
	blobPath := filepath.Join(b.TempDir(), "layer.tar.gz")

	b.SetBytes(fixtureSize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		layer, err := buildFileLayer(input, blobPath, types.DockerLayer)
		if err != nil {
			b.Fatal(err)
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
