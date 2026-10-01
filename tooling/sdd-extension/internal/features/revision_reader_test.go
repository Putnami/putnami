package features

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	featureproto "go.putnami.dev/protocol/features"
	internalgit "go.putnami.dev/tooling/sdd/extension/internal/gitread"
)

func TestEvaluateGitRevisionLoadsEachTreesOwnMembershipWithoutWorktreeMutation(t *testing.T) {
	repo := initFeatureGitRepo(t)
	writeRevisionFile(t, repo, "putnami.workspace.json", `{"name":"history","includes":["old","keep"]}`)
	writeRevisionFile(t, repo, "keep/putnami.json", `{"name":"keep"}`)
	writeRevisionFile(t, repo, "keep/putnami.features.json", string(mustFeatureManifest(t, modeledRevisionManifest("keep/stable"))))
	writeRevisionFile(t, repo, "old/putnami.json", `{"name":"old"}`)
	writeRevisionFile(t, repo, "old/putnami.features.json", string(mustFeatureManifest(t, modeledRevisionManifest("old/legacy"))))
	commitFeatureRepo(t, repo, "base membership")
	base := strings.TrimSpace(runFeatureGit(t, repo, "rev-parse", "HEAD"))

	if err := os.RemoveAll(filepath.Join(repo, "old")); err != nil {
		t.Fatal(err)
	}
	writeRevisionFile(t, repo, "putnami.workspace.json", `{"name":"history","includes":["renamed","keep"]}`)
	writeRevisionFile(t, repo, "renamed/putnami.json", `{"name":"renamed"}`)
	writeRevisionFile(t, repo, "renamed/putnami.features.json", string(mustFeatureManifest(t, modeledRevisionManifest("renamed/next"))))
	commitFeatureRepo(t, repo, "rename project")
	head := strings.TrimSpace(runFeatureGit(t, repo, "rev-parse", "HEAD"))

	writeRevisionFile(t, repo, "putnami.workspace.json", `{"name":"dirty-secret","includes":[]}`)
	writeRevisionFile(t, repo, "untracked-secret.txt", "customer-secret-value")
	beforeStatus := runFeatureGit(t, repo, "status", "--porcelain=v1", "--untracked-files=all")
	beforeHead := strings.TrimSpace(runFeatureGit(t, repo, "rev-parse", "HEAD"))

	baseEvaluation, err := EvaluateGitRevision(repo, base)
	if err != nil {
		t.Fatalf("evaluate base: %v", err)
	}
	headEvaluation, err := EvaluateGitRevision(repo, head)
	if err != nil {
		t.Fatalf("evaluate head: %v", err)
	}
	if baseEvaluation.Result.Snapshot == nil || headEvaluation.Result.Snapshot == nil {
		t.Fatalf("revision snapshots unavailable: base=%+v head=%+v", baseEvaluation.Result.Diagnostics, headEvaluation.Result.Diagnostics)
	}
	if got := revisionFeatureIDs(baseEvaluation.Result.Snapshot); strings.Join(got, ",") != "keep/stable,old/legacy" {
		t.Fatalf("base feature IDs = %v", got)
	}
	if got := revisionFeatureIDs(headEvaluation.Result.Snapshot); strings.Join(got, ",") != "keep/stable,renamed/next" {
		t.Fatalf("head feature IDs = %v", got)
	}
	if baseEvaluation.Revision.Commit != "git:"+base || headEvaluation.Revision.Commit != "git:"+head {
		t.Fatalf("resolved revisions = %+v / %+v", baseEvaluation.Revision, headEvaluation.Revision)
	}
	snapshotBytes, err := MarshalSnapshot(headEvaluation.Result.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"dirty-secret", "customer-secret-value", repo} {
		if bytes.Contains(snapshotBytes, []byte(forbidden)) {
			t.Errorf("historical snapshot leaked current-worktree content %q:\n%s", forbidden, snapshotBytes)
		}
	}
	if after := runFeatureGit(t, repo, "status", "--porcelain=v1", "--untracked-files=all"); after != beforeStatus {
		t.Fatalf("revision evaluation mutated worktree status:\nbefore=%q\nafter=%q", beforeStatus, after)
	}
	if after := strings.TrimSpace(runFeatureGit(t, repo, "rev-parse", "HEAD")); after != beforeHead {
		t.Fatalf("revision evaluation moved HEAD: %s -> %s", beforeHead, after)
	}
}

