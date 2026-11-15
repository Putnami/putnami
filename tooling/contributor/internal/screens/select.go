package screens

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"
)

// InventoryPath is where a project commits its route inventory, relative to
// the project directory.
const InventoryPath = "schema/http-routes.json"

// Inventory is the part of a putnami.http-routes.v1 document that selects
// pages: each route's match, path, methods, source kind and evidence path.
type Inventory struct {
	Routes []Route `json:"routes"`
}

// Route is one route of an inventory.
type Route struct {
	Match      string   `json:"match"`
	Path       string   `json:"path"`
	Methods    []string `json:"methods"`
	Provenance struct {
		SourceKind   string `json:"sourceKind"`
		EvidencePath string `json:"evidencePath"`
	} `json:"provenance"`
}

// ParseInventory reads a route inventory.
func ParseInventory(data []byte) (*Inventory, error) {
	var inventory Inventory
	if err := json.Unmarshal(data, &inventory); err != nil {
		return nil, fmt.Errorf("the route inventory is not JSON: %w", err)
	}
	return &inventory, nil
}

// IsPage reports whether a route is a page: a file-route that serves GET.
func (r Route) IsPage() bool {
	return r.Provenance.SourceKind == "file-route" && slices.Contains(r.Methods, "GET")
}

// Pages returns the inventory's page routes in inventory order. A nil
// inventory has none.
func (inv *Inventory) Pages() []Route {
	if inv == nil {
		return nil
	}
	var pages []Route
	for _, route := range inv.Routes {
		if route.IsPage() {
			pages = append(pages, route)
		}
	}
	return pages
}

// HasPage reports whether the inventory holds a page at the route path.
func (inv *Inventory) HasPage(route string) bool {
	for _, page := range inv.Pages() {
		if page.Path == route {
			return true
		}
	}
	return false
}

// Selection is what selects a project's touched pages.
type Selection struct {
	// Project is the project's workspace-relative directory.
	Project string
	// Changed are the workspace-relative paths the change touches.
	Changed []string
	// Base and Head are the project's route inventories at the merge base
	// and at the head; nil where the side has none.
	Base, Head *Inventory
	// Routes are the routes the run names with --route.
	Routes []string
}

// TouchedRoutes returns the page routes a change touches, without their
// visited paths: a page file the change touches is captured at its first
// route, the head's when the head holds the page, and every --route is
// added. A changed file under the project's src/ that is no route's evidence
// touches every page.
func TouchedRoutes(sel Selection) ([]Page, error) {
	prefix := strings.TrimSuffix(path.Clean(sel.Project), "/") + "/"
	if sel.Project == "." || sel.Project == "" {
		prefix = ""
	}
	evidence := map[string]bool{}
	for _, inv := range []*Inventory{sel.Head, sel.Base} {
		if inv == nil {
			continue
		}
		for _, route := range inv.Routes {
			if route.Provenance.EvidencePath != "" {
				evidence[route.Provenance.EvidencePath] = true
			}
		}
	}
	// The page files in order, each with its first route: the head's pages,
	// then the pages only the base holds.
	var files []string
	first := map[string]Route{}
	for _, route := range append(sel.Head.Pages(), sel.Base.Pages()...) {
		file := route.Provenance.EvidencePath
		if _, seen := first[file]; seen {
			continue
		}
		first[file] = route
		files = append(files, file)
	}
	touchAll := false
	touched := map[string]bool{}
	for _, changed := range sel.Changed {
		rel, ok := strings.CutPrefix(changed, prefix)
		if !ok {
			continue
		}
		if _, isPage := first[rel]; isPage {
			touched[rel] = true
			continue
		}
		if strings.HasPrefix(rel, "src/") && !evidence[rel] {
			touchAll = true
		}
	}
	var pages []Page
	seen := map[string]bool{}
	add := func(file string, route Route) {
		if seen[route.Path] {
			return
		}
		seen[route.Path] = true
		pages = append(pages, Page{File: file, Route: route.Path,
			Before: sel.Base.HasPage(route.Path), After: sel.Head.HasPage(route.Path)})
	}
	for _, file := range files {
		if touchAll || touched[file] {
			add(file, first[file])
		}
	}
	for _, named := range sel.Routes {
		route, ok := findPage(named, sel.Head, sel.Base)
		if !ok {
			return nil, fmt.Errorf("--route %s is no page of %s", named, sel.Project)
		}
		add(route.Provenance.EvidencePath, route)
	}
	return pages, nil
}

func findPage(path string, inventories ...*Inventory) (Route, bool) {
	for _, inv := range inventories {
		for _, page := range inv.Pages() {
			if page.Path == path {
				return page, true
			}
		}
	}
	return Route{}, false
}

// Select returns the touched pages with the path each is visited at: an
// exact route as is, a template with each parameter taken from fixtures,
// keyed by the template path. A touched template with a missing value is
// refused, and the error names every one.
func Select(sel Selection, fixtures map[string]map[string]string) ([]Page, error) {
	pages, err := TouchedRoutes(sel)
	if err != nil {
		return nil, err
	}
	var missing []string
	for i := range pages {
		visit, lacking := Instantiate(pages[i].Route, fixtures[pages[i].Route])
		if len(lacking) > 0 {
			missing = append(missing, fmt.Sprintf("%s (%s)", pages[i].Route, strings.Join(lacking, ", ")))
			continue
		}
		pages[i].Path = visit
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("--fixtures gives no value for the touched template routes %s", strings.Join(missing, "; "))
	}
	return pages, nil
}

// Instantiate fills a route path's parameters from values: a {name} segment
// takes one escaped segment, and a {name...} segment one or more, split on
// "/". It returns the parameters values lacks.
func Instantiate(route string, values map[string]string) (string, []string) {
	segments := strings.Split(route, "/")
	var lacking []string
	for i, segment := range segments {
		if !strings.HasPrefix(segment, "{") || !strings.HasSuffix(segment, "}") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(segment, "{"), "}")
		catchAll := strings.HasSuffix(name, "...")
		name = strings.TrimSuffix(name, "...")
		value := values[name]
		if value == "" || (!catchAll && strings.Contains(value, "/")) {
			lacking = append(lacking, name)
			continue
		}
		if !catchAll {
			segments[i] = url.PathEscape(value)
			continue
		}
		parts := strings.Split(strings.Trim(value, "/"), "/")
		for j, part := range parts {
			parts[j] = url.PathEscape(part)
		}
		segments[i] = strings.Join(parts, "/")
	}
	return strings.Join(segments, "/"), lacking
}

// ParseFixtures reads a fixtures document: an object keyed by template route
// path, each an object of parameter values.
func ParseFixtures(data []byte) (map[string]map[string]string, error) {
	var fixtures map[string]map[string]string
	if err := json.Unmarshal(data, &fixtures); err != nil {
		return nil, fmt.Errorf("the fixtures are not an object of route templates to parameter values: %w", err)
	}
	return fixtures, nil
}
