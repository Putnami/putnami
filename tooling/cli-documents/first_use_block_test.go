package documents

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The first-use smoke runs the block of the getting-started page as printed.
// The README prints the same block, and neither document may name a tool the
// smoke's bare image does not hold. This test reads the block the way the smoke
// does, through the smoke's own --print-block, so the three cannot drift apart.

const (
	firstUseSmokeRepoPath   = "tooling/cli/scripts/smoke-first-use.sh"
	gettingStartedRepoPath  = "sites/putnami.dev/doc/01-getting-started/index.md"
	firstUseBlockLastLine   = "putnami serve webapp"
	firstUseBlockFirstLine  = "curl -fsSL https://putnami.dev/install.sh | bash"
	firstUseBlockExportLine = `export PATH="$HOME/.putnami/bin:$PATH"`
)

var firstBashBlock = regexp.MustCompile("(?s)```bash\n(.*?\n)```")

func TestTheFirstUseSmokeRunsTheBlockTheReadmeAndTheGettingStartedPagePrint(t *testing.T) {
	requireShell(t)
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	root := repositoryRoot(t)

	cmd := exec.Command("bash", filepath.Join(root, filepath.FromSlash(firstUseSmokeRepoPath)), "--print-block")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "SMOKE_FIRST_USE_PAGE=")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("smoke-first-use.sh --print-block: %v\n%s", err, output)
	}
	block := string(output)

	lines := strings.Split(strings.TrimSuffix(block, "\n"), "\n")
	if lines[0] != firstUseBlockFirstLine || lines[len(lines)-1] != firstUseBlockLastLine {
		t.Fatalf("the smoke's block does not install first and serve last:\n%s", block)
	}
	// Without this line, the commands after the installer are not found in the
	// shell that ran it: a script piped into bash cannot change its parent's PATH.
	if lines[1] != firstUseBlockExportLine {
		t.Fatalf("the smoke's block does not put the install directory on PATH right after the installer:\n%s", block)
	}

	for _, repoPath := range []string{"README.md", gettingStartedRepoPath} {
		document, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(repoPath)))
		if err != nil {
			t.Fatal(err)
		}
		match := firstBashBlock.FindSubmatch(document)
		if match == nil {
			t.Fatalf("%s has no fenced bash block", repoPath)
		}
		if string(match[1]) != block {
			t.Errorf("the first bash block of %s is not the block the smoke runs:\n%s\nwant:\n%s", repoPath, match[1], block)
		}
	}
}
