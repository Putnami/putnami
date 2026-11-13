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

func TestAMovedBiomeLintKeepsBiomesDefaults(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "web/tsconfig.json", "{}\n")
	writeFile(t, root, "api/tsconfig.json", "{}\n")
	writeFile(t, root, "api/biome.jsonc", "{}\n")
	files := func(projects ...ConventionProject) []string {
		t.Helper()
		writes, _, err := ImportConventions(root, projects)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, w := range writes {
			out = append(out, w.File+" "+string(w.After))
		}
		return out
	}
	web := ConventionProject{Dir: "web", Name: "web", BiomeLint: true}
	if got := files(web); !reflect.DeepEqual(got, []string{"biome.json {}\n"}) {
		t.Errorf("a moved Biome lint writes %q, want a root biome.json that sets nothing", got)
	}
	// No Biome lint moves, or the project that moves one holds its own
	// configuration: the root gains none.
	if got := files(ConventionProject{Dir: "web", Name: "web"}, ConventionProject{Dir: "api", Name: "api", BiomeLint: true}); got != nil {
		t.Errorf("no project reads the root's configuration, yet the import writes %q", got)
	}
	// The repository's own configuration stays. The TypeScript lint does
	// not read a biome.jsonc: the import warns about each one a moved lint
	// would read.
	for _, name := range []string{"biome.json", "biome.jsonc"} {
		writeFile(t, root, name, "{\"formatter\": {\"indentStyle\": \"space\"}}\n")
		if got := files(web); got != nil {
			t.Errorf("with a root %s the import writes %q", name, got)
		}
		_, warnings, err := ImportConventions(root, []ConventionProject{web, {Dir: "api", Name: "api", BiomeLint: true}})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"api/biome.jsonc: the lint of @putnami/typescript reads biome.json alone, so the lint that moved does not check with this configuration; rename it biome.json, in place of the one the install writes"}
		if name == "biome.jsonc" {
			want = append(want, strings.Replace(want[0], "api/", "", 1))
		}
		if !reflect.DeepEqual(warnings, want) {
			t.Errorf("with a root %s the warnings = %q, want %q", name, warnings, want)
		}
		if err := os.Remove(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
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
