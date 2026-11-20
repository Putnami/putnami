package agentsession

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const (
	identityRequirement  = "identity-refresh"
	noMissionRequirement = "no-mission-reason"
)

// attach runs `agent attach` with args and fails the test unless it exits 0.
func (h *harness) attach(args ...string) string {
	h.t.Helper()
	var stdout, stderr bytes.Buffer
	if code := Attach(context.Background(), h.env, args, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		h.t.Fatalf("attach exited %d with stderr %q", code, stderr.String())
	}
	return stdout.String()
}

// A hosted session starts on a detached HEAD, checks out a branch and
// declares its issue: agent stage reports it to that mission within the
// turn, and so does the Stop that ends the turn.
func TestASessionThatChecksOutABranchReportsToItsDeclaredMissionAtItsStage(t *testing.T) {
	spectest.Proves(t, sessionFeature, identityRequirement, "a-session-that-checks-out-a-branch-reports-to-its-declared-mission")
	h := newHarness(t)
	h.branch = ""
	h.hook(claude, "session-start", claudeSession, nil)
	if syncs := h.provider.recordedSyncs(); len(syncs) != 0 {
		t.Fatalf("a detached HEAD synced %d times", len(syncs))
	}
	h.branch = sessionBranch
	if out := h.attach("--issue", "4883"); out != "The sessions of this checkout on branch "+sessionBranch+" now report to mission issue-4883.\n" {
		t.Fatalf("attach printed %q", out)
	}
	h.environ["CLAUDE_CODE_SESSION_ID"] = claudeSession
	code, stdout, stderr := h.stage("declare")
	if code != 0 || stderr != "" || stdout != "The session is at stage declare, and its mission issue-4883 knows it.\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	sync := h.provider.lastSync(t)
	if sync.Mission.Key != "issue-4883" || sync.Mission.Repository != sessionRepo || sync.Branch != sessionBranch {
		t.Errorf("sync mission %+v on branch %q", sync.Mission, sync.Branch)
	}
	h.hook(claude, "stop", claudeSession, nil)
	if sync := h.provider.lastSync(t); sync.Mission.Key != "issue-4883" || sync.Branch != sessionBranch {
		t.Errorf("stop sync mission %+v on branch %q", sync.Mission, sync.Branch)
	}
}

// Without agent stage, the Stop that ends the turn rereads the branch and
// reports to the declared mission.
func TestAStopRereadsTheBranchOfTheCheckout(t *testing.T) {
	spectest.Proves(t, sessionFeature, identityRequirement, "a-session-that-checks-out-a-branch-reports-to-its-declared-mission")
	h := newHarness(t)
	h.branch = ""
	h.hook(claude, "session-start", claudeSession, nil)
	h.branch = sessionBranch
	h.attach("--issue", "4883")
	h.hook(claude, "stop", claudeSession, nil)
	if sync := h.provider.lastSync(t); sync.Mission.Key != "issue-4883" || sync.Branch != sessionBranch {
		t.Errorf("stop sync mission %+v on branch %q", sync.Mission, sync.Branch)
	}
	// A branch switch without a declaration reports to that branch.
	h.branch = "feat/other"
	h.hook(claude, "stop", claudeSession, nil)
	if sync := h.provider.lastSync(t); sync.Mission.Key != "branch:feat/other" || sync.Branch != "feat/other" {
		t.Errorf("stop sync mission %+v on branch %q", sync.Mission, sync.Branch)
	}
}

