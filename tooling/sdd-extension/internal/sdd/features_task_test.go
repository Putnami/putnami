package sdd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	internalgit "go.putnami.dev/tooling/sdd/extension/internal/gitread"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// featureTaskWorkspace is a Git work tree with one project whose manifest
// declares two coded requirements, and evidence for each bound to the
// project's current source: one selected by project, one by package name and
// the tree base version. Both are verified until the project's source moves.
func featureTaskWorkspace(t *testing.T) string {
	t.Helper()
	root := candidateWorkspace(t)
	for _, args := range [][]string{
		{"config", "user.email", "t@t.com"}, {"config", "user.name", "T"}, {"config", "commit.gpgsign", "false"},
	} {
		gitIn(t, root, args...)
	}
	writeFixtureFile(t, filepath.Join(root, ".gitignore"), "billing/schema/feature-evidence/local.json\nbilling/out/\n")
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"gate","includes":["billing"]}`)
	writeFixtureFile(t, filepath.Join(root, "billing", "putnami.json"), `{"name":"@acme/billing","type":"library"}`)
	writeFixtureFile(t, filepath.Join(root, "billing", "tool.sh"), "#!/bin/sh\n")
	manifest := featureproto.Manifest{
		ProtocolVersion: featureproto.MinimumManifestProtocolVersion,
		Namespace:       "billing",
		Features: []featureproto.Feature{{
			ID: "billing/export", Type: featureproto.FeatureTypeFeature, Name: "Export", Outcome: "Invoices can be exported", Owner: "billing", Target: featureproto.MaturityCoded,
			Requirements: []featureproto.Requirement{
				{ID: "implementation", Stage: featureproto.MaturityCoded, EvidenceKinds: []featureproto.EvidenceKind{featureproto.EvidenceKindAttestation}},
				{ID: "packaged", Stage: featureproto.MaturityCoded, EvidenceKinds: []featureproto.EvidenceKind{featureproto.EvidenceKindAttestation}},
			},
		}},
	}
	data, err := featureproto.MarshalManifest(&manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(root, "billing", featureproto.ManifestFilename), string(data))
	binding, err := internalgit.ProjectSourceBinding(root, filepath.Join(root, "billing"))
	if err != nil {
		t.Fatal(err)
	}
	writeFeatureEvidence(t, filepath.Join(root, "billing", featureproto.EvidenceDirectory, "proof.json"),
		attestation("billing/implementation-proof", "implementation", featureproto.SourceSelector{
			Root: featureproto.LocationRootProject, OwnerProject: "@acme/billing", Binding: binding,
		}),
		attestation("billing/packaged-proof", "packaged", featureproto.SourceSelector{
			Root: featureproto.LocationRootPackage, Package: "@acme/billing", Version: treeBaseVersion, Binding: binding,
		}))
	return root
}

func attestation(id, requirement string, source featureproto.SourceSelector) featureproto.EvidenceRecord {
	return featureproto.EvidenceRecord{
		ID: id, Feature: "billing/export", Requirement: requirement, Stage: featureproto.MaturityCoded, Outcome: featureproto.EvidenceOutcomeSupports,
		Issuer:     featureproto.Issuer{Kind: featureproto.IssuerKindTest, ID: "evidence-producer"},
		Source:     source,
		Subject:    featureproto.EvidenceSubject{Kind: featureproto.EvidenceKindAttestation, Attestation: &featureproto.AttestationSubject{Claim: "proof"}},
		Provenance: featureproto.EvidenceProvenance{Root: source.Root, Path: "tool.sh"},
	}
}

func writeFeatureEvidence(t *testing.T, path string, records ...featureproto.EvidenceRecord) {
	t.Helper()
	data, err := featureproto.MarshalEvidenceDocument(&featureproto.EvidenceDocument{ProtocolVersion: featureproto.EvidenceProtocolVersion, Evidence: records})
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, path, string(data))
}

// featureTaskReport runs the cached `validate` step's builder for the billing
// project with the version the wire gives it, and returns the report it
// writes.
func featureTaskReport(t *testing.T, root, version string) (string, FeatureValidationReport) {
	t.Helper()
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{{ID: "/billing", Name: "@acme/billing", Path: "billing", Version: version}})
	report, _ := BuildFeatureTaskValidationResult(ws, Selection{Mode: pctx.SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/billing"}})
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), report
}

// staleEvidence names the evidence the report finds stale, or "verified".
func staleEvidence(report FeatureValidationReport) string {
	var stale []string
	for _, finding := range report.Diagnostics {
		if finding.Code == featureproto.WarningCodeStaleEvidence || finding.Severity == diag.Error {
			stale = append(stale, finding.Code+" "+finding.Field)
		}
	}
	if len(stale) == 0 {
		return "verified"
	}
	return strings.Join(stale, "; ")
}

func makeExecutableIn(t *testing.T, root, rel string, executable bool) {
	t.Helper()
	if runtime.GOOS == "windows" {
		// Windows stores no executable bit: a tracked file takes it from the
		// index, for the key and the source binding alike.
		flag := "--chmod=-x"
		if executable {
			flag = "--chmod=+x"
		}
		gitIn(t, root, "update-index", flag, rel)
		return
	}
	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	if err := os.Chmod(filepath.Join(root, filepath.FromSlash(rel)), mode); err != nil {
		t.Fatal(err)
	}
}

// The cached `validate` step keys on `git:**`. Its whole report moves only
// with the candidate cut that key holds: an ignored evidence fragment and the
// index state are invisible to both, and a chmod +x of a bound file, which a
// source binding records, moves both, and moves both back.
func TestFeatureTaskVerdictFollowsTheCandidateCut(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "declared-inputs-cover-the-read-set", "the-cache-policy-matches-the-declared-read-set")
	root := featureTaskWorkspace(t)
	verdict := func() (string, string) {
		data, report := featureTaskReport(t, root, "1.2.3")
		return data, staleEvidence(report)
	}
	key := cutFingerprint(t, root)
	report, state := verdict()
	if state != "verified" {
		t.Fatalf("initial verdict = %s, want every requirement verified\n%s", state, report)
	}
	keys := map[string]string{key: report}
	for _, step := range []struct {
		name  string
		apply func()
		moves bool
		stale bool
	}{
		{"an ignored evidence fragment appears", func() {
			writeFixtureFile(t, filepath.Join(root, "billing", featureproto.EvidenceDirectory, "local.json"), "{ not evidence")
		}, false, false},
		{"ignored build output appears", func() {
			writeFixtureFile(t, filepath.Join(root, "billing", "out", "tool.sh"), "#!/bin/sh\nexit 1\n")
		}, false, false},
		{"the cut is staged and committed", func() {
			gitIn(t, root, "add", "-A")
			gitIn(t, root, "commit", "-q", "-m", "fixture")
		}, false, false},
		{"a bound file becomes executable", func() { makeExecutableIn(t, root, "billing/tool.sh", true) }, true, true},
		{"the bound file is no longer executable", func() { makeExecutableIn(t, root, "billing/tool.sh", false) }, true, false},
		{"a bound file changes", func() {
			writeFixtureFile(t, filepath.Join(root, "billing", "tool.sh"), "#!/bin/sh\nexit 0\n")
		}, true, true},
	} {
		step.apply()
		nextKey := cutFingerprint(t, root)
		next, nextState := verdict()
		if moved := nextKey != key; moved != step.moves {
			t.Errorf("%s: key moved = %v, want %v", step.name, moved, step.moves)
		}
		if previous, seen := keys[nextKey]; seen && previous != next {
			t.Errorf("%s: one key, two reports:\n%s\n%s", step.name, previous, next)
		}
		if stale := nextState != "verified"; stale != step.stale {
			t.Errorf("%s: verdict = %s, want stale = %v", step.name, nextState, step.stale)
		}
		keys[nextKey] = next
		key = nextKey
	}
}

// The report is a function of the tree alone. Neither the version the wire
// gives the project, which the CLI gives a cacheable task as 0.0.0 or, where
// Git has no commit, not at all, nor the commit HEAD names moves a byte of
// it, and it names no commit a replayed entry would report for another one.
func TestFeatureTaskReportNamesNoCommitAndReadsNoVersion(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "declared-inputs-cover-the-read-set", "the-cache-policy-matches-the-declared-read-set")
	root := featureTaskWorkspace(t)
	uncommitted, report := featureTaskReport(t, root, "")
	if state := staleEvidence(report); state != "verified" {
		t.Fatalf("verdict = %s, want the package-root evidence matched at the tree base version\n%s", state, uncommitted)
	}
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "fixture")
	head := gitHead(t, root)
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "another commit, one tree")
	for _, version := range []string{"", treeBaseVersion, "1.2.3"} {
		got, report := featureTaskReport(t, root, version)
		if got != uncommitted {
			t.Errorf("version %q at another commit wrote another report:\n%s\nwant\n%s", version, got, uncommitted)
		}
		if report.Revision.Head != "" || report.Revision.Commit != "" {
			t.Errorf("revision = %+v, want the worktree with no commit", report.Revision)
		}
	}
	for _, commit := range []string{head, gitHead(t, root)} {
		if strings.Contains(uncommitted, commit) {
			t.Errorf("the report names commit %s:\n%s", commit, uncommitted)
		}
	}
	if strings.Contains(uncommitted, root) {
		t.Errorf("the report names the checkout directory:\n%s", uncommitted)
	}
}

func gitHead(t *testing.T, root string) string {
	t.Helper()
	head, err := internalgit.HeadSHA(root)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ToLower(head)
}

// Outside a Git work tree no `git:` key exists, so the step reads the disk as
// the interactive command does.
func TestFeatureTaskReadsTheDiskOutsideAWorkTree(t *testing.T) {
	root := decisionsWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, "billing", "putnami.json"), `{"name":"@acme/billing","type":"library"}`)
	manifest := featureproto.Manifest{
		ProtocolVersion: featureproto.MinimumManifestProtocolVersion,
		Namespace:       "billing",
		Features: []featureproto.Feature{{
			ID: "billing/export", Type: featureproto.FeatureTypeFeature, Name: "Export", Outcome: "Invoices can be exported", Owner: "billing", Target: featureproto.MaturityCoded,
			Requirements: []featureproto.Requirement{{ID: "implementation", Stage: featureproto.MaturityCoded, EvidenceKinds: []featureproto.EvidenceKind{featureproto.EvidenceKindAttestation}}},
		}},
	}
	data, err := featureproto.MarshalManifest(&manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(root, "billing", featureproto.ManifestFilename), string(data))
	_, report := featureTaskReport(t, root, "")
	if report.Summary.Features != 1 || report.Summary.Requirements != 1 {
		t.Fatalf("summary = %+v, want the manifest on disk read", report.Summary)
	}
	if _, err := BuildFeatureTaskValidationResult(nil, Selection{}); err == nil {
		t.Fatal("a nil workspace was evaluated")
	}
}
