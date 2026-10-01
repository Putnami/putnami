//go:build windows

package jobs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A provider declared by a bare name whose file is a .cmd shim resolves through
// PATHEXT and starts, as it did when os/exec resolved it against this process's
// PATH.
func TestProviderExecutableStartsABareCmdShim(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "provider-shim.cmd")
	if err := os.WriteFile(shim, []byte("@echo provider-shim-ran\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := ProviderExecutable("provider-shim", []string{"Path=" + dir})
	if err != nil || !strings.EqualFold(path, shim) {
		t.Fatalf("ProviderExecutable = %q, %v; want %q", path, err, shim)
	}
	//nolint:gosec // G204: the shim is this test's own fixture.
	out, err := exec.Command(path).Output()
	if err != nil || strings.TrimSpace(string(out)) != "provider-shim-ran" {
		t.Fatalf("the resolved shim ran with %v and printed %q", err, out)
	}
}
