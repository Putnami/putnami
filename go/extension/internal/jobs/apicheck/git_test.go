package apicheck

import (
	"os"
	"path/filepath"
	"testing"
)

func TestATreeListingTellsRegularFilesAndReadsAPathLiterally(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "")
	f.write("lib/greet/[x].json", "{}\n")
	f.write("lib/greet/x.json", "{\"x\":1}\n")
	if err := os.Symlink("greet.go", filepath.Join(f.project, "link.go")); err != nil {
		t.Fatal(err)
	}
	f.commit("chore: a link and a name a glob would match")
	f.git("tag", "lib/v0.5.0")

	entries, err := treeAtTag(f.project, "lib/v0.5.0", ".", treeListing{recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	regular := map[string]bool{}
	for _, entry := range entries {
		regular[entry.name] = entry.regular
		if entry.size != 0 {
			t.Fatalf("%s has size %d in a listing that reads no size", entry.name, entry.size)
		}
	}
	want := map[string]bool{"[x].json": true, "go.mod": true, "greet.go": true, "link.go": false, "putnami.json": true, "x.json": true}
	if len(regular) != len(want) {
		t.Fatalf("listed %v, want %v", regular, want)
	}
	for name, isRegular := range want {
		if got, ok := regular[name]; !ok || got != isRegular {
			t.Fatalf("listed %v, want %v", regular, want)
		}
	}

	entries, err = treeAtTag(f.project, "lib/v0.5.0", "[x].json", treeListing{sized: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].name != "[x].json" || !entries[0].regular || entries[0].size != 3 {
		t.Fatalf("listed %+v, want [x].json alone, regular, 3 bytes", entries)
	}
}

func TestAWideTreeListingReadsNoBlob(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "")
	f.git("config", "uploadpack.allowFilter", "true")
	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, f.root, "clone", "-q", "--no-checkout", "--filter=blob:none", "file://"+f.root, clone)
	// The clone holds no blob, and its promisor remote is gone: a listing that
	// reads a blob fails.
	runGit(t, clone, "config", "remote.origin.url", "file://"+filepath.Join(t.TempDir(), "gone"))
	project := filepath.Join(clone, "lib", "greet")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := treeAtTag(project, firstTag, "greet.go", treeListing{sized: true}); err == nil {
		t.Fatal("a sized listing read a blob the clone does not hold")
	}
	entries, err := treeAtTag(project, firstTag, ".", treeListing{recursive: true})
	if err != nil {
		t.Fatalf("the wide listing read a blob: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("listed %+v, want go.mod, greet.go and putnami.json", entries)
	}
}
