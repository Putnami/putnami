package agentctx

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	wsproto "go.putnami.dev/protocol/workspace"
	sdkagentartifact "go.putnami.dev/sdk/extension/agentartifact"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// publishedAgentTrees are the paths under the host trees that a published
// extension's agent content owns: the audit skill and its prune-* workers come
// from @putnami/intelligence, which no local source builds (.agents/constraints.md,
// "Agent Workflows").
var publishedAgentTrees = []string{
	".agents/skills/audit/",
	".claude/skills/audit/",
	".claude/agents/prune-",
	".codex/agents/prune-",
}

// generatedAgentTrees are the host directories this repository's own agent
// content owns.
var generatedAgentTrees = []string{".agents/skills", ".claude/skills", ".claude/agents", ".codex/agents"}

// TestLocalAgentContentMatchesCommittedRoot is the drift gate for this
// repository's own agent workflows. The root opts into the agent content of
// extensions it declares by path, so the committed host files are generated
// output: this test builds each opted-in local extension's content with the
// same builder the packager publishes with and fails when a committed file
// differs, is missing, or is no longer produced by any opted-in extension.
func TestLocalAgentContentMatchesCommittedRoot(t *testing.T) {
	spectest.Proves(t, "cli/contributor-workflows", "one-contributor-extension", "the-repository-runs-the-content-it-distributes")
	root := driftRepositoryRoot(t)
	cfg := wsproto.Load(root)
	refs, err := DeclaredAgentArtifacts(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	emitted := make(map[string]string)
	var drift []string
	inTree := 0
	for _, ref := range refs {
		located, err := extension.LocateAgentContent(root, cfg, ref.Extension)
		if err != nil {
			t.Fatalf("locate the agent content of %s: %v", ref.Extension, err)
		}
		if !located.Local {
			continue
		}
		source := "extension " + ref.Extension
		result, err := sdkagentartifact.BuildExtensionContent(located.Root, located.Contribution.Source, located.Name, localContentProbeVersion)
		if err != nil {
			t.Fatalf("build %s: %v", source, err)
		}
		inTree++
		for name, content := range result.Files {
			emitted[name] = ref.Name
			committed, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
			switch {
			case err != nil:
				drift = append(drift, name+": missing (built by "+source+")")
			case !bytes.Equal(committed, content):
				drift = append(drift, name+": differs from "+source)
			}
		}
	}
	if inTree == 0 {
		t.Fatal("putnami.workspace.json opts into no local extension's agent content, so this gate proved nothing")
	}
	for _, tree := range generatedAgentTrees {
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() {
				return walkErr
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			for _, prefix := range publishedAgentTrees {
				if strings.HasPrefix(rel, prefix) {
					return nil
				}
			}
			if _, ok := emitted[rel]; !ok {
				drift = append(drift, rel+": no opted-in local extension produces it")
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if len(drift) > 0 {
		sort.Strings(drift)
		t.Fatalf("committed agent workflow files drifted from their source:\n  %s\n"+
			"Edit the source under tooling/contributor/src, "+
			"then run `./putnamiw context generate` and commit the result.", strings.Join(drift, "\n  "))
	}
}

func driftRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, wsproto.WorkspaceConfigFilename)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("putnami.workspace.json not found above " + file)
		}
		dir = parent
	}
}
