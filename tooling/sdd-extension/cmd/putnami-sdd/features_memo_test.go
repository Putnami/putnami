package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
)

// TestTheFeaturesStepKeepsItsMemoInItsCacheRoot pins where the features step
// keeps its memo, and that it changes nothing the step writes: the memo is
// one file under the cacheRoot the job context names, a second run writes the
// bytes of the first, and a context without a cacheRoot, or with a relative
// one, keeps no memo and writes the same bytes. The step writes nothing in the
// workspace but its command output.
func TestTheFeaturesStepKeepsItsMemoInItsCacheRoot(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "feature-findings-are-evidence-fragments",
		"every-projects-findings-are-stated-at-the-files-they-name")
	root := validationFixture(t, validInvoicingSpec)
	writeFixture(t, filepath.Join(root, "billing", featureproto.ManifestFilename),
		billingManifest(`{"kind": "parent", "target": "platform/nonexistent"}`))
	before := workspaceFiles(t, root)
	run := func(cacheRoot string) ([]byte, map[string]any) {
		t.Helper()
		ctx := featuresJobContext(root, sddMember(billingMember(root)), sddMember(platformMember(root)))
		ctx.CacheRoot = cacheRoot
		outcome := runJob(t, runFeaturesValidate, ctx)
		assertStatesAndDoesNotDecide(t, outcome, ctx)
		data, _ := writtenFragments(t, ctx)
		return data, outcome.data
	}

	cacheRoot := t.TempDir()
	first, firstData := run(cacheRoot)
	memo := filepath.Join(cacheRoot, featuresMemoFile)
	written, err := os.Stat(memo)
	if err != nil {
		t.Fatalf("the step kept no memo at %s: %v", memo, err)
	}
	if entries, _ := os.ReadDir(cacheRoot); len(entries) != 1 {
		t.Fatalf("the cacheRoot holds %d entries, want the memo's directory alone", len(entries))
	}
	second, secondData := run(cacheRoot)
	if again, err := os.Stat(memo); err != nil || !os.SameFile(again, written) {
		t.Errorf("a run that reused every evaluation replaced the memo: %v", err)
	}
	none, noneData := run("")
	relative, relativeData := run(filepath.Join("relative", "cache"))
	if _, err := os.Stat("relative"); !os.IsNotExist(err) {
		t.Errorf("a relative cacheRoot was written under the working directory: %v", err)
	}
	for name, got := range map[string][]byte{"a second run": second, "no cacheRoot": none, "a relative cacheRoot": relative} {
		if string(got) != string(first) {
			t.Errorf("%s wrote other fragments:\n%s\n---\n%s", name, got, first)
		}
	}
	for name, got := range map[string]map[string]any{"a second run": secondData, "no cacheRoot": noneData, "a relative cacheRoot": relativeData} {
		if !reflect.DeepEqual(got, firstData) {
			t.Errorf("%s reported %#v, want %#v", name, got, firstData)
		}
	}
	if !strings.Contains(string(first), "billing/"+featureproto.ManifestFilename) {
		t.Fatalf("the fixture states no finding:\n%s", first)
	}
	after := workspaceFiles(t, root)
	if !slices.Equal(after, before) {
		t.Errorf("the step wrote in the workspace outside its command output:\n before %v\n after  %v", before, after)
	}
}

// workspaceFiles lists the files of root, outside the command outputs.
func workspaceFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if entry.IsDir() && rel == filepath.Join(".putnami", "out") {
			return filepath.SkipDir
		}
		if !entry.IsDir() {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
