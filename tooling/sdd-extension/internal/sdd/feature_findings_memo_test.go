package sdd

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/protocol/evidence"
	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	job "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	featureengine "go.putnami.dev/tooling/sdd/extension/internal/features"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// memoAt is a memo in a fresh directory, written by code.
func memoAt(t *testing.T, code string) FeatureFindingsMemo {
	t.Helper()
	return FeatureFindingsMemo{Path: filepath.Join(t.TempDir(), "sdd-features", "evaluations.memo"), Code: code}
}

// memoizedRun builds the findings of ws with memo and without, fails unless
// the two give the same report and the same fragment bytes, and returns the
// projects the memoized build evaluated rather than reused.
func memoizedRun(t *testing.T, ws *workspace.Workspace, memo FeatureFindingsMemo) []string {
	t.Helper()
	freshReport, fresh, err := BuildFeatureFindingsResult(ws, findingsReferences, 4)
	if err != nil {
		t.Fatalf("fresh build: %v", err)
	}
	report, fragments, evaluated, err := buildFeatureFindings(ws, findingsReferences, 4, memo)
	if err != nil {
		t.Fatalf("memoized build: %v", err)
	}
	want, err := evidence.EncodeFragments(fresh)
	if err != nil {
		t.Fatal(err)
	}
	got, err := evidence.EncodeFragments(fragments)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the memoized build wrote other fragments than a fresh one:\n got:\n%s\n want:\n%s", got, want)
	}
	if !reflect.DeepEqual(report, freshReport) {
		t.Fatalf("the memoized build reported %+v, a fresh one %+v", report, freshReport)
	}
	return evaluated
}

func assertEvaluated(t *testing.T, step string, evaluated []string, want ...string) {
	t.Helper()
	if !slices.Equal(evaluated, want) {
		t.Fatalf("%s: evaluated %v, want %v", step, evaluated, want)
	}
}

// TestAMemoizedRunWritesTheBytesOfAFreshRun pins the memo of the features
// step: a run evaluates the projects whose reads moved, reuses the others, and
// writes the bytes a fresh run writes, whatever changed.
func TestAMemoizedRunWritesTheBytesOfAFreshRun(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "feature-findings-are-evidence-fragments",
		"every-projects-findings-are-stated-at-the-files-they-name")
	ws := findingsWorkspace(t)
	memo := memoAt(t, "code")

	assertEvaluated(t, "a cold run", memoizedRun(t, ws, memo), "/billing", "/platform")
	assertEvaluated(t, "a warm run", memoizedRun(t, ws, memo))
	// A file no evaluation reads moves no entry.
	writeFixtureFile(t, filepath.Join(ws.Root, "docs", "guide.md"), "# Guide\n")
	assertEvaluated(t, "a file no evaluation reads", memoizedRun(t, ws, memo))
	// The proof billing's evidence names is read by billing's evaluation
	// alone.
	writeFixtureFile(t, filepath.Join(ws.Root, "billing", "proof_test.go"), "package billing\n")
	assertEvaluated(t, "a file of billing's source", memoizedRun(t, ws, memo), "/billing")
	// Every evaluation reads every authored manifest.
	writeFeatureManifest(t, ws.Root, "platform", provenFeature("platform/commerce", "sell", "deliver"))
	assertEvaluated(t, "platform's manifest", memoizedRun(t, ws, memo), "/billing", "/platform")
	// The workspace view is read by every evaluation.
	ws.Projects[0].Dependencies = []string{"/platform"}
	assertEvaluated(t, "the workspace view", memoizedRun(t, ws, memo), "/billing", "/platform")
	assertEvaluated(t, "the same view again", memoizedRun(t, ws, memo))
}

