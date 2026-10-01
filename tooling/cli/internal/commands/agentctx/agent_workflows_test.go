package agentctx

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// skipSourceWrapperOnWindows skips a fix skill script test that runs this
// repository's putnamiw on Windows. In a source workspace the scripts resolve
// the CLI through putnamiw, which builds the CLI from source and stays on macOS
// and Linux; a Windows consumer runs the same scripts against the installed
// putnami. The session-cap hook needs no CLI and runs everywhere.
func skipSourceWrapperOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the test runs the source workspace's putnamiw, which stays on macOS and Linux")
	}
}

func TestPortableFixFinalizer(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/contributor-workflows", "provider-neutral-publication", "the-finalizer-publishes-through-the-contracts")
	spectest.Proves(t, "cli/contributor-workflows", "provider-neutral-publication", "the-read-back-carries-what-the-upsert-answered")
	spectest.Proves(t, "cli/contributor-workflows", "provider-neutral-publication", "an-unresolved-write-stops-without-a-retry")
	spectest.Proves(t, "cli/contributor-workflows", "repository-policy", "the-policy-selects-the-delivered-state-reference-and-language")
	spectest.Proves(t, "cli/contributor-workflows", "squash-ready-publication", "a-draft-opens-before-the-first-edit")
	spectest.Proves(t, "cli/contributor-workflows", "squash-ready-publication", "the-body-is-the-squash-message")
	spectest.Proves(t, "cli/contributor-workflows", "squash-ready-publication", "no-publication-names-an-agent")
	spectest.Proves(t, "cli/contributor-workflows", "squash-ready-publication", "a-loaded-machine-gates-on-hosted-checks")
	skipSourceWrapperOnWindows(t)
	runSkillScriptTest(t, "fix", "finalize-pr.test.sh", "finalize-pr test: ok", collaborationScriptEnv(t))
}

func TestTreeFingerprint(t *testing.T) {
	skipSourceWrapperOnWindows(t)
	runSkillScriptTest(t, "fix", "tree-fingerprint.test.sh", "tree-fingerprint test: ok", nil)
}

func TestMachineLoad(t *testing.T) {
	skipSourceWrapperOnWindows(t)
	runSkillScriptTest(t, "fix", "machine-load.test.sh", "machine-load test: ok", nil)
}

func TestEnglishOnlyDetector(t *testing.T) {
	spectest.Proves(t, "cli/contributor-workflows", "provider-neutral-publication", "the-language-rule-reads-tasks-through-the-contract")
	runSkillScriptTest(t, "check", "english-only.test.sh", "english-only test: ok", collaborationScriptEnv(t))
}

func TestSessionCapHook(t *testing.T) {
	runSkillScriptTest(t, "fix", "session-cap.test.sh", "session-cap test: ok", nil)
}

// englishOnlyUsageLine is one mode line of the shared detector's usage block:
// "bash english-only.sh <mode> ... # <comment>".
var englishOnlyUsageLine = regexp.MustCompile(`(?m)^#\s+bash english-only\.sh (\w+)\b.*#\s*(.*)$`)

// englishOnlyCall is one invocation of the shared detector in a hand-authored
// skill file.
var englishOnlyCall = regexp.MustCompile(`english-only\.sh (\w+)`)

// The hand-authored audit's language rule keeps covering what it covered —
// tracked files, tasks and change proposals — through the collaboration
// contracts: it calls only the shared detector's current modes, never a
// deprecated alias, and scans proposals with its own script over
// `putnami proposals find`, which runs here against the real CLI and the
// shipped local provider.
func TestAuditLanguageRuleScansTasksAndProposalsThroughTheContracts(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(cwd, "..", "..", "..", "..", ".."))
	detector, err := os.ReadFile(filepath.Join(root, ".agents", "skills", "check", "scripts", "english-only.sh"))
	if err != nil {
		t.Fatal(err)
	}
	current := map[string]bool{}
	for _, match := range englishOnlyUsageLine.FindAllStringSubmatch(string(detector), -1) {
		current[match[1]] = !strings.Contains(match[2], "deprecated")
	}
	if len(current) == 0 {
		t.Fatal("the detector's usage block lists no mode; the assertions below would pass vacuously")
	}
	skill, err := os.ReadFile(filepath.Join(root, ".agents", "skills", "audit", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	calls := englishOnlyCall.FindAllStringSubmatch(string(skill), -1)
	modes := map[string]bool{}
	for _, call := range calls {
		mode := call[1]
		modes[mode] = true
		if supported, known := current[mode]; !known || !supported {
			t.Errorf("the audit calls english-only.sh %s, which is not a current mode of the detector", mode)
		}
	}
	for _, want := range []string{"files", "tasks"} {
		if !modes[want] {
			t.Errorf("the audit's language rule no longer scans with english-only.sh %s", want)
		}
	}
	if !strings.Contains(string(skill), "scripts/english-only-proposals.sh") {
		t.Error("the audit's language rule no longer scans change proposals")
	}
	runSkillScriptTest(t, "audit", "english-only-proposals.test.sh", "english-only-proposals test: ok", collaborationScriptEnv(t))
}

// runSkillScriptTest runs a materialized skill script's bash test from the
// repository root and requires its success marker. A nil env inherits the
// caller's environment.
func runSkillScriptTest(t *testing.T, skill, name, okMarker string, env []string) {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	// cwd is tooling/cli/internal/commands/agentctx; five levels up is the repo
	// root.
	repoRoot := filepath.Clean(filepath.Join(cwd, "..", "..", "..", "..", ".."))
	script := filepath.Join(repoRoot, ".agents", "skills", skill, "scripts", name)
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("%s is unavailable: %v", name, err)
	}

	cmd := exec.Command("bash", script)
	cmd.Dir = repoRoot
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		// CI reports a failure's first line only: lead with the suite's last
		// output line, which names the failed assertion.
		t.Fatalf("%s failed: %v: %s\n%s", name, err, lastOutputLine(output), output)
	}
	if !strings.Contains(string(output), okMarker) {
		t.Fatalf("%s did not report success:\n%s", name, output)
	}
}

func lastOutputLine(output []byte) string {
	lines := strings.Split(strings.TrimRight(string(output), "\n"), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// Shared resources live in .agents while both hosts have skill entrypoints.
// Check actual link targets: byte parity alone accepts equally broken copies.
func TestExecuteResourceLinks(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(cwd, "..", "..", "..", "..", ".."))
	links := regexp.MustCompile(`\]\(([^)]+)\)`)
	for _, host := range []string{".agents", ".claude"} {
		for _, skill := range []string{"execute", "fix", "epic", "code-review", "fix-loop", "plan", "check", "content-bump"} {
			path := filepath.Join(root, host, "skills", skill, "SKILL.md")
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, match := range links.FindAllSubmatch(body, -1) {
				target := string(match[1])
				if strings.Contains(target, "://") || strings.HasPrefix(target, "#") {
					continue
				}
				target = strings.SplitN(target, "#", 2)[0]
				if _, err := os.Stat(filepath.Join(filepath.Dir(path), target)); err != nil {
					t.Errorf("%s references unavailable resource %s: %v", path, target, err)
				}
			}
		}
	}
}
