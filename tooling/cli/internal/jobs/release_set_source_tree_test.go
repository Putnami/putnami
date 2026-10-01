package jobs

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/workspace"
	ciproto "go.putnami.dev/protocol/ci"
	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
	putnamigit "go.putnami.dev/tooling/cli/internal/git"
)

// sourceTreePolicy is the putnami.ci.json distribution section of a repository
// that opted into recording each member's source tree (protocols/distribution ADR 0005).
func sourceTreePolicy() *ciproto.Distribution {
	return &ciproto.Distribution{Namespace: "putnami", MemberSourceTree: true}
}

// TestReleaseSetSourceTreeIsOffUntilTheRepositoryOptsIn pins the rollout
// contract of protocols/distribution ADR 0005: without `distribution.memberSourceTree`, neither the
// plan a publishing extension receives nor the set the provider releases
// carries the field, so a reader built before it decodes both strictly.
func TestReleaseSetSourceTreeIsOffUntilTheRepositoryOptsIn(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-source-tree-opt-in", "default-plan-and-set-carry-no-source-tree")
	for _, policy := range []*ciproto.Distribution{nil, {Namespace: "putnami"}, {Namespace: "putnami", MemberAttribution: true}} {
		ws := attributionWorkspace(t)
		useReleaseSetProvenance(t, nil)
		options := releaseSetAllOptions()
		options.Policy = policy
		run := prepareReleaseSetWithHead(t, options, ws, emptyHead("canary"))
		planJSON, err := json.Marshal(releaseset.ClonePlan(run.plan))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(planJSON), `"sourceTree"`) {
			t.Fatalf("policy %+v: the plan carries a source tree without the opt-in:\n%s", policy, planJSON)
		}
		final, err := reconcilePublishedReleaseSet(run.plan, publishedResults(run.plan, digestFor('d')))
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		setJSON, err := json.Marshal(final)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(setJSON), `"sourceTree"`) {
			t.Fatalf("policy %+v: the released set carries a source tree without the opt-in:\n%s", policy, setJSON)
		}
		if policy == nil || !policy.MemberAttribution {
			var legacySet legacyReleaseSet
			if err := decodeStrictly(t, setJSON, &legacySet); err != nil {
				t.Fatalf("policy %+v: a pre-source-tree provider refuses the default released set: %v\n%s", policy, err, setJSON)
			}
		}
	}
}

// TestReleaseSetSourceTreeFollowsTheRepositoryOptIn is the other half: once
// the repository declares memberSourceTree, every republished member records
// the tree the provenance read, and the released set carries it.
func TestReleaseSetSourceTreeFollowsTheRepositoryOptIn(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-source-tree-opt-in", "opted-in-plan-records-the-head-tree-on-republished-members")
	ws := attributionWorkspace(t)
	useReleaseSetProvenance(t, nil)
	options := releaseSetAllOptions()
	options.Policy = sourceTreePolicy()
	run := prepareReleaseSetWithHead(t, options, ws, emptyHead("canary"))
	for _, member := range run.plan.Members {
		if !member.Selected || member.SourceTree != testTree {
			t.Fatalf("planned member %+v; want it republished from tree %s", member, testTree)
		}
		if member.Project != "" || member.Kind != "" {
			t.Fatalf("planned member %+v carries attribution the policy did not opt into", member)
		}
	}
	final, err := reconcilePublishedReleaseSet(run.plan, publishedResults(run.plan, digestFor('d')))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, member := range final.Members {
		if member.SourceTree != testTree {
			t.Fatalf("released member %+v dropped the source tree", member)
		}
	}
}

// TestReleaseSetSourceTreeIsInheritedVerbatim pins that the tree belongs to
// the publication that built the artifact: an unchanged member keeps its head's
// tree whether or not the repository still opts in, and only a republished
// member records the tree of this checkout.
func TestReleaseSetSourceTreeIsInheritedVerbatim(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-source-tree-opt-in", "inherited-member-keeps-head-source-tree")
	ws := attributionWorkspace(t)
	useReleaseSetProvenance(t, nil)
	first := releaseSetAllOptions()
	first.Policy = sourceTreePolicy()
	run := prepareReleaseSetWithHead(t, first, ws, emptyHead("canary"))
	final, err := reconcilePublishedReleaseSet(run.plan, publishedResults(run.plan, digestFor('d')))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// The head's library was built from an older tree than this checkout.
	const olderTree = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	for index := range final.Members {
		if final.Members[index].Ecosystem == "npm" {
			final.Members[index].SourceTree = olderTree
		}
	}
	head := &distribution.ChannelHead{Generation: 1, ReleaseSet: final}
	refreshHeadRef(t, head)

	// Only the service tree moved.
	useReleaseSetProvenance(t, map[string]string{"/apps/service": digestFor('9')})
	for _, policy := range []*ciproto.Distribution{sourceTreePolicy(), {Namespace: "putnami"}} {
		options := releaseSetRequest()
		options.Policy = policy
		second := prepareReleaseSetWithHead(t, options, ws, map[string]*distribution.ChannelHead{"canary": head})
		library, ok := second.plan.Member(distribution.Ecosystem("npm"), "@putnami/web")
		if !ok || library.Selected || library.SourceTree != olderTree {
			t.Fatalf("policy %+v: inherited library = %+v, %v; want the head's tree kept verbatim", policy, library, ok)
		}
		image, ok := second.plan.Member(distribution.Ecosystem("oci"), "putnami/service")
		want := ""
		if policy.MemberSourceTree {
			want = testTree
		}
		if !ok || !image.Selected || image.SourceTree != want {
			t.Fatalf("policy %+v: republished image = %+v, %v; want tree %q", policy, image, ok, want)
		}
	}
}

