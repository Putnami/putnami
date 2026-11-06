package finalize

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/gittest"
	"go.putnami.dev/sdk/extension/treefingerprint"
)

func TestRequirementlessRepositoryIsTheSameOnAForeignMachine(t *testing.T) {
	gittest.AssertMachineIndependent(t, func() string {
		root := t.TempDir()
		requirementlessRepository(t, root, nil)
		return root
	})
}

// requirementlessRepository is an ADR-only change in a real Git repository: a
// base commit, then a branch commit that adds an ADR and a decisions.json and
// carries no feature requirement. Its workflow names no feature source and one
// unit with no requirement, at stage candidate. extra adds files to the change.
func requirementlessRepository(t *testing.T, root string, extra map[string]string) (base, head string, paths []string) {
	t.Helper()
	run := func(args ...string) string {
		t.Helper()
		command := gittest.Command(root, args...)
		command.Env = gittest.Environ(os.Environ())
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, content string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ref := func(name, content string) map[string]any {
		write(name, content)
		sum := sha256.Sum256([]byte(content))
		return map[string]any{"path": name, "sha256": hex.EncodeToString(sum[:])}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.name", "Finalize Fixture")
	run("config", "user.email", "fixture@example.invalid")
	run("config", "commit.gpgSign", "false")
	run("config", "core.hooksPath", filepath.Join(root, ".git", "no-hooks"))
	write(".gitignore", ".context/\nbody.txt\n")
	write("cli/putnami.json", `{"name":"cli"}`+"\n")
	run("add", ".")
	run("commit", "-q", "-m", "base")
	base = run("rev-parse", "HEAD")
	run("checkout", "-q", "-b", "topic")
	paths = make([]string, 0, 2+len(extra))
	paths = append(paths, "cli/doc/adr/0001-a-settled-choice.md", "decisions.json")
	write(paths[0], "# ADR 0001\n")
	write(paths[1], `{"decisions":[]}`+"\n")
	for name, content := range extra {
		write(name, content)
		paths = append(paths, name)
	}
	run("add", ".")
	run("commit", "-q", "-m", "docs: settle a choice")
	head = run("rev-parse", "HEAD")

	mandate := ref(".context/mandate.md", "Deliver the whole task.\n")
	reason := ref(".context/stage-reason.md", "A settled choice with no requirement.\n")
	names := []string{"intent", "spec", "design", "prototype", "review", "adr", "candidate", "handoff"}
	stages := make([]any, 0, len(names))
	for _, name := range names {
		stage := map[string]any{"unit": "adr", "name": name, "state": "not-applicable",
			"entryCondition": "The unit has an owner and exact inputs.", "inputs": []any{mandate},
			"expectedOutput": name + " decision", "exitCriteria": "The unit is inspectable", "reason": reason}
		if name == "candidate" {
			stage["state"] = "passed"
			delete(stage, "reason")
			stage["receipt"] = ref(".context/candidate.md", "Candidate reviewed.\n")
		}
		stages = append(stages, stage)
	}
	workflow := map[string]any{
		"version": 1, "outcome": "A settled choice is recorded", "mandate": mandate,
		"sources": []any{}, "requirements": []any{},
		"units": []any{map[string]any{"id": "adr", "outcome": "The ADR and its decision are recorded",
			"boundary": ref(".context/boundary.md", "The ADR and decisions.json only.\n"), "exclusions": []any{},
			"projects":     []any{map[string]any{"id": "/cli", "manifest": ref("cli/putnami.json", `{"name":"cli"}`+"\n")}},
			"requirements": []any{}, "dependsOn": []any{}, "requiredStage": "ready", "stage": "candidate",
			"nextResult": "Merge", "nextCycleTasks": []any{}, "decisions": []any{}}},
		"artifacts": []any{}, "experiments": []any{}, "stages": stages,
		"handoff": map[string]any{"mandate": mandate, "unit": "adr", "scopes": []any{"/cli"},
			"receiver": "reviewer", "responsibility": "Inspect the candidate", "inputs": []any{mandate},
			"expectedOutput": "A review", "acceptance": "The ADR reads well", "nextAction": "Merge",
			"unresolvedFindings": []any{}},
	}
	data, err := json.Marshal(workflow)
	if err != nil {
		t.Fatal(err)
	}
	write(".context/workflow.json", string(data))
	return base, head, paths
}

func requirementlessFinalizer(t *testing.T, extra map[string]string) (Finalizer, *finalizerGit, *finalizerProvider, Options) {
	t.Helper()
	f, git, provider, opts := finalizerFixture(t)
	base, head, paths := requirementlessRepository(t, f.Root, extra)
	tree, err := treefingerprint.Compute(f.Root)
	if err != nil {
		t.Fatal(err)
	}
	git.head, git.baseSHA, git.fingerprint, git.paths = head, base, tree.Fingerprint, paths
	f.Workflow = HostedWorkflowVerify(f.Root, nil)
	opts.Title = "docs: settle a choice"
	opts.Gate, opts.VerificationFile, opts.WorkflowFile = "ci", "", ".context/workflow.json"
	opts.TaskSource, opts.TaskID = provider.task.Ref.Source, provider.task.Ref.ID
	provider.checks = collab.Checks{State: collab.ChecksStatePassing, Commit: head,
		Items: []collab.Check{{Name: "Putnami CI", State: collab.CheckStatePassing}}}
	return f, git, provider, opts
}

func TestAHostedDeliveryWithNoRequirementClosesItsTask(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "requirementless-delivery", "a-requirementless-delivery-closes-its-task")
	f, git, provider, opts := requirementlessFinalizer(t, nil)
	result, err := f.Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("a complete delivery with no requirement was refused: %v", err)
	}
	if git.pushes != 1 || provider.transitions != 1 || result.State != collab.ProposalStateOpen ||
		len(result.ReadyUnits) != 1 || result.ReadyUnits[0] != "adr" || result.HostedWorkflowReceipt == nil {
		t.Fatalf("delivery did not complete its task: result=%+v pushes=%d transitions=%d", result, git.pushes, provider.transitions)
	}
	if !strings.Contains(provider.proposal.Body, "Closes ") {
		t.Fatalf("complete delivery lost its closing reference: %s", provider.proposal.Body)
	}
}

func TestAHostedDeliveryWithNoRequirementRefusesASpecEdit(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "requirementless-delivery", "a-spec-or-manifest-edit-refuses-a-requirementless-unit")
	for name, extra := range map[string]map[string]string{
		"cli/specs/export.json":     {"cli/specs/export.json": `{"protocolVersion":1}` + "\n"},
		"cli/putnami.features.json": {"cli/putnami.features.json": `{"protocolVersion":2}` + "\n"},
	} {
		t.Run(name, func(t *testing.T) {
			f, git, provider, opts := requirementlessFinalizer(t, extra)
			_, err := f.Run(context.Background(), opts)
			if err == nil || !strings.Contains(err.Error(), "workflow unit adr carries no requirement but the change edits feature intent: "+name) ||
				git.pushes != 0 || provider.upserts != 0 || provider.transitions != 0 {
				t.Fatalf("a unit with no requirement published a spec edit: %v (pushes=%d upserts=%d)", err, git.pushes, provider.upserts)
			}
		})
	}
}
