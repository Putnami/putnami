package extensions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// A held pin whose lock install fails names the exact install of its own
// scope: the user scope's hint names install --user, never the workspace
// command, and templates name templates install.
func TestAHeldPinHintNamesTheInstallOfItsScope(t *testing.T) {
	userRoot := t.TempDir()
	ops := userScopeOps(userRoot, extension.NewUserScopeInstaller(userRoot))
	ops.resolve = func(context.Context, string, string) (*artifactInstallOutcome, error) {
		return &artifactInstallOutcome{Version: "1.0.0"}, nil
	}
	ops.install = func(_ context.Context, name, _ string, lockEntry *lockfile.LockEntry) (*artifactInstallOutcome, error) {
		if lockEntry == nil {
			t.Error("the held pin downloaded the older build")
			return &artifactInstallOutcome{Version: "1.0.0"}, nil
		}
		return nil, fmt.Errorf("integrity mismatch for %s@%s", name, lockEntry.Version)
	}
	item := artifactInstallWork{name: "@acme/tool", constraint: "latest", prior: lockfile.LockEntry{Version: "1.1.0"}, hadPrior: true, holdsPin: true}

	installHoldingPin(context.Background(), ops, &item)
	want := "; a channel request keeps this pin, so run `putnami extensions install --user @acme/tool@1.1.0` to refresh its lock entry"
	if item.err == nil || !strings.HasSuffix(item.err.Error(), want) {
		t.Errorf("user-scope error = %v, want it to end with %q", item.err, want)
	}
	if got := templateOps(t.TempDir()).installCmd(); got != "putnami templates install" {
		t.Errorf("template install command = %q, want putnami templates install", got)
	}
}

func TestHoldPin(t *testing.T) {
	tests := []struct {
		name, request, pinned, served string
		want                          *heldPin
	}{
		{"an older build on latest", "latest", lockedCanary, olderBuild, &heldPin{channel: "latest", served: olderBuild, older: true}},
		{"an older build on an empty constraint", "", "1.2.0", "1.1.0", &heldPin{channel: "latest", served: "1.1.0", older: true}},
		{"an older build on *", "*", "1.2.0", "1.1.0", &heldPin{channel: "latest", served: "1.1.0", older: true}},
		{"an older build on a channel name", "canary", lockedCanary, olderBuild, &heldPin{channel: "canary", served: olderBuild, older: true}},
		{"a build that does not compare", "latest", "1.0.0", "nightly", &heldPin{channel: "latest", served: "nightly"}},
		{"a pin that does not compare", "latest", "nightly", "1.0.0", &heldPin{channel: "latest", served: "1.0.0"}},
		{"an answer without a version", "latest", "1.0.0", "0.0.0", &heldPin{channel: "latest", served: "0.0.0"}},
		{"a newer build", "latest", lockedCanary, newerBuild, nil},
		{"the same build", "latest", lockedCanary, lockedCanary, nil},
		{"no pin", "latest", "", olderBuild, nil},
		{"a 0.0.0 pin", "latest", "0.0.0", olderBuild, nil},
		{"an exact version", olderBuild, lockedCanary, olderBuild, nil},
		{"a range", "^1.0.0", "1.2.0", "1.1.0", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := holdPin(tc.request, tc.pinned, tc.served)
			if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
				t.Errorf("holdPin(%q, %q, %q) = %+v, want %+v", tc.request, tc.pinned, tc.served, got, tc.want)
			}
		})
	}
}

func TestMoveMark(t *testing.T) {
	tests := []struct{ old, next, want string }{
		{lockedCanary, newerBuild, "↑"},
		{lockedCanary, olderBuild, "↓"},
		{"1.0.0", "v1.0.0", "→"},
		{"1.0.0", "nightly", "→"},
		{"0.0.0", olderBuild, "→"},
	}
	for _, tc := range tests {
		if got := moveMark(tc.old, tc.next); got != tc.want {
			t.Errorf("moveMark(%q, %q) = %s, want %s", tc.old, tc.next, got, tc.want)
		}
	}
}

// A stable link that did not exist before the install is removed again, and
// one that cannot be pointed back is removed with the reason.
func TestLinkSnapshotRestoreRemovesTheLink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "ext")
	build := filepath.Join(dir, "ext@1.0.0")
	if err := os.Mkdir(build, 0o755); err != nil {
		t.Fatal(err)
	}
	ops := artifactOps{stableLink: func(string) string { return link }}

	none := snapshotLink(ops, "ext")
	if err := dirlink.Create(build, link); err != nil {
		t.Fatal(err)
	}
	if err := none.restore(); err != nil {
		t.Fatalf("restore without a prior link = %v, want nil", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("a link that did not exist before is still there: %v", err)
	}

	if runtime.GOOS == "windows" {
		// dirlink.Replace replaces an empty directory on Windows.
		return
	}
	if err := dirlink.Create(build, link); err != nil {
		t.Fatal(err)
	}
	linked := snapshotLink(ops, "ext")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := linked.restore(); err == nil {
		t.Error("restore over a directory = nil, want the reason the link could not be pointed back")
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("a link that could not be pointed back is still there: %v", err)
	}
}
