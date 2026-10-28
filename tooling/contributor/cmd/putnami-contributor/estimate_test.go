package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEstimateRunsInTheWorkspaceRoot pins the dispatch of `estimate`: it
// reads .putnami/<issue>/plan.json under the working directory putnami
// launches it in, and a usage error exits 2.
func TestEstimateRunsInTheWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	plan, err := os.ReadFile(filepath.Join("..", "..", "internal", "estimate", "testdata", "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".putnami", "42")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plan.json"), plan, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"estimate", "--issue", "42", "--putnamiContext", filepath.Join(root, "absent.json")}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d with an unreadable context, want 1; stderr %q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"estimate", "--issue", "42"}, &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "Plan "+filepath.Join(".putnami", "42", "plan.json")+": L,") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"estimate"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--issue <id>") {
		t.Fatalf("exit %d, stderr %q; want a usage error", code, stderr.String())
	}
}
