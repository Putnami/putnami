package components

import (
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
)

func splitDiagnostic(t *testing.T, s Set) diag.Diagnostic {
	t.Helper()
	for _, d := range s.Diagnostics {
		if d.Code == CodeFallbackSplit {
			if d.Severity != diag.Info {
				t.Errorf("the split diagnostic is %s, want info", d.Severity)
			}
			return d
		}
	}
	t.Fatalf("no %s diagnostic in %+v", CodeFallbackSplit, s.Diagnostics)
	return diag.Diagnostic{}
}

func TestCodeownersDirectoriesAreComponents(t *testing.T) {
	spectest.Proves(t, feature, "fallback-split", "codeowners-directories-are-components")
	root := t.TempDir()
	write(t, root, map[string]string{
		".github/CODEOWNERS": strings.Join([]string{
			"# owners",
			"*                 @acme/maintainers",
			"/billing/         @acme/billing",
			"/catalog/**       @acme/catalog",
			"web/              @acme/web",
			"services/search/* @acme/search",
			"*.md              @acme/docs",
			"/apps/*/src/      @acme/apps",
			"!/billing/legacy/ @acme/legacy",
			"[Docs] @acme/docs",
			"/missing/         @acme/nobody",
			"/README.md        @acme/docs",
			"/../outside/      @acme/nobody",
			"/billing/         @acme/again",
		}, "\n"),
		"billing/invoice.py":        "",
		"catalog/catalog.py":        "",
		"web/app.js":                "",
		"services/search/search.go": "",
		"README.md":                 "",
		// .github/CODEOWNERS comes before docs/CODEOWNERS: the first file
		// that exists is the one read.
		"docs/CODEOWNERS": "/docs/ @acme/docs\n",
	})
	set := Detect(root, Default(Options{}))
	want := []string{"billing", "catalog", "services/search", "web"}
	if got := ids(set); !slices.Equal(got, want) {
		t.Fatalf("components %v, want %v", got, want)
	}
	for _, c := range set.Components {
		if c.Source != evidence.SourceFallbackCodeowners || c.Ecosystem != FallbackEcosystem || len(c.DependsOn) != 0 || c.Name != c.ID[strings.LastIndex(c.ID, "/")+1:] {
			t.Errorf("component %+v", c)
		}
	}
	if d := splitDiagnostic(t, set); !strings.Contains(d.Message, ".github/CODEOWNERS") || !strings.Contains(d.Message, "4 directories") {
		t.Errorf("split diagnostic %q", d.Message)
	}
	if owner, ok := set.Owner("services/search/search.go"); !ok || owner != "services/search" {
		t.Errorf("owner %q", owner)
	}
	if _, ok := set.Owner("README.md"); ok {
		t.Error("a root file has an owner")
	}
}

func TestTheFirstCodeownersFileIsRead(t *testing.T) {
	spectest.Proves(t, feature, "fallback-split", "codeowners-directories-are-components")
	root := t.TempDir()
	write(t, root, map[string]string{
		".github/CODEOWNERS": "/api/ @acme/api\n",
		"CODEOWNERS":         "/web/ @acme/web\n",
		"api/main.go":        "",
		"web/app.js":         "",
	})
	set := Detect(root, Default(Options{}))
	if got := ids(set); !slices.Equal(got, []string{"api"}) {
		t.Fatalf("components %v", got)
	}
}

func TestTopLevelSourceDirectoriesAreComponents(t *testing.T) {
	spectest.Proves(t, feature, "fallback-split", "top-level-source-directories-are-components")
	root := t.TempDir()
	write(t, root, map[string]string{
		// A CODEOWNERS that names no directory leaves the split to the
		// top-level directories.
		"CODEOWNERS":                 "* @acme/maintainers\n*.md @acme/docs\n",
		"README.md":                  "",
		"main.c":                     "",
		"billing/src/deep/invoice.c": "",
		"catalog/Catalog.KT":         "",
		"assets/logo.png":            "",
		"notes/todo.txt":             "",
		"hidden/.cache/x.go":         "",
		"deps/node_modules/x/i.js":   "",
		".github/workflows/ci.sh":    "",
		".tools/run.sh":              "",
		"docs/build.py":              "",
		"vendor/lib/lib.go":          "",
		"node_modules/x/index.js":    "",
	})
	set := Detect(root, Default(Options{}))
	want := []string{"billing", "catalog"}
	if got := ids(set); !slices.Equal(got, want) {
		t.Fatalf("components %v, want %v", got, want)
	}
	for _, c := range set.Components {
		if c.Source != evidence.SourceFallbackDirectories || c.Ecosystem != FallbackEcosystem || c.Name != c.ID || len(c.DependsOn) != 0 {
			t.Errorf("component %+v", c)
		}
	}
	if d := splitDiagnostic(t, set); !strings.Contains(d.Message, "2 top-level directories") {
		t.Errorf("split diagnostic %q", d.Message)
	}
}

