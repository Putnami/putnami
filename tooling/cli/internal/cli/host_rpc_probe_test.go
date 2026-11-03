package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const hostRPCWorkspaceProbe = "host-rpc-workspace-probe"

// probeFixtureExtension is a user-scope extension whose workspace probe is a
// shell script: it marks every directory that holds marker, and its probe
// prints answer whatever it is asked. An empty answer makes the probe exit 3.
func probeFixtureExtension(t *testing.T, name, marker, answer string) *extension.ExtensionDescription {
	t.Helper()
	script := "#!/bin/sh\ncat >/dev/null\n"
	if answer == "" {
		script += "echo 'the probe broke' >&2\nexit 3\n"
	} else {
		script += "cat <<'EOF'\n" + answer + "\nEOF\n"
	}
	executable := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return &extension.ExtensionDescription{
		Name:              name,
		Path:              filepath.Dir(executable),
		Workspace:         &extproto.WorkspaceAdapter{Markers: []string{marker}, Inputs: []string{marker}},
		RuntimeExecutable: executable,
	}
}

// probeCallerDir writes a directory that is no workspace: a Node package that
// depends on a Go module, and a README.
func probeCallerDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeParityFile(t, dir, "apps/web/package.json", `{"name":"web"}`)
	writeParityFile(t, dir, "libs/core/go.mod", "module example.com/core\n")
	writeParityFile(t, dir, "README.md", "x\n")
	return dir
}

// treeState lists every entry under dir with its mode, size and modification
// time, so two states differ when anything was created, removed or written.
func treeState(t *testing.T, dir string) []string {
	t.Helper()
	var entries []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		entries = append(entries, strings.Join([]string{path, info.Mode().String(), info.ModTime().String(), strconv.FormatInt(info.Size(), 10)}, " "))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func userScopeProbeBinding(t *testing.T, callerDir string, extensions ...*extension.ExtensionDescription) hostRPCBinding {
	t.Helper()
	userScope := workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: userScopeWorkspaceName}, nil)
	return hostRPCBinding{extensions: extensions, callerDir: callerDir, userScope: userScope}
}

const (
	nodeProbeAnswer = `{"version":1,"extension":"@acme/node","projects":[` +
		`{"path":"apps/web","sourceName":"web","sourceFile":"apps/web/package.json","dependencies":["libs/core"]}]}`
	goProbeAnswer = `{"version":1,"extension":"@acme/go","projects":[` +
		`{"path":"libs/core","sourceName":"example.com/core","sourceFile":"libs/core/go.mod"}]}`
)

func TestHostRPC_WorkspaceProbeAnswersTheUserScopeProbesOutsideAWorkspace(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, hostRPCFeature, hostRPCWorkspaceProbe, "workspace-probe-answers-the-user-scope-probes-outside-a-workspace")
	spectest.Proves(t, hostRPCFeature, hostRPCWorkspaceProbe, "workspace-probe-writes-nothing-in-the-callers-directory")
	callerDir := probeCallerDir(t)
	before := treeState(t, callerDir)
	client := dialHostRPC(t, userScopeProbeBinding(t, callerDir,
		probeFixtureExtension(t, "@acme/node", "package.json", nodeProbeAnswer),
		probeFixtureExtension(t, "@acme/go", "go.mod", goProbeAnswer),
	))

	result, err := client.WorkspaceProbe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Root != callerDir || len(result.Diagnostics) != 0 {
		t.Fatalf("WorkspaceProbe = %+v, want the caller's directory as root and no diagnostic", result)
	}
	paths := make([]string, 0, len(result.Projects))
	for _, project := range result.Projects {
		paths = append(paths, project.Path)
	}
	if !slices.Equal(paths, []string{"apps/web", "libs/core"}) {
		t.Fatalf("projects %v, want apps/web and libs/core", paths)
	}
	if web := result.Projects[0]; web.SourceName != "web" || !slices.Equal(web.Dependencies, []string{"libs/core"}) {
		t.Errorf("apps/web = %+v, want the name and the edge its probe declares", web)
	}
	if core := result.Projects[1]; core.SourceName != "example.com/core" || core.SourceFile != "libs/core/go.mod" {
		t.Errorf("libs/core = %+v, want the module its probe declares", core)
	}
	if after := treeState(t, callerDir); !slices.Equal(before, after) {
		t.Fatalf("the caller's directory changed:\nbefore %v\nafter  %v", before, after)
	}
}

func TestHostRPC_WorkspaceProbeReportsAFailedProbeAsADiagnostic(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, hostRPCFeature, hostRPCWorkspaceProbe, "workspace-probe-reports-a-failed-probe-as-a-diagnostic")
	callerDir := probeCallerDir(t)
	client := dialHostRPC(t, userScopeProbeBinding(t, callerDir,
		probeFixtureExtension(t, "@acme/node", "package.json", nodeProbeAnswer),
		probeFixtureExtension(t, "@acme/go", "go.mod", ""),
		probeFixtureExtension(t, "@acme/liar", "README.md", `{"version":1,"extension":"@acme/other"}`),
	))

	result, err := client.WorkspaceProbe(t.Context())
	if err != nil {
		t.Fatalf("WorkspaceProbe = %v, want an answer that reports the failed probes", err)
	}
	if len(result.Projects) != 1 || result.Projects[0].Path != "apps/web" {
		t.Errorf("projects %+v, want the project of the probe that answered", result.Projects)
	}
	failed := map[string]diag.Diagnostic{}
	for _, d := range result.Diagnostics {
		if d.Severity == diag.Error {
			failed[d.Field] = d
		}
	}
	if broke := failed["@acme/go"]; broke.Code != string(wsproto.ProbeFailureTransport) || !strings.Contains(broke.Message, "the probe broke") {
		t.Errorf("diagnostics %+v, want the failed probe of @acme/go with its stderr", result.Diagnostics)
	}
	if misrouted := failed["@acme/liar"]; misrouted.Code != string(wsproto.ProbeFailureTransport) {
		t.Errorf("diagnostics %+v, want the misrouted answer of @acme/liar", result.Diagnostics)
	}
}

func TestHostRPC_WorkspaceProbeAnswersTheWorkspaceViewInsideAWorkspace(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, hostRPCFeature, hostRPCWorkspaceProbe, "workspace-probe-answers-the-workspace-view-inside-a-workspace")
	root := hostRPCOwnersWorkspace(t)
	client := dialHostRPC(t, hostRPCBinding{root: root, config: wsproto.Load(root)})

	result, err := client.WorkspaceProbe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Root != root {
		t.Fatalf("root %q, want the workspace root %q", result.Root, root)
	}
	watched := map[string][]string{}
	for _, project := range result.Projects {
		watched[project.Path] = project.WatchedFiles
	}
	for _, path := range []string{"packages/claimant", "packages/outer"} {
		if !slices.Equal(watched[path], []string{"go.work"}) {
			t.Errorf("projects %+v, want %s with the go.work the recorded probe answer watches", result.Projects, path)
		}
	}
}

func TestHostRPC_WorkspaceProbeIsUnavailableWithoutADirectory(t *testing.T) {
	t.Parallel()
	_, err := dialHostRPC(t, hostRPCBinding{}).WorkspaceProbe(t.Context())
	if err == nil || !strings.Contains(err.Error(), "neither a workspace nor a caller directory") {
		t.Fatalf("workspace.probe without a directory = %v, want unavailable", err)
	}
}
