package apicheck

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/typescript/extension/internal/apisurface"
)

func TestATreeListingTellsRegularFilesAndReadsAPathLiterally(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "")
	f.write("ts/lib/[x].json", "{}\n")
	f.write("ts/lib/x.json", "{\"x\":1}\n")
	if err := os.Symlink("src/index.ts", filepath.Join(f.project, "link.ts")); err != nil {
		t.Fatal(err)
	}
	f.commit("chore: a link and a name a glob would match")
	f.git("tag", "ts/v0.5.0")

	entries, err := treeAtTag(f.project, "ts/v0.5.0", ".", treeListing{recursive: true})
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
	want := map[string]bool{
		"[x].json": true, "link.ts": false, "package.json": true, "putnami.json": true,
		"src/hooks.ts": true, "src/index.ts": true, "x.json": true,
	}
	if len(regular) != len(want) {
		t.Fatalf("listed %v, want %v", regular, want)
	}
	for name, isRegular := range want {
		if got, ok := regular[name]; !ok || got != isRegular {
			t.Fatalf("listed %v, want %v", regular, want)
		}
	}

	entries, err = treeAtTag(f.project, "ts/v0.5.0", "[x].json", treeListing{sized: true})
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
	project := filepath.Join(clone, "ts", "lib")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := treeAtTag(project, firstTag, "package.json", treeListing{sized: true}); err == nil {
		t.Fatal("a sized listing read a blob the clone does not hold")
	}
	entries, err := treeAtTag(project, firstTag, ".", treeListing{recursive: true})
	if err != nil {
		t.Fatalf("the wide listing read a blob: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("listed %+v, want package.json, putnami.json and the two sources", entries)
	}
}

// names returns the paths of files, with their contents.
func names(files []apisurface.File) []string {
	found := make([]string, len(files))
	for i, file := range files {
		found[i] = file.Path + "=" + string(file.Data)
	}
	return found
}

func TestTheFilesAtATagAreItsRegularSourceFiles(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "")
	f.write("ts/lib/src/view.tsx", "export const view = 1;\n")
	f.write("ts/lib/readme.md", "# lib\n")
	f.write("ts/lib/vendor/node_modules/dep/index.ts", "export const dep = 1;\n")
	if err := os.Symlink("src/index.ts", filepath.Join(f.project, "link.ts")); err != nil {
		t.Fatal(err)
	}
	f.commit("chore: files of other kinds")
	f.git("tag", "ts/v0.5.0")
	// The tag is read, not the working tree.
	f.write("ts/lib/src/view.tsx", "export const view = 2;\n")
	f.write("ts/lib/src/later.ts", "export const later = 1;\n")

	entries, err := treeAtTag(f.project, "ts/v0.5.0", ".", treeListing{recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	files, err := filesAtTag(f.project, entries)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"src/hooks.ts=" + hooksSource, "src/index.ts=" + greetSource, "src/view.tsx=export const view = 1;\n"}
	if got := names(files); !slices.Equal(got, want) {
		t.Fatalf("files = %q\nwant    %q", got, want)
	}

	if files, err := filesAtTag(f.project, []treeEntry{{name: "readme.md", regular: true}}); err != nil || files != nil {
		t.Fatalf("files = %v, err = %v, want none and no git call for a listing without source", files, err)
	}
}

func TestTheFilesOfTheTreeAreTheSourceFilesACommitWouldHold(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "")
	f.write("ts/lib/.gitignore", "dist\n")
	f.write("ts/lib/dist/index.js", "export const built = 1;\n")
	f.write("ts/lib/src/untracked.ts", "export const untracked = 1;\n")
	f.write("ts/lib/src/index.ts", waveOnlySource)
	f.write("ts/lib/notes.md", "notes\n")
	f.write("ts/lib/[x].ts", "export const literal = 1;\n")
	f.remove("ts/lib/src/hooks.ts")
	if err := os.Symlink("src/index.ts", filepath.Join(f.project, "link.ts")); err != nil {
		t.Fatal(err)
	}

	files, err := filesInTree(f.project)
	if err != nil {
		t.Fatal(err)
	}
	// The uncommitted edit and the untracked file are read; the ignored build
	// output, the deleted file, the link and the file of another kind are not.
	want := []string{"[x].ts=export const literal = 1;\n", "src/index.ts=" + waveOnlySource, "src/untracked.ts=export const untracked = 1;\n"}
	if got := names(files); !slices.Equal(got, want) {
		t.Fatalf("files = %q\nwant    %q", got, want)
	}
}
