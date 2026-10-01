package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/iox"
)

var lowercaseSHA256Command = regexp.MustCompile(`^[0-9a-f]{64}$`)

func treeCommandRepo(t *testing.T) string {
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

func runTree(t *testing.T, env *CommandEnv) (string, error) {
	t.Helper()
	command, ok := lookupCommand("tree")
	if !ok {
		t.Fatal("tree command is not registered")
	}
	var err error
	captured, captureErr := iox.CaptureStdout(func() error {
		err = command.run(env)
		return nil
	})
	if captureErr != nil {
		t.Fatalf("capture stdout: %v", captureErr)
	}
	return captured, err
}

// TestCmdTreeFingerprintNeedsNoWorkspace is the property the fix skill's script
// depends on: the digest is a pure git operation, and the script fingerprints
// throwaway repositories that carry no putnami.json. WsRoot is deliberately
// empty here — a handler that required a workspace would fail this outright.
func TestCmdTreeFingerprintNeedsNoWorkspace(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "gated-tree-identity",
		"one-digest-covers-head-the-tracked-diff-and-untracked-content")
	repo := treeCommandRepo(t)
	t.Chdir(repo)

	output, err := runTree(t, &CommandEnv{Ctx: context.Background(), Cfg: &wsproto.Config{}, Sub: "fingerprint"})
	if err != nil {
		t.Fatalf("tree fingerprint outside a workspace: %v", err)
	}
	// Human output is the bare digest and nothing else: its first caller is a
	// shell that captures it into a variable and compares it with another
	// agent's, so a label or a second line would have to be stripped by every
	// caller — and a caller that forgot would compare two different strings for
	// one tree.
	digest := strings.TrimSuffix(output, "\n")
	if !lowercaseSHA256Command.MatchString(digest) {
		t.Fatalf("tree fingerprint printed %q, want one bare lowercase hex sha256", output)
	}

	// The command reads the tree it RUNS IN, not a workspace root it was handed.
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("noise\n"), 0o644); err != nil {
		t.Fatalf("write untracked file: %v", err)
	}
	moved, err := runTree(t, &CommandEnv{Ctx: context.Background(), Cfg: &wsproto.Config{}, Sub: "fingerprint"})
	if err != nil {
		t.Fatalf("tree fingerprint after an untracked write: %v", err)
	}
	if strings.TrimSpace(moved) == digest {
		t.Errorf("an untracked file did not move the printed digest")
	}
}

// TestCmdTreeFingerprintStructuredOutputMirrorsTheRecord keeps one shape across
// the two surfaces: a consumer reading `--output=json` reads the same member
// names a recorded session's tree block carries.
func TestCmdTreeFingerprintStructuredOutputMirrorsTheRecord(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "gated-tree-identity",
		"one-digest-covers-head-the-tracked-diff-and-untracked-content")
	repo := treeCommandRepo(t)
	t.Chdir(repo)

	output, err := runTree(t, &CommandEnv{
		Ctx: context.Background(), Cfg: &wsproto.Config{}, Sub: "fingerprint", OutputFormat: "jsonl",
	})
	if err != nil {
		t.Fatalf("tree fingerprint --output=jsonl: %v", err)
	}
	var tree protocolcli.SessionTree
	if err := json.Unmarshal([]byte(output), &tree); err != nil {
		t.Fatalf("parse %q: %v", output, err)
	}
	if !lowercaseSHA256Command.MatchString(tree.Fingerprint) {
		t.Errorf("fingerprint = %q, want a lowercase hex sha256", tree.Fingerprint)
	}
	if tree.Dirty {
		t.Errorf("a freshly committed tree reported dirty")
	}
	if len(tree.HeadSHA) != 40 && len(tree.HeadSHA) != 64 {
		t.Errorf("headSHA = %q, want a full object id", tree.HeadSHA)
	}

	// The digest the two modes report is one digest, not two.
	human, err := runTree(t, &CommandEnv{Ctx: context.Background(), Cfg: &wsproto.Config{}, Sub: "fingerprint"})
	if err != nil {
		t.Fatalf("tree fingerprint: %v", err)
	}
	if strings.TrimSpace(human) != tree.Fingerprint {
		t.Errorf("human output %q disagrees with structured %q", strings.TrimSpace(human), tree.Fingerprint)
	}
}

func TestCmdTreeRejectsWhatItDoesNotAccept(t *testing.T) {
	t.Parallel()
	command, ok := lookupCommand("tree")
	if !ok {
		t.Fatal("tree command is not registered")
	}
	cases := map[string]*CommandEnv{
		"an unknown subcommand": {Ctx: context.Background(), Cfg: &wsproto.Config{}, Sub: "digest"},
		"a positional argument": {Ctx: context.Background(), Cfg: &wsproto.Config{}, Sub: "fingerprint", Args: []string{"HEAD"}},
		"an undeclared flag":    {Ctx: context.Background(), Cfg: &wsproto.Config{}, Sub: "fingerprint", Args: []string{"--algorithm", "sha1"}},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if err := command.run(env); !errors.Is(err, cmderr.ErrUsage) {
				t.Errorf("err = %v, want ErrUsage", err)
			}
		})
	}
}

// TestCmdTreeNamesVerifyAmongItsSubcommands keeps the usage error of an unknown
// subcommand in step with the subcommands cmdTree dispatches.
func TestCmdTreeNamesVerifyAmongItsSubcommands(t *testing.T) {
	t.Parallel()
	command, ok := lookupCommand("tree")
	if !ok {
		t.Fatal("tree command is not registered")
	}
	err := command.run(&CommandEnv{Ctx: context.Background(), Cfg: &wsproto.Config{}, Sub: "digest"})
	if !errors.Is(err, cmderr.ErrUsage) || !strings.Contains(err.Error(), "Available: fingerprint, verify") {
		t.Fatalf("err = %v, want a usage error naming fingerprint and verify", err)
	}
}
