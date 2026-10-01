package oci

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// WriteDockerTarball closes every layout blob it opened. Windows refuses to
// remove an open file, so a leaked blob keeps the layout from being removed.
// The garbage collector closes a leaked file eventually, so it is off here.
func TestWriteDockerTarballClosesTheLayoutBlobs(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))

	binary := writeFile(t, t.TempDir(), "my-app", "binary")
	img, err := Assemble(testSpec(binary), syntheticBase(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layoutDir := filepath.Join(t.TempDir(), "oci")
	if _, err := WriteLayout(layoutDir, img); err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(t.TempDir(), "image.tar")
	if err := WriteDockerTarball(layoutDir, "myapp:c-abc123", tarPath); err != nil {
		t.Fatal(err)
	}

	assertLayoutBlobsClosed(t, layoutDir)
}

// Recording the layer streams changes nothing in the tarball: its bytes are
// the ones tarball.Write produces for the layout image itself.
func TestWriteDockerTarballBytesMatchTheUnwrappedImage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the reference tarball.Write leaves the layout blobs open, and Windows then refuses to remove the test's layout; the bytes do not depend on the OS")
	}
	binary := writeFile(t, t.TempDir(), "my-app", "binary")
	img, err := Assemble(testSpec(binary), syntheticBase(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layoutDir := filepath.Join(t.TempDir(), "oci")
	if _, err := WriteLayout(layoutDir, img); err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(t.TempDir(), "image.tar")
	if err := WriteDockerTarball(layoutDir, "myapp:c-abc123", tarPath); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(tarPath)
	if err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadFromLayout(layoutDir)
	if err != nil {
		t.Fatal(err)
	}
	tag, err := parseTag("myapp:c-abc123")
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	if err := tarball.Write(tag, loaded, &want); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Errorf("WriteDockerTarball wrote %d bytes that differ from tarball.Write's %d", len(got), want.Len())
	}
}