// A git that fails at Stop or at agent stage keeps the identity the session
// read last; a directory the agent moved to keeps the session's checkout.
func TestAGitFailureKeepsTheIdentityReadLast(t *testing.T) {
	spectest.Proves(t, sessionFeature, identityRequirement, "a-session-that-checks-out-a-branch-reports-to-its-declared-mission")
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	working := h.env.Git
	h.env.Git = func(context.Context, string, ...string) (string, error) {
		return "", context.DeadlineExceeded
	}
	h.hook(claude, "stop", claudeSession, nil)
	if sync := h.provider.lastSync(t); sync.Mission.Key != sessionBranchKey || sync.Branch != sessionBranch {
		t.Errorf("stop sync mission %+v on branch %q", sync.Mission, sync.Branch)
	}
	if !logSays(h.hookLog(), "context deadline exceeded") {
		t.Errorf("hook.log %v", h.hookLog())
	}
	h.environ["CLAUDE_CODE_SESSION_ID"] = claudeSession
	if code, stdout, _ := h.stage("code"); code != 0 || !strings.Contains(stdout, "its mission "+sessionBranchKey+" knows it") {
		t.Errorf("exit %d, stdout %q", code, stdout)
	}
	h.env.Git = working
	// Another workspace the agent moved to.
	elsewhere := canonicalRoot(t.TempDir())
	if err := os.MkdirAll(filepath.Join(elsewhere, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(elsewhere, workspaceManifest), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := sessionWorkspaceRoot(filepath.Join(elsewhere, "sub"), h.root); got != elsewhere {
		t.Fatalf("the payload names workspace %s, want %s", got, elsewhere)
	}
	var dirs []string
	h.env.Git = func(ctx context.Context, dir string, args ...string) (string, error) {
		dirs = append(dirs, dir)
		return working(ctx, dir, args...)
	}
	h.hook(claude, "stop", claudeSession, func(payload map[string]any) { payload["cwd"] = filepath.Join(elsewhere, "sub") })
	if len(dirs) == 0 {
		t.Error("the stop read no git")
	}
	if sync := h.provider.lastSync(t); sync.Mission.Repository != sessionRepo || sync.Branch != sessionBranch {
		t.Errorf("stop sync mission %+v on branch %q", sync.Mission, sync.Branch)
	}
	for _, dir := range dirs {
		if dir != h.root {
			t.Errorf("the stop read git in %s, not the session's checkout %s", dir, h.root)
		}
	}
	if cache := h.cache(claude, claudeSession); cache.Root != h.root {
		t.Errorf("root %q, want %q", cache.Root, h.root)
	}
}

// A session that reports to no mission logs why once, until the reason
// changes, and agent stage prints the same reason.
func TestASessionWithoutAMissionSaysWhyOnce(t *testing.T) {
	spectest.Proves(t, sessionFeature, noMissionRequirement, "a-session-without-a-mission-says-why-once")
	detached := "the session reports to no mission: the checkout is on a detached HEAD and putnami agent attach --issue <n> declared no issue for it"
	h := newHarness(t)
	h.branch = ""
	h.hook(claude, "session-start", claudeSession, nil)
	h.hook(claude, "stop", claudeSession, nil)
	h.hook(claude, "stop", claudeSession, nil)
	if lines := h.hookLog(); len(lines) != 1 || lines[0]["error"] != detached || lines[0]["event"] != hookEventSessionStart || lines[0]["root"] != h.root {
		t.Fatalf("hook.log %v", lines)
	}
	h.environ["CLAUDE_CODE_SESSION_ID"] = claudeSession
	code, stdout, stderr := h.stage("talk")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "It reports to no mission yet (the checkout is on a detached HEAD and putnami agent attach --issue <n> declared no issue for it)") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	// Another reason is logged once more.
	h.branch, h.origin = sessionBranch, "/srv/git/widgets.git"
	h.hook(claude, "stop", claudeSession, nil)
	h.hook(claude, "stop", claudeSession, nil)
	lines := h.hookLog()
	if len(lines) != 2 || lines[1]["error"] != "the session reports to no mission: the origin remote of the checkout names no owner/name repository" {
		t.Fatalf("hook.log %v", lines)
	}
	// A session that reports again, then loses its mission, logs again.
	h.origin = sessionOrigin
	h.hook(claude, "stop", claudeSession, nil)
	if sync := h.provider.lastSync(t); sync.Mission.Key != sessionBranchKey {
		t.Errorf("mission %+v", sync.Mission)
	}
	h.origin = ""
	h.hook(claude, "stop", claudeSession, nil)
	if lines := h.hookLog(); len(lines) != 3 || lines[2]["error"] != "the session reports to no mission: the origin remote of the checkout names no owner/name repository" {
		t.Fatalf("hook.log %v", lines)
	}
}

// A detached HEAD read after a branch, such as during a rebase stopped on a
// conflict, keeps the session on its branch's mission, unless an issue is
// declared for the detached HEAD.
func TestATransientDetachedHeadKeepsTheBranch(t *testing.T) {
	spectest.Proves(t, sessionFeature, identityRequirement, "a-session-that-checks-out-a-branch-reports-to-its-declared-mission")
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.attach("--issue", "4883")
	h.branch = ""
	h.hook(claude, "stop", claudeSession, nil)
	if sync := h.provider.lastSync(t); sync.Mission.Key != "issue-4883" || sync.Branch != sessionBranch {
		t.Errorf("stop sync mission %+v on branch %q", sync.Mission, sync.Branch)
	}
	h.environ["CLAUDE_CODE_SESSION_ID"] = claudeSession
	if code, stdout, _ := h.stage("check"); code != 0 || stdout != "The session is at stage check, and its mission issue-4883 knows it.\n" {
		t.Errorf("exit %d, stdout %q", code, stdout)
	}
	if h.hookLog() != nil {
		t.Errorf("hook.log %v", h.hookLog())
	}
	// An issue declared for the detached HEAD moves the session there.
	h.attach("--issue", "12")
	h.hook(claude, "stop", claudeSession, nil)
	if sync := h.provider.lastSync(t); sync.Mission.Key != "issue-12" || sync.Branch != "" {
		t.Errorf("stop sync mission %+v on branch %q", sync.Mission, sync.Branch)
	}
}

func TestNoMissionReason(t *testing.T) {
	spectest.Proves(t, sessionFeature, noMissionRequirement, "a-session-without-a-mission-says-why-once")
	for _, tc := range []struct {
		cache sessionCache
		want  string
	}{
		{sessionCache{}, "the session has not read its checkout yet"},
		{sessionCache{Root: "/r"}, "the origin remote of the checkout names no owner/name repository"},
		{sessionCache{Root: "/r", Repository: "a/b"}, "the checkout is on a detached HEAD and putnami agent attach --issue <n> declared no issue for it"},
		{sessionCache{Root: "/r", Repository: "a/b", Branch: "x y"}, "the branch name is not a mission key: run putnami agent attach --issue <n>"},
		{sessionCache{Root: "/r", Repository: "a/b", Mission: sessionMissionState{Key: "branch:x"}}, ""},
	} {
		if got := noMissionReason(&tc.cache); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.cache, got, tc.want)
		}
	}
}
