package rust

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
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

func TestTheFixtureRepositoryPassesWithTheRealTools(t *testing.T) {
	spectest.Proves(t, feature, "real-tools-pass", "the-fixture-repository-passes-with-the-real-tools")
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

	tests := map[string]int{"crates/api": 2, "crates/pricing": 4, "crates/worker": 1}
	for _, crate := range []string{"crates/pricing", "crates/api", "crates/worker"} {
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
}
