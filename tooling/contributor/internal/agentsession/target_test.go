package agentsession

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestTheRepositoryComesFromOriginWithoutHostOrCredential(t *testing.T) {
	spectest.Proves(t, sessionFeature, targetRequirement, "the-repository-comes-from-origin-without-host-or-credential")
	for remote, want := range map[string]string{
		"git@example.com:Acme/Widgets.git":                     "acme/widgets",
		"https://deploy:secret@example.com/Acme/Widgets.git":   "acme/widgets",
		"ssh://git@example.com:22/acme/widgets":                "acme/widgets",
		"https://example.com/group/sub/acme/widgets/?ref=x#y":  "acme/widgets",
		"https://example.com/acme/widgets.git/":                "acme/widgets",
		"  git@example.com:acme/widgets\n":                     "acme/widgets",
		"/srv/git/acme/widgets.git":                            "",
		"./widgets":                                            "",
		"../acme/widgets":                                      "",
		"~/repos/acme/widgets":                                 "",
		"file:///srv/acme/widgets.git":                         "",
		"FILE:///srv/acme/widgets.git":                         "",
		"widgets":                                              "",
		"srv/acme/widgets":                                     "",
		"https://example.com/widgets":                          "",
		"example.com:widgets":                                  "",
		"https://example.com/acme/..":                          "",
		"":                                                     "",
		"https://example.com/acme/wid gets":                    "",
		"git@example.com:acme/" + string(rune(0x7f)) + "x.git": "",
	} {
		if got := repositoryFromRemote(remote); got != want {
			t.Errorf("repositoryFromRemote(%q) = %q, want %q", remote, got, want)
		}
	}
}

func TestTheMissionKeyFollowsTheDeclaredIssue(t *testing.T) {
	spectest.Proves(t, sessionFeature, targetRequirement, "attach-declares-the-mission-of-the-branch")
	dir, root := t.TempDir(), "/work/repo"
	if key := sessionMissionKey(dir, root, "feat/x"); key != "branch:feat/x" {
		t.Errorf("key %q", key)
	}
	if key := sessionMissionKey(dir, root, ""); key != "" {
		t.Errorf("a detached HEAD answered %q", key)
	}
	if err := declareMissionIssue(dir, root, "feat/x", 596, sessionEpoch); err != nil {
		t.Fatal(err)
	}
	if err := declareMissionIssue(dir, root, "", 7, sessionEpoch); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ root, branch, want string }{
		{root, "feat/x", "issue-596"},
		{root, "feat/y", "branch:feat/y"},
		{"/work/other", "feat/x", "branch:feat/x"},
		{root, "", "issue-7"},
	} {
		if key := sessionMissionKey(dir, tc.root, tc.branch); key != tc.want {
			t.Errorf("%s %s: key %q, want %q", tc.root, tc.branch, key, tc.want)
		}
	}
	if err := declareMissionIssue(dir, root, "feat/x", 0, sessionEpoch); err == nil {
		t.Error("issue 0 was declared")
	}
	// A declaration that names another checkout is not this one's.
	path := missionDeclarationPath(dir, root, "feat/z")
	foreign, err := json.Marshal(missionDeclaration{Root: "/elsewhere", Branch: "feat/z", Issue: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if key := sessionMissionKey(dir, root, "feat/z"); key != "branch:feat/z" {
		t.Errorf("a foreign declaration answered %q", key)
	}
	if title := missionTitle("issue-7"); title != "Issue #7" {
		t.Errorf("title %q", title)
	}
	if title := missionTitle("branch:feat/x"); title != "feat/x" {
		t.Errorf("title %q", title)
	}
}

func TestAttachDeclaresTheMissionOfTheBranch(t *testing.T) {
	spectest.Proves(t, sessionFeature, targetRequirement, "attach-declares-the-mission-of-the-branch")
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	var stdout, stderr bytes.Buffer
	if code := Attach(context.Background(), h.env, []string{"--issue", "#7"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if want := "The sessions of this checkout on branch feat/session-hooks now report to mission issue-7.\n"; stdout.String() != want {
		t.Errorf("stdout %q, want %q", stdout.String(), want)
	}
	if calls := h.provider.recordedCalls(); len(calls) != 2 {
		t.Errorf("attach called the provider: %v", calls)
	}
	// A session that runs already moves at its next sync, and a new one
	// starts there.
	h.hook(claude, "stop", claudeSession, nil)
	if sync := h.provider.lastSync(t); sync.Mission.Key != "issue-7" || sync.Mission.Title != "Issue #7" {
		t.Errorf("mission %+v", sync.Mission)
	}
	h.hook(codex, "session-start", codexSession, nil)
	if sync := h.provider.lastSync(t); sync.Mission.Key != "issue-7" || sync.Session.Engine != codex {
		t.Errorf("mission %+v", sync.Mission)
	}

	// A detached HEAD declares the issue for the checkout without a branch.
	h = newHarness(t)
	h.branch = ""
	stdout.Reset()
	if code := Attach(context.Background(), h.env, []string{"--issue=12"}, &stdout, &stderr); code != 0 || stdout.String() != "The sessions of this checkout on a detached HEAD now report to mission issue-12.\n" {
		t.Errorf("exit %d, stdout %q", code, stdout.String())
	}
}

func TestAttachRefusesAUsageError(t *testing.T) {
	spectest.Proves(t, sessionFeature, targetRequirement, "attach-declares-the-mission-of-the-branch")
	h := newHarness(t)
	for _, args := range [][]string{nil, {"--issue"}, {"--issue", "0"}, {"--issue", "x"}, {"--issue", "-3"}, {"--other"}, {"--issue", "1", "--issue", "2"}, {"--issue", "1000000001"}} {
		var stdout, stderr bytes.Buffer
		if code := Attach(context.Background(), h.env, args, &stdout, &stderr); code != 2 || stdout.Len() != 0 || !bytes.HasPrefix(stderr.Bytes(), []byte("agent attach: ")) {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", args, code, stdout.String(), stderr.String())
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(h.env.sessionsDir(), declaredDir)); len(entries) != 0 {
		t.Errorf("a usage error declared %d issues", len(entries))
	}
	// A store it cannot write is a failure.
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.env.Home = blocked
	var stdout, stderr bytes.Buffer
	if code := Attach(context.Background(), h.env, []string{"--issue", "5"}, &stdout, &stderr); code != 1 {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestTheWorkspaceRootIsTheNearestManifest(t *testing.T) {
	spectest.Proves(t, sessionFeature, targetRequirement, "attach-declares-the-mission-of-the-branch")
	root := canonicalRoot(t.TempDir())
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, workspaceManifest), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := sessionWorkspaceRoot(deep, "/fallback"); got != root {
		t.Errorf("root %q, want %q", got, root)
	}
	for _, cwd := range []string{"", "relative/dir", filepath.Join(t.TempDir(), "none")} {
		if got := sessionWorkspaceRoot(cwd, "/fallback"); got != "/fallback" {
			t.Errorf("%q: root %q", cwd, got)
		}
	}
	if canonicalRoot(" ") != "" {
		t.Error("a blank root resolved")
	}
}
