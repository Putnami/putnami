package cli

import (
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestFinalizeOptionsRejectsGateExecutionAndAmbiguousArguments(t *testing.T) {
	t.Parallel()
	args := []string{"--base", "main", "--branch", "topic", "--title", "fix: change", "--body-file", "body.md", "--verification-file", "proof.json", "--partial-delivery"}
	got, err := finalizeOptions(args)
	if err != nil || !got.PartialDelivery || got.VerificationFile != "proof.json" {
		t.Fatalf("options=%+v err=%v", got, err)
	}
	for _, extra := range [][]string{{"--gate", "local"}, {"--retry-failed"}, {"--file", "source.go"}, {"--proof-status", "passed"}, {"--base", "other"}} {
		_, err := finalizeOptions(append(append([]string{}, args...), extra...))
		if err == nil {
			t.Fatalf("unsupported argument %v accepted", extra)
		}
	}
	if _, err := finalizeOptions([]string{"--title", ""}); err == nil {
		t.Fatal("empty title accepted")
	}
	if _, err := finalizeOptions([]string{"--body-file", "--draft"}); err == nil || !strings.Contains(err.Error(), "value") {
		t.Fatalf("missing body value: %v", err)
	}
	hosted, err := finalizeOptions([]string{"--gate", "ci", "--workflow-file", ".context/workflow.json"})
	if err != nil || hosted.Gate != "ci" || hosted.WorkflowFile != ".context/workflow.json" {
		t.Fatalf("hosted workflow options=%+v err=%v", hosted, err)
	}
}

func TestRequirementOwnersKeepOnlyProjectsThatCanHoldARequirement(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/contributor-workflows", "squash-ready-publication", "a-featureless-partial-needs-no-workflow")
	repo := t.TempDir()
	root := filepath.Join(repo, "ws")
	none := `"featureAuthority":{"none":"A wire contract, not a user-facing feature."}`
	projects := map[string]string{
		"proto":    `{"name":"proto",` + none + `}`,
		"app":      `{"name":"app"}`,
		"part":     `{"name":"part","featureAuthority":{"owner":"app/feature"}}`,
		"flipped":  `{"name":"flipped"}`,
		"authored": `{"name":"authored",` + none + `}`,
		"blank":    `{"name":"blank","featureAuthority":{"none":"  "}}`,
	}
	includes := []string{}
	for _, name := range slices.Sorted(maps.Keys(projects)) {
		includes = append(includes, `"packages/`+name+`"`)
		writeParityFile(t, filepath.Join(root, "packages", name), "putnami.json", projects[name])
	}
	includes = append(includes, `"packages/fresh"`)
	writeParityFile(t, root, wsproto.WorkspaceConfigFilename, `{"name":"owners","includes":[`+strings.Join(includes, ",")+`]}`)
	writeParityFile(t, filepath.Join(root, "packages", "authored"), "putnami.features.json", `{}`)
	ownersGit(t, repo, "init", "-q")
	ownersGit(t, repo, "add", "-A")
	ownersGit(t, repo, "commit", "-q", "-m", "base")
	base := strings.TrimSpace(ownersGit(t, repo, "rev-parse", "HEAD"))
	writeParityFile(t, filepath.Join(root, "packages", "flipped"), "putnami.json", `{"name":"flipped",`+none+`}`)
	writeParityFile(t, filepath.Join(root, "packages", "fresh"), "putnami.json", `{"name":"fresh",`+none+`}`)
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
	owners := nativeRequirementOwners(root)
	got, err := owners(base, []string{"ws/packages/proto/schema.go", "ws/packages/fresh/putnami.json"})
	if err != nil || len(got.Projects) != 0 || len(got.Unowned) != 0 {
		t.Fatalf("featureless change has requirement owners %+v: %v", got, err)
	}
	changed := []string{"outside.txt", "ws/packages/proto/schema.go"}
	for name := range projects {
		changed = append(changed, "ws/packages/"+name+"/source.go")
	}
	got, err = owners(base, changed)
	if err != nil || !slices.Equal(got.Projects, []string{"/packages/app", "/packages/authored", "/packages/blank", "/packages/flipped", "/packages/part"}) ||
		!slices.Equal(got.Unowned, []string{"outside.txt"}) {
		t.Fatalf("requirement owners %+v: %v", got, err)
	}
	if _, err := owners("-bad", changed); err == nil {
		t.Fatal("invalid base revision accepted")
	}
}

func ownersGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}
