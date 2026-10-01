//go:build windows

package output

import (
	"os"
	"path/filepath"
	"testing"
)

// A regular file is not a console, so it is never a terminal and live
// rendering never writes escape sequences into it.
func TestIsTTYRejectsARegularFile(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if isTTY(int(file.Fd())) {
		t.Fatal("isTTY reported a regular file as a terminal")
	}
	if ShouldUseLiveRenderer(file) {
		t.Fatal("live rendering chose a regular file")
	}
}

func TestTermSizeIsNeverEmpty(t *testing.T) {
	if w, h := termSize(); w <= 0 || h <= 0 {
		t.Fatalf("termSize = %dx%d, want a positive size", w, h)
	}
}
