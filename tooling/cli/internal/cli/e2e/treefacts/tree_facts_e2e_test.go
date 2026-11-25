// Package treefacts drives a planned job through the real CLI on a fixture
// workspace under Git, and reads the tree facts its context carries with no
// git on the job's PATH. The tests set the environment and change the working
// directory, so they run in their own test binary.
package treefacts

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	protocoljob "go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/gittest"
	"go.putnami.dev/sdk/extension/treefacts"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

func TestMain(m *testing.M) {
	os.Exit(clitest.Main(m))
}

// factsManifest declares a planned "facts" command whose task declares the
// four tree facts as runtime inputs, and the release baseline's files.
const factsManifest = `{
  "$schema": "https://putnami.dev/schemas/putnami-extension.json",
  "name": "@putnami/facts-extension",
  "version": "0.1.0",
  "cliContract": 7,
  "commands": {
    "facts": {
      "description": "Reads the tree facts.",
      "activationFiles": ["main.go"],
      "run": [{ "id": "facts", "task": "facts-task" }]
    }
  },
  "tasks": {
    "facts-task": {
      "kind": "command",
      "command": "{extensionRoot}/facts.sh",
      "cache": false,
      "timeoutMs": -1,
      "inputs": {
        "sources": { "from": "project", "files": ["main.go"] },
        "trackedFiles": { "from": "runtime" },
        "ignoredDirectories": { "from": "runtime" },
        "repository": { "from": "runtime" },
        "releaseBaseline": { "from": "runtime" },
        "releaseBaselineFiles": { "from": "runtime" }
      }
    }
  }
}`

// factsScript removes git from its PATH, fails if a git is still found,
// copies its context document, and reads main.go from the release baseline
// directory the context names, with shell builtins alone.
const factsScript = `#!/bin/sh
PATH=/nonexistent
if command -v git >/dev/null 2>&1; then
  echo "git is on the job's PATH" >&2
  exit 7
fi
ctx=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --putnamiContext) ctx="$2"; shift 2 ;;
    *) shift ;;
  esac
done
content=""
while IFS= read -r line || [ -n "$line" ]; do content="$content$line"; done < "$ctx"
printf '%s\n' "$content" > "$TREE_FACTS_DUMP"
rest=${content#*\"releaseBaseline\":}
rest=${rest#*\"directory\":\"}
dir=${rest%%\"*}
IFS= read -r first < "$dir/main.go"
printf '%s\n' "$first" > "$TREE_FACTS_DUMP.baseline"
`

// writeWorkspace commits a workspace whose app project is tagged v0.1.0, then
// changes it in a commit that declares a breaking change, and ignores dist.
func writeWorkspace(t *testing.T, root string) {
	t.Helper()
	clitest.RequireShell(t)
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"facts-ws","includes":["facts-extension","app"]}`)
	clitest.WriteFile(t, filepath.Join(root, "facts-extension", "putnami.json"), `{"name":"@putnami/facts-extension"}`)
	clitest.WriteFile(t, filepath.Join(root, "facts-extension", "putnami.extension.json"), factsManifest)
	if err := os.WriteFile(filepath.Join(root, "facts-extension", "facts.sh"), []byte(factsScript), 0o755); err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, filepath.Join(root, "app", "putnami.json"), `{"name":"app","extensions":["@putnami/facts-extension"]}`)
	clitest.WriteFile(t, filepath.Join(root, "app", "main.go"), "package released\n")
	clitest.WriteFile(t, filepath.Join(root, ".gitignore"), "dist/\n.putnami/\n")
	gittest.Run(t, root, "init", "-q", "-b", "main")
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-q", "-m", "feat: the workspace")
	gittest.Run(t, root, "tag", "-a", "v0.1.0", "-m", "release 0.1.0")
	clitest.WriteFile(t, filepath.Join(root, "app", "main.go"), "package changed\n")
	gittest.Run(t, root, "commit", "-q", "-am", "feat(app)!: change the package")
	clitest.WriteFile(t, filepath.Join(root, "app", "dist", "out.txt"), "built\n")
}

// A planned, uncached job that declares the four tree facts receives them in
// its context: with no git on its PATH, it reads the release baseline's files
// from the directory the context names, the context passes the strict reader,
// and the SDK answers the other three facts from that context with no git on
// the test's PATH either. The keys of these facts are pinned by the
// scheduler's and cachekey's tests.
func TestAJobReadsTheTreeFactsWithoutGit(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "tree-facts-follow-the-key", "a-job-reads-the-tree-facts-without-git")
	root := t.TempDir()
	writeWorkspace(t, root)
	dump := filepath.Join(t.TempDir(), "context.json")
	t.Setenv("TREE_FACTS_DUMP", dump)

	var code int
	output := clitest.CaptureStdoutStderr(t, func() {
		app, err := cli.NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(root)
		code = app.Run(context.Background(), []string{"facts", "--projects", "app"})
	})
	if code != cli.ExitSuccess {
		t.Fatalf("facts exit = %d, want %d:\n%s", code, cli.ExitSuccess, output)
	}
	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the job wrote no context: %v\n%s", err, output)
	}
	document, diags := protocoljob.ParseAndValidate(data)
	if diag.HasErrors(diags) {
		t.Fatalf("the job's context fails the strict reader: %v", diags)
	}
	baselineLine, err := os.ReadFile(dump + ".baseline")
	if err != nil || strings.TrimSpace(string(baselineLine)) != "package released" {
		t.Fatalf("main.go in the release baseline directory = %q (%v), want the tagged content", baselineLine, err)
	}

	t.Setenv("PATH", t.TempDir())
	facts := treefacts.From(document)
	app := filepath.Join(document.WorkspaceRoot, "app")
	if tracked, err := facts.TrackedFiles(app); err != nil || strings.Join(tracked, ",") != "main.go,putnami.json" {
		t.Errorf("tracked files = %v, %v; want main.go and putnami.json", tracked, err)
	}
	if ignored := facts.IgnoredDirectories(app); len(ignored) != 1 || !ignored["dist"] {
		t.Errorf("ignored directories = %v, want dist", ignored)
	}
	if state, err := facts.Repository(); err != nil || state != protocoljob.RepositoryComplete {
		t.Errorf("repository = %q, %v; want complete", state, err)
	}
	baseline, ok := facts.ReleaseBaseline()
	if !ok || baseline.State != protocoljob.ReleaseBaselineTagged || baseline.Tag != "v0.1.0" || !baseline.Breaking {
		t.Errorf("release baseline = %+v, want v0.1.0 with the breaking change", baseline)
	}
}