// TestACrossProjectFindingReevaluatesEveryProjectItConcerns pins the rules
// that read another project's files: a feature id declared in two projects,
// and evidence another root holds for a project's feature. The evaluation of
// each project that read the moved file runs again.
func TestACrossProjectFindingReevaluatesEveryProjectItConcerns(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "feature-findings-are-evidence-fragments",
		"every-projects-findings-are-stated-at-the-files-they-name")
	root := candidateWorkspace(t)
	modeled := func(id string) featureproto.Feature {
		return featureproto.Feature{ID: id, Type: featureproto.FeatureTypeFeature, Name: "Feature " + id, Outcome: "Outcome of " + id, Owner: "team", Target: featureproto.MaturityModeled}
	}
	writeFeatureManifest(t, root, "billing", modeled("billing/invoicing"))
	writeFeatureManifest(t, root, "platform", modeled("platform/commerce"))
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{
		{ID: "/billing", Name: "billing", Path: "billing", Extensions: []string{"@putnami/sdd"}},
		{ID: "/platform", Name: "platform", Path: "platform", Extensions: []string{"@putnami/sdd"}},
	})
	memo := memoAt(t, "code")
	memoizedRun(t, ws, memo)
	assertEvaluated(t, "a warm run", memoizedRun(t, ws, memo))

	// platform declares billing's id: the finding concerns billing's file too.
	writeFeatureManifest(t, root, "platform", modeled("billing/invoicing"))
	assertEvaluated(t, "a duplicate id in platform", memoizedRun(t, ws, memo), "/billing", "/platform")
	_, fragments, _ := BuildFeatureFindingsResult(ws, findingsReferences, 1)
	stated := false
	for _, fragment := range fragments {
		stated = stated || (fragment.Rule == featureproto.ErrorCodeDuplicateFeature && fragment.Subject.Path == "billing/"+featureproto.ManifestFilename)
	}
	if !stated {
		t.Fatalf("fragments = %+v, want the duplicate stated at billing's manifest", fragments)
	}
	writeFeatureManifest(t, root, "platform", modeled("platform/commerce"))
	assertEvaluated(t, "the duplicate removed", memoizedRun(t, ws, memo), "/billing", "/platform")

	// platform's root holds a record for billing's feature.
	writeFeatureEvidence(t, filepath.Join(root, "platform", featureproto.EvidenceDirectory, "billing.json"), featureproto.EvidenceRecord{
		ID: "platform/billing-proof", Feature: "billing/invoicing", Requirement: "issue", Stage: featureproto.MaturityCoded, Outcome: featureproto.EvidenceOutcomeSupports,
		Issuer:     featureproto.Issuer{Kind: featureproto.IssuerKindTest, ID: "evidence-producer"},
		Source:     featureproto.SourceSelector{Root: featureproto.LocationRootProject, OwnerProject: "billing", Binding: "source-v1:sha256:" + strings.Repeat("a", 64)},
		Subject:    featureproto.EvidenceSubject{Kind: featureproto.EvidenceKindArtifact, Artifact: &featureproto.ArtifactSubject{Path: "invoice.txt", Digest: "sha256:" + strings.Repeat("b", 64)}},
		Provenance: featureproto.EvidenceProvenance{Root: featureproto.LocationRootProject, Path: "proof_test.go"},
	})
	if evaluated := memoizedRun(t, ws, memo); !slices.Contains(evaluated, "/billing") {
		t.Fatalf("evidence for billing's feature in platform's root: evaluated %v, want /billing among them", evaluated)
	}
}