func TestEvaluateGitRevisionRecomputesEvidenceFreshnessPerTree(t *testing.T) {
	repo := initFeatureGitRepo(t)
	writeRevisionFile(t, repo, "putnami.workspace.json", `{"name":"evidence-history","includes":["app"]}`)
	writeRevisionFile(t, repo, "app/putnami.json", `{"name":"app"}`)
	manifest := featureproto.Manifest{
		ProtocolVersion: featureproto.MinimumManifestProtocolVersion,
		Namespace:       "app",
		Features: []featureproto.Feature{{
			ID: "app/change", Type: featureproto.FeatureTypeFeature, Name: "Change", Outcome: "Evidence changes are visible", Owner: "product",
			Target:       featureproto.MaturityCoded,
			Requirements: []featureproto.Requirement{{ID: "implementation", Stage: featureproto.MaturityCoded, EvidenceKinds: []featureproto.EvidenceKind{featureproto.EvidenceKindArtifact}}},
		}},
	}
	writeRevisionFile(t, repo, "app/putnami.features.json", string(mustFeatureManifest(t, manifest)))
	writeRevisionFile(t, repo, "app/source.txt", "base source\n")
	writeRevisionEvidence(t, repo, sourceBinding('0'), sha256Digest([]byte("base source\n")), featureproto.EvidenceOutcomeSupports)
	commitFeatureRepo(t, repo, "seed evidence")

	baseBinding, err := internalgit.CommitSourceBinding(repo, "HEAD", "app")
	if err != nil {
		t.Fatal(err)
	}
	writeRevisionEvidence(t, repo, baseBinding, sha256Digest([]byte("base source\n")), featureproto.EvidenceOutcomeSupports)
	commitFeatureRepo(t, repo, "current support")
	base := strings.TrimSpace(runFeatureGit(t, repo, "rev-parse", "HEAD"))

	writeRevisionFile(t, repo, "app/source.txt", "changed source\n")
	commitFeatureRepo(t, repo, "stale support")
	stale := strings.TrimSpace(runFeatureGit(t, repo, "rev-parse", "HEAD"))

	baseEvaluation, err := EvaluateGitRevision(repo, base)
	if err != nil {
		t.Fatal(err)
	}
	staleEvaluation, err := EvaluateGitRevision(repo, stale)
	if err != nil {
		t.Fatal(err)
	}
	baseRequirement := baseEvaluation.Result.Snapshot.Features[0].Requirements[0]
	staleRequirement := staleEvaluation.Result.Snapshot.Features[0].Requirements[0]
	if baseRequirement.State != VerificationVerified || staleRequirement.State != VerificationStale {
		t.Fatalf("historical requirement states = %s -> %s", baseRequirement.State, staleRequirement.State)
	}
	staleDelta := CompareSnapshots(baseEvaluation.Result.Snapshot, staleEvaluation.Result.Snapshot)
	if len(staleDelta.Regressed) != 1 || len(staleDelta.Stale) != 1 {
		t.Fatalf("stale delta = %+v", staleDelta)
	}

	headBinding, err := internalgit.CommitSourceBinding(repo, stale, "app")
	if err != nil {
		t.Fatal(err)
	}
	writeRevisionEvidence(t, repo, headBinding, sha256Digest([]byte("changed source\n")), featureproto.EvidenceOutcomeContradicts)
	commitFeatureRepo(t, repo, "active contradiction")
	contradicted := strings.TrimSpace(runFeatureGit(t, repo, "rev-parse", "HEAD"))
	contradictedEvaluation, err := EvaluateGitRevision(repo, contradicted)
	if err != nil {
		t.Fatal(err)
	}
	contradictedRequirement := contradictedEvaluation.Result.Snapshot.Features[0].Requirements[0]
	if contradictedRequirement.State != VerificationContradicted {
		t.Fatalf("contradicted requirement = %+v", contradictedRequirement)
	}
	contradictedDelta := CompareSnapshots(baseEvaluation.Result.Snapshot, contradictedEvaluation.Result.Snapshot)
	if len(contradictedDelta.Regressed) != 1 || len(contradictedDelta.Contradicted) != 1 {
		t.Fatalf("contradicted delta = %+v", contradictedDelta)
	}
}

