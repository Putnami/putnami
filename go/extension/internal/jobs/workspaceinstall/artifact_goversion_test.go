package workspaceinstall

import (
	"debug/buildinfo"
	"fmt"
	"go/version"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/pkgmeta"
)

// assertArtifactServesUpToItsMinor pins the rule workspace-install shares with
// lint (toolchain.ToolServesLocalGo): a release builds its tools with a newer
// Go than a workspace may pin, and that build serves the workspace, so the job
// restores it instead of compiling the tool; a local Go newer than the build
// does not match. The older-Go case failed while the rule required the same
// minor: every fresh workspace on the default Go compiled both tools, about
// 4.8 minutes on a 4-vCPU Windows host.
func assertArtifactServesUpToItsMinor(t *testing.T, w *install, i *installJob, artifact, pin string) {
	t.Helper()
	info, err := buildinfo.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(info.GoVersion)
	minor, err := strconv.Atoi(strings.TrimPrefix(version.Lang(fields[0]), "go1."))
	if err != nil {
		t.Fatalf("no minor in the artifact's Go version %q", info.GoVersion)
	}
	resolved := w.GoBinary
	defer func() { w.GoBinary = resolved }()
	for _, tc := range []struct {
		local string
		want  bool
	}{
		{fmt.Sprintf("1.%d.7", minor-1), true},
		{fmt.Sprintf("1.%d.0", minor), true},
		{fmt.Sprintf("1.%d.0", minor+1), false},
	} {
		w.GoBinary = i.fakes.GoRelease(t, filepath.Join(t.TempDir(), pkgmeta.ExecutableName(runtime.GOOS, "go")), tc.local)
		if got := w.artifactMatches(artifact, pin); got != tc.want {
			t.Errorf("artifactMatches(built with %s) on a local Go %s = %v, want %v", info.GoVersion, tc.local, got, tc.want)
		}
	}
}