func TestADetectedComponentLeavesNoFallbackSplit(t *testing.T) {
	spectest.Proves(t, feature, "fallback-split", "codeowners-directories-are-components")
	root := t.TempDir()
	write(t, root, map[string]string{
		"CODEOWNERS":  "/api/ /web/ @acme\n/web/ @acme/web\n",
		"api/go.mod":  "module example.com/api\n\ngo 1.23\n",
		"api/main.go": "package main\n",
		"web/app.js":  "",
	})
	set := Detect(root, Default(Options{}))
	if got := ids(set); !slices.Equal(got, []string{"api"}) {
		t.Fatalf("components %v", got)
	}
	for _, d := range set.Diagnostics {
		if d.Code == CodeFallbackSplit {
			t.Errorf("split diagnostic %q with a detected component", d.Message)
		}
	}
}

func TestAFallbackChangeTouchesOnlyItsOwnComponents(t *testing.T) {
	spectest.Proves(t, feature, "fallback-split", "a-fallback-change-touches-only-its-own-components")
	root := t.TempDir()
	write(t, root, map[string]string{
		"CODEOWNERS":            "/services/ @acme\n/services/billing/ @acme/billing\n/web/ @acme/web\n",
		"services/a.go":         "",
		"services/billing/b.go": "",
		"web/app.js":            "",
	})
	set := Detect(root, Default(Options{}))
	owner, _ := set.Owner("services/billing/b.go")
	got := set.Impacts(map[string]bool{owner: true})
	want := map[string]string{"services": evidence.ImpactUnaffected, "services/billing": evidence.ImpactChanged, "web": evidence.ImpactUnaffected}
	for id, impact := range want {
		if got[id] != impact {
			t.Errorf("impact of %s = %s, want %s", id, got[id], impact)
		}
	}
	if len(got) != len(want) {
		t.Errorf("impacts %v", got)
	}
}

func TestCodeownersDirectory(t *testing.T) {
	spectest.Proves(t, feature, "fallback-split", "codeowners-directories-are-components")
	for line, want := range map[string]string{
		"/api/ @a":      "api",
		"api/ @a":       "api",
		"/api @a":       "api",
		"/api/** @a":    "api",
		"/api/* @a":     "api",
		"  /a/b/  @a":   "a/b",
		"/a//b/ @a":     "a/b",
		"/ @a":          "",
		"* @a":          "",
		"*.go @a":       "",
		"/a/*/b @a":     "",
		"/a/b?/ @a":     "",
		"# /api/":       "",
		"!/api/ @a":     "",
		"[Section] @a":  "",
		"^[Section] @a": "",
		`/my\ dir/ @a`:  "",
		"/../x/ @a":     "",
		"":              "",
	} {
		got, ok := codeownersDirectory(line)
		if ok != (want != "") || got != want {
			t.Errorf("codeownersDirectory(%q) = %q, %v; want %q", line, got, ok, want)
		}
	}
}

func TestALargeDirectoryWithoutSourceLeavesTheOthersToTheSplit(t *testing.T) {
	spectest.Proves(t, feature, "fallback-split", "top-level-source-directories-are-components")
	budget := fallbackWalkBudget
	fallbackWalkBudget = 5
	t.Cleanup(func() { fallbackWalkBudget = budget })
	root := t.TempDir()
	files := map[string]string{"billing/invoice.go": "", "web/app.js": ""}
	for i := range 10 {
		files["assets/img"+string(rune('a'+i))+".png"] = ""
	}
	write(t, root, files)
	set := Detect(root, Default(Options{}))
	if got := ids(set); !slices.Equal(got, []string{"billing", "web"}) {
		t.Fatalf("components %v", got)
	}
	bounded := false
	for _, d := range set.Diagnostics {
		if d.Code == CodeFallbackWalkBounded && strings.Contains(d.Message, "assets") {
			bounded = true
		}
	}
	if !bounded {
		t.Errorf("no %s diagnostic naming assets in %+v", CodeFallbackWalkBounded, set.Diagnostics)
	}
}