// TestACorruptOrForeignMemoIsNeverTrusted pins that the memo is a hint: a
// file that is not whole, intact and written by the same code for the same
// workspace view gives no entry, the run evaluates every project and writes
// the bytes of a fresh run, and the memo it leaves serves the next run.
func TestACorruptOrForeignMemoIsNeverTrusted(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "feature-findings-are-evidence-fragments",
		"every-projects-findings-are-stated-at-the-files-they-name")
	ws := findingsWorkspace(t)
	valid := memoAt(t, "code")
	memoizedRun(t, ws, valid)
	written, err := os.ReadFile(valid.Path)
	if err != nil {
		t.Fatalf("the run wrote no memo: %v", err)
	}
	prior, ok := readFeatureMemo(valid.Path)
	if !ok {
		t.Fatal("the memo the run wrote does not read back")
	}
	crafted := func(edit func(*featureMemoFile)) func(*testing.T, string) {
		return func(t *testing.T, path string) {
			copied := *prior
			copied.Entries = slices.Clone(prior.Entries)
			copied.Reads = slices.Clone(prior.Reads)
			edit(&copied)
			encoded, err := encodeFeatureMemo(&copied)
			if err != nil {
				t.Fatal(err)
			}
			writeFixtureFile(t, path, string(encoded))
		}
	}
	for _, tc := range []struct {
		name  string
		write func(*testing.T, string)
	}{
		{"not a memo", func(t *testing.T, path string) { writeFixtureFile(t, path, "not a memo\n") }},
		{"cut short", func(t *testing.T, path string) { writeFixtureFile(t, path, string(written[:len(written)/2])) }},
		{"a changed byte", func(t *testing.T, path string) {
			changed := bytes.Clone(written)
			changed[len(changed)-1] ^= 0xff
			writeFixtureFile(t, path, string(changed))
		}},
		{"a changed character in a finding", func(t *testing.T, path string) {
			at := bytes.LastIndex(written, []byte("feature"))
			if at < 0 {
				t.Fatal("the memo holds no finding message")
			}
			changed := bytes.Clone(written)
			changed[at] = 'F'
			writeFixtureFile(t, path, string(changed))
		}},
		{"an unknown format", func(t *testing.T, path string) {
			writeFixtureFile(t, path, strings.Replace(string(written), featureMemoMagic, "putnami-sdd-features-memo/0\n", 1))
		}},
		{"over the bound", func(t *testing.T, path string) {
			writeFixtureFile(t, path, string(written))
			if err := os.Truncate(path, maxFeatureMemoBytes+1); err != nil {
				t.Fatal(err)
			}
		}},
		{"a directory", func(t *testing.T, path string) {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"written by another code", crafted(func(file *featureMemoFile) { file.Code = "other" })},
		{"for another view", crafted(func(file *featureMemoFile) { file.View[0] ^= 0xff })},
		{"a read out of range", crafted(func(file *featureMemoFile) {
			file.Entries[0].Reads = append(slices.Clone(file.Entries[0].Reads), len(file.Reads))
		})},
		{"a project twice", crafted(func(file *featureMemoFile) { file.Entries = append(file.Entries, file.Entries[0]) })},
		{"a read the reader does not know", crafted(func(file *featureMemoFile) {
			for index := range file.Reads {
				file.Reads[index].Key.Op = 'x'
			}
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memo := memoAt(t, "code")
			tc.write(t, memo.Path)
			assertEvaluated(t, tc.name, memoizedRun(t, ws, memo), "/billing", "/platform")
			if tc.name != "a directory" {
				assertEvaluated(t, tc.name+", the next run", memoizedRun(t, ws, memo))
			}
		})
	}
}

// TestConcurrentRunsKeepTheMemoWhole pins that runs which share a memo at
// once each write the bytes of a fresh run, and leave a memo the next run
// reads whole.
func TestConcurrentRunsKeepTheMemoWhole(t *testing.T) {
	ws := findingsWorkspace(t)
	memo := memoAt(t, "code")
	_, fresh, err := BuildFeatureFindingsResult(ws, findingsReferences, 1)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := evidence.EncodeFragments(fresh)
	var wait sync.WaitGroup
	failures := make(chan string, 8)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, fragments, err := BuildFeatureFindingsResultWithMemo(ws, findingsReferences, 2, memo)
			got, _ := evidence.EncodeFragments(fragments)
			if err != nil || !bytes.Equal(got, want) {
				failures <- "a concurrent run wrote other bytes"
			}
		}()
	}
	wait.Wait()
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}
	assertEvaluated(t, "the run after", memoizedRun(t, ws, memo))
	entries, _ := os.ReadDir(filepath.Dir(memo.Path))
	if len(entries) != 1 {
		t.Fatalf("the memo directory holds %d file(s), want the memo alone", len(entries))
	}
}