func TestGitTreeReaderFollowsOnlyContainedSymlinks(t *testing.T) {
	repo := initFeatureGitRepo(t)
	writeRevisionFile(t, repo, "app/inside.json", "inside")
	writeRevisionFile(t, repo, "outside.json", "outside")
	if err := os.Symlink("inside.json", filepath.Join(repo, "app", "inside-link.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside.json", filepath.Join(repo, "app", "outside-link.json")); err != nil {
		t.Fatal(err)
	}
	commitFeatureRepo(t, repo, "symlinks")
	reader, err := NewGitTreeReader(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	data, err := reader.ReadFile("app", "inside-link.json")
	if err != nil || string(data) != "inside" {
		t.Fatalf("contained symlink = %q, %v", data, err)
	}
	if _, err := reader.ReadFile("app", "outside-link.json"); kindOfReaderError(err) != ReaderErrorSymlinkEscape {
		t.Fatalf("escaping symlink error = %v", err)
	}
	if _, err := reader.ReadFile("app", "../outside.json"); kindOfReaderError(err) != ReaderErrorPathEscape {
		t.Fatalf("escaping relative path error = %v", err)
	}
}

func modeledRevisionManifest(id string) featureproto.Manifest {
	namespace, _, _ := strings.Cut(id, "/")
	return featureproto.Manifest{
		ProtocolVersion: featureproto.MinimumManifestProtocolVersion,
		Namespace:       namespace,
		Features: []featureproto.Feature{{
			ID: id, Type: featureproto.FeatureTypeFeature, Name: pathLeaf(id), Outcome: "Visible at its own revision", Owner: namespace, Target: featureproto.MaturityModeled,
		}},
	}
}

func writeRevisionEvidence(t *testing.T, repo, binding, digest string, outcome featureproto.EvidenceOutcome) {
	t.Helper()
	document := featureproto.EvidenceDocument{
		ProtocolVersion: featureproto.EvidenceProtocolVersion,
		Evidence: []featureproto.EvidenceRecord{{
			ID: "app/change-implementation", Feature: "app/change", Requirement: "implementation", Stage: featureproto.MaturityCoded, Outcome: outcome,
			Issuer:     featureproto.Issuer{Kind: featureproto.IssuerKindBuild, ID: "fixture"},
			Source:     featureproto.SourceSelector{Root: featureproto.LocationRootProject, OwnerProject: "app", Binding: binding},
			Subject:    featureproto.EvidenceSubject{Kind: featureproto.EvidenceKindArtifact, Artifact: &featureproto.ArtifactSubject{Path: "source.txt", Digest: digest}},
			Provenance: featureproto.EvidenceProvenance{Root: featureproto.LocationRootProject, Path: "evidence.go"},
		}},
	}
	writeRevisionFile(t, repo, "app/schema/feature-evidence/build.json", string(mustEvidence(t, document)))
}

func revisionFeatureIDs(snapshot *Snapshot) []string {
	ids := make([]string, len(snapshot.Features))
	for index, feature := range snapshot.Features {
		ids[index] = feature.ID
	}
	return ids
}

func pathLeaf(value string) string {
	_, leaf, found := strings.Cut(value, "/")
	if found {
		return leaf
	}
	return value
}

func initFeatureGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "test@test.com"}, {"config", "user.name", "Test"}, {"config", "commit.gpgsign", "false"}} {
		runFeatureGit(t, repo, args...)
	}
	return repo
}

func commitFeatureRepo(t *testing.T, repo, message string) {
	t.Helper()
	runFeatureGit(t, repo, "add", "-A")
	runFeatureGit(t, repo, "commit", "-q", "-m", message)
}

func runFeatureGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = repo
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func writeRevisionFile(t *testing.T, repo, relative, contents string) {
	t.Helper()
	filename := filepath.Join(repo, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
