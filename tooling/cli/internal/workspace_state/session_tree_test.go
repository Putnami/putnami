package workspace_state

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	gitpkg "go.putnami.dev/tooling/cli/internal/git"
)

// The recorded tree block.
//
// A session that gates a tree and a reader that later trusts the record must
// agree on WHICH tree, and the only thing that can carry that agreement is a
// content digest the session itself measured. These pin the two halves: the
// record carries the digest of the tree the session OPENED on — not the one its
// tasks left behind — and a session that could not determine the tree records no
// block at all rather than an invented clean one.

func treeTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
	} {
		command := exec.Command("git", args...)
		command.Dir = dir
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "initial"}} {
		command := exec.Command("git", args...)
		command.Dir = dir
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func recordedSession(t *testing.T, session *Session) cli.SessionFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(session.Dir(), "session.json"))
	if err != nil {
		t.Fatalf("read session.json: %v", err)
	}
	if violations := cli.ValidateDocument(cli.DocumentSessionFile, data); len(violations) != 0 {
		t.Fatalf("recorded session violates the contract: %v\n%s", violations, data)
	}
	var parsed cli.SessionFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse session.json: %v", err)
	}
	return parsed
}

func minimalSessionFile() *cli.SessionFile {
	return &cli.SessionFile{
		Commands: []string{"lint", "test", "build", "validate"},
		Run: cli.RunSummary{
			Outcome:  cli.RunOutcomeSuccess,
			ExitCode: cli.ExitSuccess,
			Counts:   cli.RunCounts{Total: 1, Succeeded: 1},
		},
	}
}

func TestSessionRecordsTheTreeItOpenedOn(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "gated-tree-identity",
		"a-recorded-session-names-the-tree-it-opened-on")
	repo := treeTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "worker.txt"), []byte("what the gate consumed\n"), 0o644); err != nil {
		t.Fatalf("write untracked input: %v", err)
	}

	session, err := NewSession(t.TempDir())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	session.CaptureTree(repo)

	atStart, err := gitpkg.FingerprintTree(repo)
	if err != nil {
		t.Fatalf("FingerprintTree: %v", err)
	}

	// The session's own work rewrites one of its inputs, exactly as `lint --fix`
	// does inside a gate. The record must keep naming the tree the run STARTED
	// on: a reader comparing it against the tree on disk is entitled to learn
	// that the gate mutated it, and a capture taken at finalize would hide that.
	if err := os.WriteFile(filepath.Join(repo, "worker.txt"), []byte("what the gate produced\n"), 0o644); err != nil {
		t.Fatalf("rewrite input: %v", err)
	}

	if err := session.FinalizeV2(minimalSessionFile(), repo, ""); err != nil {
		t.Fatalf("FinalizeV2: %v", err)
	}
	parsed := recordedSession(t, session)
	if parsed.Tree == nil {
		t.Fatal("the recorded session carries no tree block")
	}
	if parsed.Tree.Fingerprint != atStart.Fingerprint {
		t.Errorf("recorded fingerprint = %s, want the start-of-run %s", parsed.Tree.Fingerprint, atStart.Fingerprint)
	}
	if parsed.Tree.HeadSHA != atStart.HeadSHA {
		t.Errorf("recorded headSHA = %s, want %s", parsed.Tree.HeadSHA, atStart.HeadSHA)
	}
	if !parsed.Tree.Dirty {
		t.Errorf("a worktree carrying an untracked input recorded dirty = false")
	}

	after, err := gitpkg.FingerprintTree(repo)
	if err != nil {
		t.Fatalf("FingerprintTree after the run: %v", err)
	}
	if after.Fingerprint == parsed.Tree.Fingerprint {
		t.Error("the scenario is wrong, the mutation did not move the tree")
	}
}

func TestSessionRecordsACleanTreeAsCleanNotAsAbsent(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "gated-tree-identity",
		"a-recorded-session-names-the-tree-it-opened-on")
	repo := treeTestRepo(t)
	session, err := NewSession(t.TempDir())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	session.CaptureTree(repo)
	if err := session.FinalizeV2(minimalSessionFile(), repo, ""); err != nil {
		t.Fatalf("FinalizeV2: %v", err)
	}

	parsed := recordedSession(t, session)
	if parsed.Tree == nil {
		t.Fatal("a clean worktree recorded no tree block")
	}
	if parsed.Tree.Dirty {
		t.Errorf("a clean worktree recorded dirty = true")
	}
	data, err := os.ReadFile(filepath.Join(session.Dir(), "session.json"))
	if err != nil {
		t.Fatalf("read session.json: %v", err)
	}
	// A clean verdict is a measurement, so it is written rather than elided:
	// absence of the whole block is the only spelling of "unknown".
	if !strings.Contains(string(data), "\"dirty\": false") {
		t.Errorf("a clean tree dropped its dirty verdict:\n%s", data)
	}
}

func TestSessionWithoutAKnownTreeRecordsNothing(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "gated-tree-identity",
		"a-producer-that-cannot-determine-the-tree-records-nothing")
	cases := map[string]string{
		"no repository root supplied":          "",
		"a directory outside any git worktree": t.TempDir(),
	}
	for name, repoRoot := range cases {
		t.Run(name, func(t *testing.T) {
			session, err := NewSession(t.TempDir())
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			session.CaptureTree(repoRoot)
			if err := session.FinalizeV2(minimalSessionFile(), "", ""); err != nil {
				t.Fatalf("FinalizeV2: %v", err)
			}
			if parsed := recordedSession(t, session); parsed.Tree != nil {
				t.Errorf("an undeterminable tree was recorded as %+v", parsed.Tree)
			}
		})
	}
}