// TestTheMemoHoldsTheProjectsOfItsLastRun pins the bound of the memo: one
// entry for each project the last run validated, and no other.
func TestTheMemoHoldsTheProjectsOfItsLastRun(t *testing.T) {
	ws := findingsWorkspace(t)
	memo := memoAt(t, "code")
	memoizedRun(t, ws, memo)
	ws.Projects[3].Extensions = []string{"/go/extension"}
	assertEvaluated(t, "platform no longer names the extension", memoizedRun(t, ws, memo), "/billing")
	file, ok := readFeatureMemo(memo.Path)
	if !ok || len(file.Entries) != 1 || file.Entries[0].Project != "/billing" {
		t.Fatalf("memo = %+v, want billing's entry alone", file)
	}
	for _, read := range file.Entries[0].Reads {
		if read < 0 || read >= len(file.Reads) {
			t.Fatalf("billing's entry names read %d of %d", read, len(file.Reads))
		}
	}
}

// TestAMemoThatCannotBeWrittenChangesNothing pins that a memo path the run
// cannot write leaves the run's bytes as they are.
func TestAMemoThatCannotBeWrittenChangesNothing(t *testing.T) {
	ws := findingsWorkspace(t)
	blocked := filepath.Join(t.TempDir(), "file")
	writeFixtureFile(t, blocked, "a file, not a directory\n")
	memo := FeatureFindingsMemo{Path: filepath.Join(blocked, "evaluations.memo"), Code: "code"}
	assertEvaluated(t, "an unwritable memo", memoizedRun(t, ws, memo), "/billing", "/platform")
	assertEvaluated(t, "an unwritable memo again", memoizedRun(t, ws, memo), "/billing", "/platform")
	for _, disabled := range []FeatureFindingsMemo{{Path: memo.Path}, {Code: "code"}} {
		assertEvaluated(t, "a disabled memo", memoizedRun(t, ws, disabled), "/billing", "/platform")
	}
}

// TestFeatureCountsPlusAddsEveryCount pins that the summary of the merged
// evaluations adds every member of FeatureCounts.
func TestFeatureCountsPlusAddsEveryCount(t *testing.T) {
	var counts FeatureCounts
	value := reflect.ValueOf(&counts).Elem()
	for index := range value.NumField() {
		value.Field(index).SetInt(int64(index + 1))
	}
	sum := reflect.ValueOf(counts.plus(counts))
	for index := range sum.NumField() {
		if got := sum.Field(index).Int(); got != int64(2*(index+1)) {
			t.Errorf("plus adds %s to %d, want %d", sum.Type().Field(index).Name, got, 2*(index+1))
		}
	}
}

