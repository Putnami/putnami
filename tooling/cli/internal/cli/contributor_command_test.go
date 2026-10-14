package cli

import (
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
	root := t.TempDir()
	writeParityFile(t, root, wsproto.WorkspaceConfigFilename, `{"name":"owners","includes":["packages/proto","packages/app","packages/part"]}`)
	writeParityFile(t, filepath.Join(root, "packages", "proto"), "putnami.json",
		`{"name":"proto","type":"library","featureAuthority":{"none":"A wire contract, not a user-facing feature."}}`)
	writeParityFile(t, filepath.Join(root, "packages", "app"), "putnami.json", `{"name":"app","type":"application"}`)
	writeParityFile(t, filepath.Join(root, "packages", "part"), "putnami.json",
		`{"name":"part","type":"library","featureAuthority":{"owner":"app/feature"}}`)
	workspace.InvalidateLoadCache(root)
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.RefreshSnapshot(ws, workspace.SnapshotWritePolicy{}); err != nil {
		t.Fatal(err)
	}
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
	owners := nativeRequirementOwners(root)
	got, err := owners([]string{"packages/proto/schema.go", "packages/proto/doc/README.md"})
	if err != nil || len(got) != 0 {
		t.Fatalf("featureless change has requirement owners %v: %v", got, err)
	}
	got, err = owners([]string{"packages/proto/schema.go", "packages/app/main.go", "packages/part/lib.go", "README.md"})
	if err != nil || !slices.Equal(got, []string{"/packages/app", "/packages/part", "README.md"}) {
		t.Fatalf("requirement owners %v: %v", got, err)
	}
}
