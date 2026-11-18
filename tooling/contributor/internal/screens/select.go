package screens

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"

	"go.putnami.dev/protocol/screenshots"
)

// InventoryPath is where a project commits its route inventory, relative to
// the project directory.
const InventoryPath = "schema/http-routes.json"

// Inventory is the part of a putnami.http-routes.v1 document that selects
// pages: each route's match, path, methods, source kind, evidence path, and
// the requirements a page route proves.
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
	// Proves lists the feature requirements the page proves, each with a
	// concrete path that renders it with fixture data.
	Proves []RouteProof `json:"proves,omitempty"`
}

// RouteProof is one requirement a page route proves, and the path, at, that
// renders the page with fixture data.
type RouteProof struct {
	Feature     string `json:"feature"`
	Requirement string `json:"requirement"`
	At          string `json:"at"`
}

// Key returns the requirement the proof names.
func (p RouteProof) Key() Requirement {
	return Requirement{Feature: p.Feature, Requirement: p.Requirement}
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
	_, ok := inv.page(route)
	return ok
}

// page returns the page route at a route path.
func (inv *Inventory) page(route string) (Route, bool) {
	for _, page := range inv.Pages() {
		if page.Path == route {
			return page, true
		}
	}
	return Route{}, false
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
	// Touched are the requirements the change adds or changes.
	Touched map[Requirement]bool
}

// TouchedRoutes returns the page routes a change touches, without their
// visited paths: each head page route whose proves names a touched
// requirement, in inventory order; then each page whose file the change
// touches, at its first route, the head's when the head holds the page; then
// every --route. No other changed file touches a page. Each page names the
// requirements its head route proves.
func TouchedRoutes(sel Selection) ([]Page, error) {
	prefix := strings.TrimSuffix(path.Clean(sel.Project), "/") + "/"
	if sel.Project == "." || sel.Project == "" {
		prefix = ""
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
	touched := map[string]bool{}
	for _, changed := range sel.Changed {
		if rel, ok := strings.CutPrefix(changed, prefix); ok {
			touched[rel] = true
		}
	}
	var pages []Page
	seen := map[string]bool{}
	add := func(file string, route Route) {
		if seen[route.Path] {
			return
		}
		seen[route.Path] = true
		page := Page{File: file, Route: route.Path, Before: sel.Base.HasPage(route.Path), After: sel.Head.HasPage(route.Path)}
		if head, ok := sel.Head.page(route.Path); ok {
			page.Proves = proofsOf(head)
		}
		pages = append(pages, page)
	}
	for _, route := range sel.Head.Pages() {
		if provesAny(route, sel.Touched) {
			add(route.Provenance.EvidencePath, route)
		}
	}
	for _, file := range files {
		if touched[file] {
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

// Proves reports whether a page route of the inventory proves one of the
// requirements. A nil inventory proves none.
func (inv *Inventory) Proves(requirements map[Requirement]bool) bool {
	for _, route := range inv.Pages() {
		if provesAny(route, requirements) {
			return true
		}
	}
	return false
}

// provesAny reports whether a route proves one of the requirements.
func provesAny(route Route, requirements map[Requirement]bool) bool {
	for _, proof := range route.Proves {
		if requirements[proof.Key()] {
			return true
		}
	}
	return false
}

// proofsOf returns the requirements a route proves, each once, in the
// route's order; nil when it proves none.
func proofsOf(route Route) []screenshots.Proof {
	var proofs []screenshots.Proof
	seen := map[Requirement]bool{}
	for _, proof := range route.Proves {
		if seen[proof.Key()] {
			continue
		}
		seen[proof.Key()] = true
		proofs = append(proofs, screenshots.Proof{Feature: proof.Feature, Requirement: proof.Requirement})
	}
	return proofs
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

// Select returns the touched pages with the path each is visited at: the
// route filled with its fixtures, keyed by the template path, when the
// fixtures name the route; else the at of a touched requirement the head
// route proves, else the at of its first proof; else the route filled with
// no value, which an exact route needs. A touched template with a missing
// value is refused, and the error names every one.
func Select(sel Selection, fixtures map[string]map[string]string) ([]Page, error) {
	pages, err := TouchedRoutes(sel)
	if err != nil {
		return nil, err
	}
	var missing []string
	for i := range pages {
		values, named := fixtures[pages[i].Route]
		if !named {
			if at := proofPath(sel, pages[i].Route); at != "" {
				pages[i].Path = at
				continue
			}
		}
		visit, lacking := Instantiate(pages[i].Route, values)
		if len(lacking) > 0 {
			missing = append(missing, fmt.Sprintf("%s (%s)", pages[i].Route, strings.Join(lacking, ", ")))
			continue
		}
		pages[i].Path = visit
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("--fixtures gives no value, and no proves gives an at, for the touched template routes %s", strings.Join(missing, "; "))
	}
	return pages, nil
}

// proofPath returns the at a page is visited at: that of the first
// requirement its head route proves that the change touches, else that of
// its first proof; "" when the head route proves nothing.
func proofPath(sel Selection, route string) string {
	head, ok := sel.Head.page(route)
	if !ok || len(head.Proves) == 0 {
		return ""
	}
	for _, proof := range head.Proves {
		if sel.Touched[proof.Key()] {
			return proof.At
		}
	}
	return head.Proves[0].At
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
