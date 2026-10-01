package test

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/toolchain"
)

// TestListModulePackagesSortsWhatGoListOwns pins the listing's contract: the
// exact `go list` arguments, run in the module's directory with the caller's
// env, and a sorted result with blank lines dropped, so the -coverpkg built
// from it is deterministic whatever order go prints.
func TestListModulePackagesSortsWhatGoListOwns(t *testing.T) {
	var seen []string
	var seenDir string
	var seenEnv []string
	mockGoCommandRaw(t, func(_ string, args []string, dir string, env []string) ([]byte, error) {
		seen, seenDir, seenEnv = args, dir, env
		return []byte("example.com/a/y\n\nexample.com/a\nexample.com/a/x\n\n"), nil
	})
	packages, err := listModulePackages("go", "/work/a", []string{"GOWORK=/work/go.work"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"example.com/a", "example.com/a/x", "example.com/a/y"}; !slices.Equal(packages, want) {
		t.Fatalf("packages = %q, want sorted %q", packages, want)
	}
	if want := []string{"list", "-e", "-f", "{{if not .Error}}{{.ImportPath}}{{end}}", "./..."}; !slices.Equal(seen, want) {
		t.Fatalf("go list args = %q, want %q", seen, want)
	}
	if seenDir != "/work/a" || !slices.Equal(seenEnv, []string{"GOWORK=/work/go.work"}) {
		t.Fatalf("go list ran in %q with %q, want the module directory and the caller's env", seenDir, seenEnv)
	}
}

// TestListModulePackagesFailureCarriesWhatGoSaid pins that a listing failure
// reaches the diagnostic with go's own explanation, not a bare exit status.
func TestListModulePackagesFailureCarriesWhatGoSaid(t *testing.T) {
	mockGoCommandRaw(t, func(_ string, _ []string, _ string, _ []string) ([]byte, error) {
		return []byte("go: updates to go.mod needed; to update it:\n\tgo mod tidy\n"), errors.New("exit status 1")
	})
	packages, err := listModulePackages("go", "/work/a", nil)
	if err == nil || packages != nil {
		t.Fatalf("listModulePackages = %q, %v, want a failure", packages, err)
	}
	for _, want := range []string{"/work/a", "exit status 1", "go mod tidy"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
}

// TestRunGoCommandListKeepsStderrOutOfTheData pins the seam against the real
// toolchain: `go list ./...` in a module without packages exits 0 with a
// warning on stderr, and that warning must never come back as a package.
// The same seam keeps `go test` transcripts whole, stderr included.
func TestRunGoCommandListKeepsStderrOutOfTheData(t *testing.T) {
	goBinary, err := toolchain.ResolveGo()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module example.com/empty\n\ngo 1.21\n")
	env := toolchain.WorkspaceBuildEnv(nil, dir, goBinary)

	packages, err := listModulePackages(goBinary, dir, env)
	if err != nil {
		t.Fatalf("listModulePackages on an empty module: %v", err)
	}
	if len(packages) != 0 {
		t.Fatalf("empty module lists %q, want nothing; a stderr warning read as a package", packages)
	}

	// `go test` on the same selection fails; its explanation is on stderr
	// and the transcript must carry it.
	output, _ := runGoCommand(goBinary, []string{"test", "./..."}, dir, env)
	if !strings.Contains(string(output), "matched no packages") {
		t.Fatalf("go test transcript = %q, want its stderr explanation kept", output)
	}
}

// mockGoCommandRaw replaces the seam without the `go list` answer
// mockGoCommand supplies, for tests of the listing itself.
func mockGoCommandRaw(t *testing.T, mock func(binary string, args []string, dir string, env []string) ([]byte, error)) {
	t.Helper()
	original := runGoCommand
	runGoCommand = mock
	t.Cleanup(func() { runGoCommand = original })
}