// TestTheViewDigestReadsTheWholeView pins that the workspace view an
// evaluation reads moves the memo's view digest: every member of a project,
// and every member of the workspace but those derived from the projects.
func TestTheViewDigestReadsTheWholeView(t *testing.T) {
	known := []string{"Name", "Version", "Root", "Config", "Projects", "Graph", "Warnings", "WarningCodes", "Tree", "NamesNoCommit"}
	var members []string
	for _, field := range reflect.VisibleFields(reflect.TypeOf(workspace.Workspace{})) {
		if field.IsExported() {
			members = append(members, field.Name)
		}
	}
	if !slices.Equal(members, known) {
		t.Fatalf("workspace.Workspace members = %v, viewDigest reads %v: digest each new member", members, known)
	}
	base := func() *workspace.Workspace {
		return workspace.NewWorkspace("/ws", nil, []*workspace.Project{{ID: "/billing", Name: "billing", Path: "billing"}})
	}
	digest := func(view *workspace.Workspace) [32]byte {
		sum, err := viewDigest(view)
		if err != nil {
			t.Fatal(err)
		}
		return sum
	}
	want := digest(base())
	projectType := reflect.TypeOf(workspace.Project{})
	for index := range projectType.NumField() {
		field := projectType.Field(index)
		view := base()
		member := reflect.ValueOf(view.Projects[0]).Elem().Field(index)
		switch member.Kind() {
		case reflect.String:
			member.SetString("changed")
		case reflect.Bool:
			member.SetBool(true)
		case reflect.Slice:
			member.Set(reflect.ValueOf([]string{"changed"}))
		case reflect.Pointer:
			member.Set(reflect.New(field.Type.Elem()))
		default:
			t.Fatalf("project member %s has a kind this test does not change", field.Name)
		}
		if digest(view) == want {
			t.Errorf("changing project member %s keeps the view digest", field.Name)
		}
	}
	for name, edit := range map[string]func(*workspace.Workspace){
		"Name":    func(view *workspace.Workspace) { view.Name = "changed" },
		"Version": func(view *workspace.Workspace) { view.Version = "1.0.0" },
		"Root":    func(view *workspace.Workspace) { view.Root = "/other" },
		"Warnings": func(view *workspace.Workspace) {
			view.AddWarning(workspace.WarningCodeProviderViewUnavailable, "partial")
		},
		"NamesNoCommit": func(view *workspace.Workspace) { view.NamesNoCommit = true },
		"Config":        func(view *workspace.Workspace) { view.Config = &wsproto.Config{Name: "changed"} },
		"Tree":          func(view *workspace.Workspace) { view.Tree = &job.Tree{Directory: "."} },
	} {
		view := base()
		edit(view)
		if digest(view) == want {
			t.Errorf("changing the workspace's %s keeps the view digest", name)
		}
	}
}

// TestAnAnswerDigestTellsEveryAnswerApart pins the digest a memo compares a
// read's answer by: two answers an evaluation can tell apart have two
// digests.
func TestAnAnswerDigestTellsEveryAnswerApart(t *testing.T) {
	missing := &fs.PathError{Op: "stat", Path: "a", Err: fs.ErrNotExist}
	answers := map[string][32]byte{
		"file a":               answerDigest(readFile, []byte("a"), nil, "", nil),
		"file b":               answerDigest(readFile, []byte("b"), nil, "", nil),
		"file empty":           answerDigest(readFile, []byte{}, nil, "", nil),
		"file missing":         answerDigest(readFile, nil, nil, "", missing),
		"file refused":         answerDigest(readFile, nil, nil, "", errors.New("feature reader: read-limit")),
		"dir a":                answerDigest(readDir, nil, []featureengine.DirEntry{{Name: "a"}}, "", nil),
		"dir a/":               answerDigest(readDir, nil, []featureengine.DirEntry{{Name: "a", IsDirectory: true}}, "", nil),
		"dir a b":              answerDigest(readDir, nil, []featureengine.DirEntry{{Name: "a"}, {Name: "b"}}, "", nil),
		"dir b a":              answerDigest(readDir, nil, []featureengine.DirEntry{{Name: "b"}, {Name: "a"}}, "", nil),
		"dir ab":               answerDigest(readDir, nil, []featureengine.DirEntry{{Name: "ab"}}, "", nil),
		"binding a":            answerDigest(readBinding, nil, nil, "a", nil),
		"binding unavailable":  answerDigest(readBinding, nil, nil, "", errors.New("unavailable")),
		"binding a with error": answerDigest(readBinding, nil, nil, "a", errors.New("unavailable")),
	}
	seen := map[[32]byte]string{}
	for name, digest := range answers {
		if other, ok := seen[digest]; ok {
			t.Errorf("%s and %s share a digest", name, other)
		}
		seen[digest] = name
	}
	if answerDigest(readFile, []byte("a"), nil, "", missing) != answerDigest(readFile, []byte("a"), nil, "", &fs.PathError{Op: "stat", Path: "a", Err: fs.ErrNotExist}) {
		t.Error("one answer has two digests")
	}
}
