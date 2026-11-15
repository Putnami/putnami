package screens

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// webInventory builds an inventory of page and action routes, each written
// as "<match> <path> <file>" for a page and "action <path> <file>" for an
// action.
func webInventory(t *testing.T, routes ...string) *Inventory {
	t.Helper()
	var doc struct {
		Routes []map[string]any `json:"routes"`
	}
	for _, route := range routes {
		fields := strings.Fields(route)
		match, methods := fields[0], []string{"GET", "HEAD"}
		if match == "action" {
			match, methods = "exact", []string{"POST"}
			if strings.Contains(fields[1], "{") {
				match = "template"
			}
		}
		doc.Routes = append(doc.Routes, map[string]any{"match": match, "path": fields[1], "methods": methods, "publicEdge": true,
			"provenance": map[string]any{"project": "web", "package": "@putnami/web", "sourceKind": "file-route", "evidencePath": fields[2]}})
	}
	doc.Routes = append(doc.Routes, map[string]any{"match": "exact", "path": "/api/tasks", "methods": []string{"GET"}, "publicEdge": true,
		"provenance": map[string]any{"project": "web", "package": "@putnami/application", "sourceKind": "typed-api", "evidencePath": "src/api/tasks/get.ts"}})
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := ParseInventory(data)
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func sampleWeb(t *testing.T) *Inventory {
	return webInventory(t,
		"exact / src/app/page.tsx",
		"exact /tasks src/app/tasks/page.tsx",
		"exact /tasks/new src/app/tasks/new/page.tsx",
		"action /tasks/new src/app/tasks/new/action.ts",
		"template /tasks/{id} src/app/tasks/[id]/page.tsx",
	)
}

func routesOf(pages []Page) []string {
	var routes []string
	for _, page := range pages {
		routes = append(routes, page.Route)
	}
	return routes
}

// TestAPageFileInTheDiffTouchesItsPage selects the one page whose file
// changed, on both sides.
func TestAPageFileInTheDiffTouchesItsPage(t *testing.T) {
	t.Parallel()
	inv := sampleWeb(t)
	pages, err := Select(Selection{Project: "apps/web", Base: inv, Head: inv,
		Changed: []string{"apps/web/src/app/tasks/page.tsx", "README.md"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []Page{{File: "src/app/tasks/page.tsx", Route: "/tasks", Path: "/tasks", Before: true, After: true}}
	if !reflect.DeepEqual(pages, want) {
		t.Fatalf("pages = %+v", pages)
	}
	for _, changed := range []string{"apps/web/src/app/tasks/new/action.ts", "apps/web/src/api/tasks/get.ts",
		"apps/web/test/page.test.ts", "apps/web/doc/guide.md", "apps/webby/src/app/page.tsx"} {
		pages, err := TouchedRoutes(Selection{Project: "apps/web", Base: inv, Head: inv, Changed: []string{changed}})
		if err != nil || len(pages) != 0 {
			t.Fatalf("%s touched %v, %v", changed, routesOf(pages), err)
		}
	}
	spectest.Proves(t, screensFeature, touchedPages, "a-page-file-in-the-diff-touches-its-page")
}

// TestASharedSourceFileTouchesEveryPage selects every page, at the first
// route of each page file, when a component under src/ changed.
func TestASharedSourceFileTouchesEveryPage(t *testing.T) {
	t.Parallel()
	inv := webInventory(t,
		"exact / src/app/page.tsx",
		"exact /docs/a src/app/docs/[...slug]/page.tsx",
		"exact /docs/b src/app/docs/[...slug]/page.tsx",
		"exact /about src/app/about/page.tsx",
	)
	pages, err := TouchedRoutes(Selection{Project: "apps/web", Base: inv, Head: inv,
		Changed: []string{"apps/web/src/components/header.tsx"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := routesOf(pages); !reflect.DeepEqual(got, []string{"/", "/docs/a", "/about"}) {
		t.Fatalf("routes = %v", got)
	}
	pages, err = TouchedRoutes(Selection{Project: "apps/web", Base: inv, Head: inv,
		Changed: []string{"apps/web/src/app/docs/[...slug]/page.tsx"}, Routes: []string{"/docs/b", "/docs/a"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := routesOf(pages); !reflect.DeepEqual(got, []string{"/docs/a", "/docs/b"}) {
		t.Fatalf("routes with --route = %v", got)
	}
	if _, err := TouchedRoutes(Selection{Project: "apps/web", Head: inv, Routes: []string{"/api/tasks"}}); err == nil ||
		!strings.Contains(err.Error(), "--route /api/tasks is no page of apps/web") {
		t.Fatalf("an API route as --route: %v", err)
	}
	spectest.Proves(t, screensFeature, touchedPages, "a-shared-source-file-touches-every-page")
}

// TestAPageOneSideLacksIsCapturedOnTheOtherSide selects a new page after only
// and a removed page before only.
func TestAPageOneSideLacksIsCapturedOnTheOtherSide(t *testing.T) {
	t.Parallel()
	base := webInventory(t, "exact / src/app/page.tsx", "exact /old src/app/old/page.tsx")
	head := webInventory(t, "exact / src/app/page.tsx", "exact /new src/app/new/page.tsx")
	pages, err := TouchedRoutes(Selection{Project: "web", Base: base, Head: head,
		Changed: []string{"web/src/app/new/page.tsx", "web/src/app/old/page.tsx"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []Page{
		{File: "src/app/new/page.tsx", Route: "/new", After: true},
		{File: "src/app/old/page.tsx", Route: "/old", Before: true},
	}
	if !reflect.DeepEqual(pages, want) {
		t.Fatalf("pages = %+v", pages)
	}
}

// TestATemplatePageTakesItsValuesFromTheFixtures visits a template at the
// escaped values the fixtures give, a catch-all over several segments.
func TestATemplatePageTakesItsValuesFromTheFixtures(t *testing.T) {
	t.Parallel()
	inv := webInventory(t, "template /tasks/{id} src/app/tasks/[id]/page.tsx",
		"template /docs/{slug...}/edit src/app/docs/[...slug]/edit/page.tsx")
	fixtures, err := ParseFixtures([]byte(`{"/tasks/{id}": {"id": "a b"}, "/docs/{slug...}/edit": {"slug": "guide/intro"}}`))
	if err != nil {
		t.Fatal(err)
	}
	pages, err := Select(Selection{Project: "web", Base: inv, Head: inv, Changed: []string{"web/src/lib/x.ts"}}, fixtures)
	if err != nil {
		t.Fatal(err)
	}
	if pages[0].Path != "/tasks/a%20b" || pages[1].Path != "/docs/guide/intro/edit" {
		t.Fatalf("paths = %q, %q", pages[0].Path, pages[1].Path)
	}
	if _, err := ParseFixtures([]byte(`{"/tasks/{id}": "1"}`)); err == nil {
		t.Fatal("a fixture that is no object of values was accepted")
	}
	spectest.Proves(t, screensFeature, touchedPages, "a-template-page-takes-its-values-from-the-fixtures")
}

// TestATouchedTemplateWithoutAValueIsRefused names every template and
// parameter the fixtures lack.
func TestATouchedTemplateWithoutAValueIsRefused(t *testing.T) {
	t.Parallel()
	inv := webInventory(t, "template /tasks/{id} src/app/tasks/[id]/page.tsx",
		"template /{org}/{repo} src/app/[org]/[repo]/page.tsx")
	_, err := Select(Selection{Project: "web", Base: inv, Head: inv, Changed: []string{"web/src/lib/x.ts"}},
		map[string]map[string]string{"/{org}/{repo}": {"org": "a"}, "/tasks/{id}": {"id": "x/y"}})
	if err == nil {
		t.Fatal("a touched template without a value was accepted")
	}
	for _, want := range []string{"/tasks/{id} (id)", "/{org}/{repo} (repo)"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %s", err, want)
		}
	}
	spectest.Proves(t, screensFeature, touchedPages, "a-touched-template-without-a-value-is-refused")
}
