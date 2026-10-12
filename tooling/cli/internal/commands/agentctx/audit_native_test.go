package agentctx

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The managed audit's proposal scan must reach the one native language
// evaluator through the installed check relay, with an ordinary provider
// binding. The fixture has a remote-tracking head, as the audit helper expects.
func TestManagedAuditProposalScanUsesNativeLanguage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the managed audit helper requires bash")
	}
	for _, tool := range []string{"bash", "git", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("managed audit helper requires %s: %v", tool, err)
		}
	}
	harness := collaborationHarness(t)
	root := t.TempDir()
	providerDir := filepath.ToSlash(filepath.Join(harness, "local-collaboration"))
	config, err := json.Marshal(map[string]any{
		"name":       "audit-language-fixture",
		"extensions": []string{providerDir},
		"options": map[string]any{"collaboration": map[string]any{
			"proposals": map[string]any{"provider": "@putnami/local-collaboration", "version": 1,
				"settings": map[string]any{"repository": "acme/app"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	writePathFile(t, root, "putnami.workspace.json", string(config))
	content := buildContributorContent(t)
	relayPath := ".agents/skills/check/scripts/english-only.sh"
	relay := content.Files[relayPath]
	if len(relay) == 0 || strings.Contains(string(relay), "FRENCH_WORDS=") {
		t.Fatal("packaged check path is missing or contains a second language implementation")
	}
	writePathFile(t, root, relayPath, string(relay))
	auditSource := filepath.Join(driftRepositoryRoot(t), ".agents", "skills", "audit", "scripts", "english-only-proposals.sh")
	audit, err := os.ReadFile(auditSource)
	if err != nil {
		t.Fatal(err)
	}
	auditPath := ".agents/skills/audit/scripts/english-only-proposals.sh"
	writePathFile(t, root, auditPath, string(audit))

	run := func(command string, args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command(command, args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "PATH="+harness+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	git := func(args ...string) string {
		t.Helper()
		out, err := run("git", args...)
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(out)
	}
	git("init", "-b", "main")
	git("config", "user.name", "Audit Test")
	git("config", "user.email", "audit-test@example.com")
	writePathFile(t, root, "README.md", "# Audit fixture\n")
	git("add", "README.md", "putnami.workspace.json")
	git("commit", "-m", "docs: add the audit fixture")
	head := git("rev-parse", "HEAD")
	git("update-ref", "refs/remotes/origin/main", head)
	git("update-ref", "refs/remotes/origin/topic", head)

	upsert := func(body string) {
		t.Helper()
		input, err := json.Marshal(map[string]any{
			"change": map[string]any{"base": "main", "head": "topic"},
			"title":  "docs: explain the parser", "body": body,
		})
		if err != nil {
			t.Fatal(err)
		}
		out, err := run(filepath.Join(harness, "putnami"), "proposals", "upsert", "--input", string(input), "--output=json")
		var response struct {
			Outcome string `json:"outcome"`
		}
		if decodeErr := json.Unmarshal([]byte(out), &response); err != nil || decodeErr != nil || response.Outcome != "ok" {
			t.Fatalf("proposal fixture upsert: %v: %s", err, out)
		}
	}
	scan := func(wantStatus int) string {
		t.Helper()
		out, err := run("bash", auditPath, "--base", "main", "--heads", "1")
		status := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("audit helper failed to run: %v: %s", err, out)
			}
			status = exit.ExitCode()
		}
		if status != wantStatus {
			t.Fatalf("audit helper status %d, want %d: %s", status, wantStatus, out)
		}
		return out
	}
	upsert("Nous corrigons le parseur.")
	if out := scan(1); !strings.Contains(out, "proposal ") || !strings.Contains(out, "non-English signals") {
		t.Fatalf("managed audit did not report the native language finding: %s", out)
	}
	upsert("The parser reads input in order.")
	if out := scan(0); !strings.Contains(out, "1 proposal(s) into main scanned, no offender") {
		t.Fatalf("managed audit did not report a clean native scan: %s", out)
	}
}
