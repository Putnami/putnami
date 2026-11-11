package stacks

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSourceExportsKeepAManifestTheyCannotEdit(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "src/index.ts", "export {}\n")
	for name, manifest := range map[string]string{
		"a main under src": `{"name": "a", "main": "src/index.ts"}`,
		"no main":          `{"name": "a"}`,
		"exports":          `{"name": "a", "main": "lib/index.js", "exports": "./lib/index.js"}`,
	} {
		if got, reason, err := withSourceExports(dir, []byte(manifest)); err != nil || got != nil || reason != "" {
			t.Errorf("%s: %s %q, %v", name, got, reason, err)
		}
	}
	for name, c := range map[string]struct{ manifest, reason string }{
		"one line": {`{"name": "a", "main": "lib/index.js"}`, "its entry is on its last line"},
		"shared":   {"{\n  \"name\": \"a\", \"main\": \"lib/index.js\"\n}\n", "shares its line"},
		"CRLF":     {"{\r\n  \"main\": \"lib/index.js\"\r\n}\r\n", "CRLF"},
	} {
		if got, reason, err := withSourceExports(dir, []byte(c.manifest)); err != nil || got != nil || !strings.Contains(reason, c.reason) {
			t.Errorf("%s: %s %q, %v, want the reason %q", name, got, reason, err, c.reason)
		}
	}
	got, _, err := withSourceExports(dir, []byte("{\n\t\"name\": \"a\",\n\t\"types\": \"dist/index.d.ts\"\n}\n"))
	if err != nil || string(got) != "{\n\t\"name\": \"a\",\n\t\"types\": \"dist/index.d.ts\",\n\t\"exports\": {\n\t\t\".\": \"./src/index.ts\"\n\t}\n}\n" {
		t.Errorf("a last types member: %q, %v", got, err)
	}
	// A nested main before the top-level one stays as it is.
	nested := "{\n  \"publishConfig\": {\n    \"main\": \"dist/index.js\"\n  },\n  \"main\": \"lib/index.js\",\n  \"name\": \"a\"\n}\n"
	got, _, err = withSourceExports(dir, []byte(nested))
	want := "{\n  \"publishConfig\": {\n    \"main\": \"dist/index.js\"\n  },\n  \"main\": \"lib/index.js\",\n  \"exports\": {\n    \".\": \"./src/index.ts\"\n  },\n  \"name\": \"a\"\n}\n"
	if err != nil || string(got) != want {
		t.Errorf("a nested main:\n%s\nwant\n%s (%v)", got, want, err)
	}
	if _, _, err := withSourceExports(dir, []byte("not json")); err == nil {
		t.Error("a manifest that is no JSON passes")
	}
}
func TestImportConventionsFollowTheBuild(t *testing.T) {
	root := t.TempDir()
	manifest := "{\n  \"name\": \"@demo/pricing\",\n  \"main\": \"lib/index.js\"\n}\n"
	for _, dir := range []string{"bun", "tsc", "published", "configured"} {
		writeFile(t, root, dir+"/package.json", manifest)
		writeFile(t, root, dir+"/src/index.ts", "export {}\n")
	}
	writeFile(t, root, "tsc/tsconfig.lib.json", "{}\n")
	writeFile(t, root, "configured/tsconfig.json", "{}\n")
	writes, warnings, err := ImportConventions(root, []ConventionProject{
		{Dir: "bun", Name: "bun", BunBuild: true},
		{Dir: "tsc", Name: "tsc"},
		{Dir: "published", Name: "@demo/published", BunBuild: true, PublishedBy: "rush.json"},
		{Dir: "configured", Name: "configured"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, w := range writes {
		files = append(files, w.File)
	}
	// A Bun build gets the source entry; a tsc build, whose outDir its main
	// names, and a published project keep theirs. Only a project with no
	// tsconfig*.json gets one.
	if want := []string{"bun/package.json", "bun/tsconfig.json", "published/tsconfig.json"}; !reflect.DeepEqual(files, want) {
		t.Errorf("writes = %v, want %v", files, want)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "rush.json publishes @demo/published") {
		t.Errorf("warnings = %v, want the published project", warnings)
	}

	changes, notes, err := ApplyConventions(root, writes, true)
	if err != nil || len(changes) != 3 || len(notes) != 0 {
		t.Fatalf("a dry apply: %v %v %v", changes, notes, err)
	}
	if _, err := os.Stat(filepath.Join(root, "bun/tsconfig.json")); !os.IsNotExist(err) {
		t.Errorf("a dry apply wrote: %v", err)
	}
	changes, notes, err = ApplyConventions(root, writes, false)
	if err != nil || len(changes) != 0 || len(notes) != 3 || !strings.HasPrefix(notes[0], "bun/package.json: exports ./src/index.ts added") {
		t.Fatalf("an apply: %v %v %v", changes, notes, err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "bun/tsconfig.json")); string(data) != "{\n  \"extends\": \"../tsconfig.json\"\n}\n" {
		t.Errorf("bun/tsconfig.json = %s", data)
	}
}

func TestPackagePublishedReadsPrivate(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "open/package.json", `{"name": "a"}`)
	writeFile(t, root, "closed/package.json", `{"name": "b", "private": true}`)
	if PackagePublished(root, "open") == "" || PackagePublished(root, "closed") != "" || PackagePublished(root, "missing") != "" {
		t.Errorf("published: open %q, closed %q, missing %q", PackagePublished(root, "open"), PackagePublished(root, "closed"), PackagePublished(root, "missing"))
	}
}
