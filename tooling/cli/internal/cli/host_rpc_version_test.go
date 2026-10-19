package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	hostrpcproto "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/protocol/features/spectest"
	supportproto "go.putnami.dev/protocol/support"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/versioncmd"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// hostRPCVersionLines is the requirement of the version.lines method.
const hostRPCVersionLines = "host-rpc-version-lines"

// hostRPCGit runs git in dir with no global or system configuration and
// returns its trimmed output.
func hostRPCGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// hostRPCVersionCommit writes one file and commits it with message, and
// returns the commit's full ID.
func hostRPCVersionCommit(t *testing.T, root, file string, message ...string) string {
	t.Helper()
	writeParityFile(t, root, file, file+"\n")
	args := []string{"commit", "-q"}
	for _, paragraph := range message {
		args = append(args, "-m", paragraph)
	}
	hostRPCGit(t, root, "add", "-A")
	hostRPCGit(t, root, args...)
	return hostRPCGit(t, root, "rev-parse", "HEAD")
}

// hostRPCVersionRepo is a committed workspace with two version lines, go and
// tooling. The first commit is tooling's release tooling/v0.1.0. After it come
// a fix to tooling, a fix outside both lines, and a breaking feature to
// tooling with a body. The go line has no release. It returns the root and
// the IDs of the four commits, oldest first.
func hostRPCVersionRepo(t *testing.T) (string, []string) {
	t.Helper()
	root := t.TempDir()
	writeParityFile(t, root, wsproto.WorkspaceConfigFilename, `{"name":"lines","includes":["go","tooling"]}`)
	writeParityFile(t, root, "go/"+wsproto.ConfigFilename, `{"line":{},"includes":["lib"]}`)
	writeParityFile(t, root, "go/lib/"+wsproto.ConfigFilename, `{"name":"lib"}`)
	writeParityFile(t, root, "tooling/"+wsproto.ConfigFilename, `{"line":{},"includes":["cli"]}`)
	writeParityFile(t, root, "tooling/cli/"+wsproto.ConfigFilename, `{"name":"cli"}`)
	writeParityFile(t, root, ".gitignore", ".putnami/\n")
	for _, args := range [][]string{
		{"init", "-q", "--initial-branch=main"},
		{"config", "user.email", "lines@example.test"},
		{"config", "user.name", "Lines Fixture"},
		{"config", "commit.gpgsign", "false"},
		{"config", "tag.gpgsign", "false"},
		{"config", "maintenance.auto", "false"},
		{"config", "gc.auto", "0"},
	} {
		hostRPCGit(t, root, args...)
	}
	commits := []string{hostRPCVersionCommit(t, root, "go/lib/lib.go", "feat: the workspace")}
	hostRPCGit(t, root, "tag", "tooling/v0.1.0")
	commits = append(commits,
		hostRPCVersionCommit(t, root, "tooling/cli/a.go", "fix(cli): repair a"),
		hostRPCVersionCommit(t, root, "README.md", "fix: the readme"),
		hostRPCVersionCommit(t, root, "tooling/cli/b.go", "feat(cli)!: break b", "The body says why."))
	return root, commits
}