// TestReleaseSetCommitDropsATreeTheCheckoutNoLongerHolds pins the second read:
// a task that ran after planning can rewrite a tracked file, and the artifacts
// are then built from content the planned tree does not name. The commit reads
// the tree again and, when it moved, releases the republished members without
// one, while an inherited member keeps its head's tree.
func TestReleaseSetCommitDropsATreeTheCheckoutNoLongerHolds(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-source-tree-opt-in", "commit-drops-a-tree-the-checkout-no-longer-holds")
	ws := attributionWorkspace(t)
	useReleaseSetProvenance(t, nil)
	first := releaseSetAllOptions()
	first.Policy = sourceTreePolicy()
	run := prepareReleaseSetWithHead(t, first, ws, emptyHead("canary"))
	final, err := reconcilePublishedReleaseSet(run.plan, publishedResults(run.plan, digestFor('d')))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	head := &distribution.ChannelHead{Generation: 1, ReleaseSet: final}
	refreshHeadRef(t, head)

	useReleaseSetProvenance(t, map[string]string{"/apps/service": digestFor('9')})
	for _, moved := range []bool{false, true} {
		if moved {
			currentReleaseSourceTree = func(string) string { return "" }
		}
		provider := &fakeReleaseSetProvider{heads: map[string]*distribution.ChannelHead{"canary": head}}
		useReleaseSetProviderFake(t, provider)
		options := releaseSetRequest()
		options.Policy = sourceTreePolicy()
		second, err := prepareReleaseSet(t, options, ws, ws.Projects, providerDiscovery())
		if err != nil {
			t.Fatal(err)
		}
		results := publishedResults(second.plan, digestFor('c'))
		second.Finalizer(context.Background())(results)
		if result := results[releaseSetResultKey]; result == nil || result.Status != "success" || provider.releaseRequest == nil {
			t.Fatalf("checkout moved %v: release = %+v", moved, result)
		}
		for _, member := range provider.releaseRequest.ReleaseSet.Members {
			want := testTree
			if moved && member.Ecosystem == "oci" {
				want = ""
			}
			if member.SourceTree != want {
				t.Fatalf("checkout moved %v: released member %s has tree %q, want %q", moved, member.Coordinate, member.SourceTree, want)
			}
		}
	}
}

// TestReleaseSetProvenanceReadsTheHeadTree is the git half, on a real
// checkout: asked for a tree, the provenance records HEAD's, including when
// PUTNAMI_SOURCE_REVISION binds the publication to another commit, and records
// none on a dirty checkout or outside git, without failing. It sets the
// environment, so it is not parallel.
func TestReleaseSetProvenanceReadsTheHeadTree(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-source-tree-opt-in", "clean-checkout-records-head-tree-whatever-the-bound-revision")
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "index.ts"), []byte("export const a = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "init")
	tree := git("rev-parse", "HEAD^{tree}")
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "putnami"}, nil)

	t.Setenv(putnamigit.SourceRevisionEnv, "")
	provenance, err := realReleaseSetProvenance(ws, true)
	if err != nil || provenance.tree != tree {
		t.Fatalf("provenance = %+v, %v; want HEAD's tree %s", provenance, err, tree)
	}
	if provenance, err := realReleaseSetProvenance(ws, false); err != nil || provenance.tree != "" {
		t.Fatalf("provenance without the opt-in = %+v, %v; want no tree", provenance, err)
	}

	// A runner binds the publication to another commit: the revision follows
	// the binding, the tree stays the one the checkout holds.
	t.Setenv(putnamigit.SourceRevisionEnv, testRevision)
	t.Setenv(putnamigit.SourceCommitTimeEnv, "1767323045")
	provenance, err = realReleaseSetProvenance(ws, true)
	if err != nil || provenance.revision != testRevision || provenance.tree != tree {
		t.Fatalf("provenance with a bound revision = %+v, %v; want revision %s and HEAD's tree %s", provenance, err, testRevision, tree)
	}

	// HEAD's tree does not describe a dirty checkout: no tree, no failure.
	if err := os.WriteFile(filepath.Join(root, "untracked.ts"), []byte("export const b = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if provenance, err := realReleaseSetProvenance(ws, true); err != nil || provenance.tree != "" {
		t.Fatalf("provenance on a dirty checkout = %+v, %v; want no tree", provenance, err)
	}

	// Outside git, a bound revision still publishes, without a tree.
	bare := workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "putnami"}, nil)
	if provenance, err := realReleaseSetProvenance(bare, true); err != nil || provenance.tree != "" {
		t.Fatalf("provenance outside git = %+v, %v; want the bound revision without a tree", provenance, err)
	}
}
