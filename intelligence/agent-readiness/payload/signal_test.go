package payload_test

import (
	"strings"
	"testing"
	"time"

	"go.putnami.dev/intelligence/agent-readiness/contract"
	"go.putnami.dev/intelligence/agent-readiness/internal/gittest"
)

func TestReliableSignalLeavesProductionCodeOut(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	r := gittest.New(t)
	r.Write("go.mod", "module acme\n\ngo 1.23.4\n")
	r.Write(".github/workflows/ci.yml", "on:\n  pull_request:\njobs:\n  test:\n    steps:\n      - run: go test ./...\n")
	r.Write("engine/invoker.go", "package engine\n")
	r.Write("engine/invoker_test.go", "package engine\n")
	r.Write("web/database.ts", "export const db = 1;\n")
	r.Write("web/playwright.config.ts", "export default {};\n")
	r.Commit("chore: bootstrap", authors[0], day(now, -200))

	r.Write("engine/invoker.go", "package engine\n\nimport \"fmt\"\n\nfunc Fail(n int) error { return fmt.Errorf(\"invoke: retries: %d\", n) }\n")
	r.Write("web/database.ts", "export const byId = (id: string) => IDBKeyRange.only(id);\n")
	r.Commit("feat: retries (#1)", authors[0], day(now, -20))
	r.Write("engine/invoker_test.go", "package engine\n\nimport \"testing\"\n\nfunc TestFail(t *testing.T) { t.Skip(\"flaky\") }\n")
	r.Write("web/playwright.config.ts", "export default { retries: 2 };\n")
	r.Commit("test: quarantine (#2)", authors[1], day(now, -10))

	p := collect(t, r.Dir, now).Payload
	m := find(t, p, "verify.reliable-signal", "")
	if m.State != contract.MarkerExists || value(t, m) != 2 {
		t.Fatalf("reliable signal = %+v, want exists with the skip and the retry count", m)
	}
	if got := strings.Join(m.Evidence.Sample, ","); got != "engine/invoker_test.go:5,web/playwright.config.ts:1" {
		t.Fatalf("sample = %s, want the test and the runner configuration only", got)
	}
	printed := r.Shell(m.Evidence.Command)
	if added := addedSignals(printed); float64(added) != value(t, m) {
		t.Fatalf("%s printed %q, want %v matches", m.Evidence.Command, printed, value(t, m))
	}
}

func TestReliableSignalLeavesGuardedSkipsOut(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	r := gittest.New(t)
	r.Write("go.mod", "module acme\n\ngo 1.23.4\n")
	r.Write(".github/workflows/ci.yml", "on:\n  pull_request:\njobs:\n  test:\n    steps:\n      - run: go test ./...\n")
	r.Write("store/store_test.go", "package store\n")
	r.Commit("chore: bootstrap", authors[0], day(now, -200))

	r.Write("store/store_test.go", strings.Join([]string{
		"package store",
		"",
		"func open(t *testing.T) {",
		"\tres, err := testprovider.Provision(ctx, opts)",
		"\tif errors.Is(err, testprovider.ErrSkip) {",
		"\t\tt.Skip(\"no usable test database binding (mode=skip)\")",
		"\t}",
		"}",
		"",
		"func TestShell(t *testing.T) {",
		"\tif _, err := exec.LookPath(\"/bin/sh\"); err != nil {",
		"\t\tt.Skip(\"/bin/sh unavailable on this platform\")",
		"\t}",
		"}",
		"",
		"func TestLedger(t *testing.T) { t.Skip(\"flaky\") }",
		"",
	}, "\n"))
	r.Commit("test: guard the database and the shell (#3)", authors[1], day(now, -10))

	p := collect(t, r.Dir, now).Payload
	m := find(t, p, "verify.reliable-signal", "")
	if m.State != contract.MarkerExists || value(t, m) != 1 {
		t.Fatalf("reliable signal = %+v, want exists with the flaky skip only", m)
	}
	if got := strings.Join(m.Evidence.Sample, ","); got != "store/store_test.go:16" {
		t.Fatalf("sample = %s, want the flaky skip only", got)
	}
	printed := r.Shell(m.Evidence.Command)
	if added := addedSignals(printed); added != 3 || !strings.Contains(printed, "exec.LookPath") || !strings.Contains(printed, "testprovider.ErrSkip") {
		t.Fatalf("%s printed %q, want the three skips with their guards", m.Evidence.Command, printed)
	}
}