// hostRPCLineSpec is the line of root whose scope path is scope, as
// `version get` reads it.
func hostRPCLineSpec(t *testing.T, root, scope string) git.LineSpec {
	t.Helper()
	specs, err := versioncmd.LineSpecs(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range specs {
		if spec.ScopePath == scope {
			return spec
		}
	}
	t.Fatalf("no line %q in %+v", scope, specs)
	return git.LineSpec{}
}

func TestHostRPC_VersionLinesAnswersWhatTheCoreComputesForEachLine(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, hostRPCFeature, hostRPCVersionLines, "version-lines-answers-what-the-core-computes-for-each-line")
	spectest.Proves(t, hostRPCFeature, hostRPCVersionLines, "version-lines-selects-the-line-its-scope-names")
	spectest.Proves(t, hostRPCFeature, hostRPCVersionLines, "version-lines-refuses-a-scope-that-names-no-line")
	root, commits := hostRPCVersionRepo(t)
	client := dialHostRPC(t, hostRPCBinding{root: root, config: wsproto.Load(root)})

	answer, err := client.VersionLines(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if answer.SourceRevision != commits[3] || answer.RecordedGraph != nil {
		t.Errorf("source revision %q, recorded graph %+v; want HEAD %s and no recorded graph", answer.SourceRevision, answer.RecordedGraph, commits[3])
	}
	if len(answer.Lines) != 2 || answer.Lines[0].Line != "go" || answer.Lines[1].Line != "tooling" {
		t.Fatalf("lines %+v, want go then tooling", answer.Lines)
	}
	for _, tc := range []struct {
		line       hostrpcproto.VersionLine
		base       string
		release    hostrpcproto.LineRelease
		commits    []string
		subjects   []string
		newestBody string
	}{
		{answer.Lines[0], "0.0.0-", hostrpcproto.LineRelease{Version: "0.0.1", Tag: "go/v0.0.1"},
			[]string{commits[0]}, []string{"feat: the workspace"}, ""},
		{answer.Lines[1], "0.2.0-", hostrpcproto.LineRelease{Version: "0.2.0", Tag: "tooling/v0.2.0"},
			[]string{commits[3], commits[1]}, []string{"feat(cli)!: break b", "fix(cli): repair a"}, "The body says why."},
	} {
		line := tc.line
		info, err := git.GetVersionInfo(root, hostRPCLineSpec(t, root, line.Line))
		if err != nil {
			t.Fatal(err)
		}
		if line.Version != info.Full || !strings.HasPrefix(line.Version, tc.base) || line.Tagged || line.Tag != "" {
			t.Errorf("line %s: version %q, tagged %t, tag %q; want %q, which `version get` prints, untagged", line.Line, line.Version, line.Tagged, line.Tag, info.Full)
		}
		if line.TagPattern != line.Line+"/v{version}" || line.Release == nil || *line.Release != tc.release {
			t.Errorf("line %s: tag pattern %q, release %+v; want %s/v{version} and %+v", line.Line, line.TagPattern, line.Release, line.Line, tc.release)
		}
		var shas, subjects []string
		for _, commit := range line.Commits {
			shas = append(shas, commit.SHA)
			subjects = append(subjects, commit.Subject)
		}
		if !slices.Equal(shas, tc.commits) || !slices.Equal(subjects, tc.subjects) || line.Commits[0].Body != tc.newestBody {
			t.Errorf("line %s: commits %+v, want %v (%v), newest first, with the body %q", line.Line, line.Commits, tc.commits, tc.subjects, tc.newestBody)
		}
	}

	scoped, err := client.VersionLines(t.Context(), "/tooling/")
	if err != nil || len(scoped.Lines) != 1 || !slices.Equal(scoped.Lines[0].Commits, answer.Lines[1].Commits) ||
		scoped.Lines[0].Version != answer.Lines[1].Version {
		t.Fatalf("VersionLines(/tooling/) = %+v, %v, want the tooling line alone", scoped, err)
	}
	var refusal *hostrpcproto.Error
	for _, tc := range []struct{ scope, named string }{{"tooling/cli", "tooling/cli"}, {"/nope/", "nope"}} {
		_, err = client.VersionLines(t.Context(), tc.scope)
		if !errors.As(err, &refusal) || refusal.Code != hostrpcproto.CodeInvalidParams ||
			refusal.Message != `no version line "`+tc.named+`"; this workspace declares go, tooling` {
			t.Fatalf("VersionLines(%q) = %v, want %d naming %q, as `version get --scope` does, and listing the lines",
				tc.scope, err, hostrpcproto.CodeInvalidParams, tc.named)
		}
	}

	hostRPCGit(t, root, "tag", "go/v0.0.1")
	tagged, err := client.VersionLines(t.Context(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if line := tagged.Lines[0]; line.Version != "0.0.1" || !line.Tagged || line.Tag != "go/v0.0.1" || line.Release != nil || len(line.Commits) != 0 {
		t.Errorf("the tagged line = %+v, want version 0.0.1 tagged go/v0.0.1, no release and no commit", line)
	}
}

func TestHostRPC_VersionLinesNamesTheRecordedGraphAfterAFailedRefresh(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, hostRPCFeature, hostRPCVersionLines, "version-lines-names-the-recorded-graph-after-a-failed-refresh")
	root := versionGraphRepo(t, true)
	binding := hostRPCBinding{root: root, config: wsproto.Load(root)}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	// The binding is called directly, not through the client: only a canceled context fails the refresh.
	if _, err := binding.versionLines(canceled, hostrpcproto.VersionLinesParams{}); err == nil ||
		!strings.Contains(err.Error(), supportproto.CatalogFilename) {
		t.Fatalf("a failed refresh without a recorded graph = %v, want a refusal naming %s", err, supportproto.CatalogFilename)
	}
	fresh, err := binding.versionLines(context.Background(), hostrpcproto.VersionLinesParams{})
	if err != nil || fresh.RecordedGraph != nil || len(fresh.Lines) != 1 {
		t.Fatalf("a refreshed graph = %+v, %v, want one line and no recorded graph", fresh, err)
	}
	recorded, err := binding.versionLines(canceled, hostrpcproto.VersionLinesParams{})
	if err != nil {
		t.Fatalf("a failed refresh with a recorded graph = %v, want the recorded answer", err)
	}
	view := workspace.RecordedIndexView(root, time.Now())
	if recorded.RecordedGraph == nil || recorded.RecordedGraph.ObservedAt == "" ||
		recorded.RecordedGraph.ObservedAt != view.ObservedAt || recorded.RecordedGraph.RefreshError == "" {
		t.Fatalf("recorded graph %+v, want the view observed at %s and the refresh's error", recorded.RecordedGraph, view.ObservedAt)
	}
	if recorded.Lines[0].Version != fresh.Lines[0].Version {
		t.Errorf("version %q from the recorded graph, want %q", recorded.Lines[0].Version, fresh.Lines[0].Version)
	}
}

func TestHostRPC_VersionLinesReportsAnInvalidConfigurationWithItsOwnCode(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, hostRPCFeature, hostRPCVersionLines, "version-lines-reports-an-invalid-configuration-with-its-own-code")
	root := versionGraphRepo(t, false)
	writeParityFile(t, root, supportproto.CatalogFilename, `{"protocolVersion": 1, "entries": [{"id": "@putnami/cli"}]}`)
	_, want := versioncmd.LineSpecs(root)
	if !errors.Is(want, cmderr.ErrInvalidConfig) || protocolcli.ExitCodeForError(want) != protocolcli.ExitUsage {
		t.Fatalf("the core's refusal of an invalid catalog = %v, want an invalid configuration that exits %d", want, protocolcli.ExitUsage)
	}
	_, err := dialHostRPC(t, hostRPCBinding{root: root, config: wsproto.Load(root)}).VersionLines(t.Context(), "")
	var refusal *hostrpcproto.Error
	if !errors.As(err, &refusal) || refusal.Code != hostrpcproto.CodeInvalidConfig || refusal.Message != want.Error() {
		t.Fatalf("version.lines with an invalid catalog = %v, want %d with the message %q", err, hostrpcproto.CodeInvalidConfig, want)
	}

	// The environment is process-wide, so a child of this test binary holds the malformed binding.
	repo, _ := hostRPCVersionRepo(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestHostRPCVersionLinesHelperProcess$")
	cmd.Env = append(os.Environ(), hostRPCVersionRootEnv+"="+repo, git.SourceRevisionEnv+"=NOT-A-COMMIT")
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("the helper process failed: %v\n%s", err, stdout)
	}
	report := hostRPCReport(string(stdout))
	wantMessage := "version tag: " + git.SourceRevisionEnv + `="NOT-A-COMMIT" is not a full lowercase hex commit id (40 or 64 characters)`
	if report["code"] != strconv.Itoa(hostrpcproto.CodeInvalidConfig) || report["message"] != wantMessage {
		t.Fatalf("version.lines with a malformed %s = %v, want %d with the message %q", git.SourceRevisionEnv, report, hostrpcproto.CodeInvalidConfig, wantMessage)
	}
}

// hostRPCVersionRootEnv names the workspace of TestHostRPCVersionLinesHelperProcess.
const hostRPCVersionRootEnv = "PUTNAMI_CLI_HOSTRPC_VERSION_ROOT"

// TestHostRPCVersionLinesHelperProcess is the child of the test above: it asks
// version.lines of the workspace in hostRPCVersionRootEnv and prints the code
// and the message of the refusal, one key=value line each.
func TestHostRPCVersionLinesHelperProcess(t *testing.T) {
	t.Parallel()
	root := os.Getenv(hostRPCVersionRootEnv)
	if root == "" {
		t.Skip("helper process: not invoked as a version.lines client")
	}
	_, err := dialHostRPC(t, hostRPCBinding{root: root, config: wsproto.Load(root)}).VersionLines(t.Context(), "")
	var refusal *hostrpcproto.Error
	if !errors.As(err, &refusal) {
		iox.Fprintf(os.Stdout, "error=%v\n", err)
		return
	}
	iox.Fprintf(os.Stdout, "code=%d\nmessage=%s\n", refusal.Code, refusal.Message)
}

func TestHostRPC_VersionLinesIsUnavailableOutsideAWorkspace(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, hostRPCFeature, hostRPCVersionLines, "version-lines-is-unavailable-outside-a-workspace")
	_, err := dialHostRPC(t, hostRPCBinding{}).VersionLines(t.Context(), "")
	var refusal *hostrpcproto.Error
	if !errors.As(err, &refusal) || refusal.Code != hostrpcproto.CodeUnavailable ||
		refusal.Message != "this command runs outside any workspace, so there is no version line" {
		t.Fatalf("version.lines outside a workspace = %v, want %d", err, hostrpcproto.CodeUnavailable)
	}
}

func TestHostRPC_VersionLinesReportsAGitFailureAsTheHosts(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, hostRPCFeature, hostRPCVersionLines, "version-lines-reports-a-git-failure-as-the-hosts")
	root, _ := hostRPCVersionRepo(t)
	shallow := filepath.Join(t.TempDir(), "shallow")
	hostRPCGit(t, root, "clone", "-q", "--depth=1", "file://"+filepath.ToSlash(root), shallow)
	_, err := dialHostRPC(t, hostRPCBinding{root: shallow, config: wsproto.Load(shallow)}).VersionLines(t.Context(), "")
	var refusal *hostrpcproto.Error
	if !errors.As(err, &refusal) || refusal.Code != hostrpcproto.CodeInternalError ||
		!strings.Contains(refusal.Message, "git fetch --unshallow --tags") {
		t.Fatalf("version.lines in a shallow clone = %v, want %d naming the fetch to run", err, hostrpcproto.CodeInternalError)
	}
}
