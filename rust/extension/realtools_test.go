package rust

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/job"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/tooling/stacks"
)

// RealToolsVariable makes the real-tool proof fail, instead of skip, when a
// tool is missing: the CI leg that installs them sets it.
const RealToolsVariable = "PUTNAMI_RUST_REAL_TOOLS"

// demoFixture is the fixture repository of the observe Cargo detector.
const demoFixture = "../../tooling/observe/internal/observe/testdata/demos/rust.txtar"

// writeDemo writes the demo's base tree, then its head tree, under root.
func writeDemo(t *testing.T, root string) {
	t.Helper()
	data, err := os.ReadFile(demoFixture)
	if err != nil {
		t.Fatal(err)
	}
	type file struct{ name, content string }
	var files []file
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if header, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "-- "); ok && strings.HasSuffix(header, " --") {
			files = append(files, file{name: strings.TrimSuffix(header, " --")})
			continue
		}
		if len(files) > 0 {
			files[len(files)-1].content += line
		}
	}
	for _, stage := range []string{"base/", "head/"} {
		for _, f := range files {
			if rest, ok := strings.CutPrefix(f.name, stage); ok {
				writeFile(t, root, rest, f.content)
			}
		}
	}
}

// TestTheFixtureRepositoryPassesWithTheRealTools carries no spec check: it
// skips on a machine without the tools, and the rust-stack workflow, which
// installs them, runs it.
func TestTheFixtureRepositoryPassesWithTheRealTools(t *testing.T) {
	for _, tool := range []string{"rustc", "cargo", "cargo-nextest", "cargo-llvm-cov"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv(RealToolsVariable) == "1" {
				t.Fatalf("%s=1 and %s is not on PATH", RealToolsVariable, tool)
			}
			t.Skipf("%s is not on PATH; set %s=1 to require it", tool, RealToolsVariable)
		}
	}
	// The reports and artifacts the profile names lie in the stack root's
	// target directory.
	t.Setenv("CARGO_TARGET_DIR", "")
	if err := os.Unsetenv("CARGO_TARGET_DIR"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeDemo(t, root)
	crates := []string{"crates/pricing", "crates/api", "crates/worker"}

	// workspace-fetch fills a cargo home of its own with the network; every
	// task then runs offline and reads only that home.
	t.Setenv("CARGO_HOME", t.TempDir())
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	var refs []job.ProjectRef
	for _, crate := range crates {
		refs = append(refs, crateRef(root, crate, `{}`))
	}
	if status, emitted, err := runWorkspaceJob(t, root, WorkspaceFetchJobName, nil, refs...); status != "OK" || err != nil {
		t.Fatalf("workspace-fetch: %s %v\n%s", status, err, emitted)
	}
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")

	tests := map[string]int{"crates/api": 2, "crates/pricing": 4, "crates/worker": 1}
	for _, crate := range crates {
		for _, task := range stacks.TaskNames {
			status, data, _, emitted, err := runJobOutput(t, root, crate, task)
			if status != "OK" || err != nil {
				t.Fatalf("%s %s: %s %v\n%s", crate, task, status, err, emitted)
			}
			for _, code := range []string{stacks.CodeTestUnattributed, stacks.CodeCoverageUnmapped, stacks.CodeStackRootUnsupported} {
				if strings.Contains(emitted, code) {
					t.Errorf("%s %s reports %s:\n%s", crate, task, code, emitted)
				}
			}
			if task != stacks.TaskTest {
				continue
			}
			if got := data["testSummary"].(map[string]any); got["total"] != tests[crate] || got["failed"] != 0 {
				t.Errorf("%s testSummary = %v, want %d cases", crate, got, tests[crate])
			}
			if got := data["coverageSummary"].(map[string]any); got["total"].(int) == 0 || got["covered"] != got["total"] {
				t.Errorf("%s coverageSummary = %v, want every line of its own files", crate, got)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, "target", "nextest", "ci", "junit.xml")); err != nil {
		t.Errorf("the nextest ci profile wrote its report under the stack root: %v", err)
	}

	// The filter selects the tests whose name holds it, and a crate where it
	// selects none passes, with and without coverage.
	filtered := map[string]int{"crates/api": 1, "crates/pricing": 1, "crates/worker": 0}
	params := pctx.Params{testFilterFlag: json.RawMessage(`"lines"`)}
	for _, coverage := range []bool{true, false} {
		if !coverage {
			// A cargo-llvm-cov that finds no llvm-tools fails the coverage
			// probe, so the task runs cargo nextest run.
			shim := t.TempDir()
			writeFile(t, shim, "cargo-llvm-cov", "#!/bin/sh\nexit 1\n")
			if err := os.Chmod(filepath.Join(shim, "cargo-llvm-cov"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
		}
		for _, crate := range crates {
			status, data, _, emitted, err := runJobParams(t, root, crate, stacks.TaskTest, params)
			if status != "OK" || err != nil {
				t.Fatalf("%s test -t lines, coverage %v: %s %v\n%s", crate, coverage, status, err, emitted)
			}
			if got := data["testSummary"].(map[string]any); got["total"] != filtered[crate] || got["failed"] != 0 {
				t.Errorf("%s test -t lines, coverage %v: testSummary = %v, want %d cases", crate, coverage, got, filtered[crate])
			}
			if _, measured := data["coverageSummary"]; measured != coverage {
				t.Errorf("%s test -t lines, coverage %v: coverageSummary = %v", crate, coverage, data["coverageSummary"])
			}
		}
	}
}
