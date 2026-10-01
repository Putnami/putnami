package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/catalog"
)

const (
	// putnamiScopePrefix marks framework packages distributed under the
	// @putnami npm scope.
	putnamiScopePrefix = "@putnami/"
	// catalogProtocol is the Bun dependency protocol that resolves a version
	// from the workspace catalog. The bare form "catalog:" selects the default
	// catalog; "catalog:<name>" selects a named catalog.
	catalogProtocol = catalog.Protocol
)

// readRootManifest loads the workspace root package.json into a key→raw map,
// plus the order its top-level keys appeared in on disk. A missing file
// yields a minimal private manifest so callers can still seed catalog or
// dependency entries from scratch. The order slice must be threaded through
// to writeRootManifest so a rewrite doesn't scramble unmanaged top-level keys
// (overrides, scripts, packageManager, ...) into map[string]json.RawMessage's
// alphabetical iteration order — the churn that let an overrides drop
// drop slip by unnoticed.
func readRootManifest(path string) (map[string]json.RawMessage, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("read package.json: %w", err)
		}
		data = []byte(`{"private":true}`)
	}
	var pkg map[string]json.RawMessage
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, nil, fmt.Errorf("parse package.json: %w", err)
	}
	order, err := topLevelKeyOrder(data)
	if err != nil {
		return nil, nil, fmt.Errorf("parse package.json key order: %w", err)
	}
	return pkg, order, nil
}

// topLevelKeyOrder walks data's JSON token stream to recover the order its
// top-level object keys appear in, information json.Unmarshal into a map
// discards. It hand-rolls this over encoding/json's Decoder/Token API rather
// than pulling in an ordered-JSON dependency (this repo forbids new external
// libs). data is expected to already be valid JSON (the caller unmarshals it
// first); a non-object top level yields a nil order with no error.
func topLevelKeyOrder(data []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, nil
	}

	var order []string
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("unexpected top-level key token %v", keyTok)
		}
		order = append(order, key)
		// Decode (rather than skip) the value: whatever its shape — object,
		// array, or scalar — this advances the decoder past it so the next
		// Token() call lands on the following key.
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// writeRootManifest serializes pkg back to path with the 2-space indentation
// Bun and npm use, plus a trailing newline. Top-level keys are emitted in
// order: every key from order that's still present in pkg keeps its original
// relative position; any key in pkg that isn't in order (freshly added by the
// tooling — e.g. a seeded "catalog") is appended afterward, sorted
// alphabetically so output stays deterministic. This is what keeps unmanaged
// top-level keys (overrides, scripts, packageManager, ...) untouched across a
// rewrite instead of being reordered alphabetically by Go's map marshaling.
func writeRootManifest(path string, pkg map[string]json.RawMessage, order []string) error {
	out, err := json.MarshalIndent(orderedRawObject{keys: manifestKeyOrder(pkg, order), values: pkg}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal package.json: %w", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0644); err != nil {
		return fmt.Errorf("write package.json: %w", err)
	}
	return nil
}

// manifestKeyOrder returns the top-level keys of pkg to emit: every key in
// order that pkg still has, in its original relative order, followed by any
// key pkg has that order doesn't (a brand-new key the tooling added), sorted
// alphabetically.
func manifestKeyOrder(pkg map[string]json.RawMessage, order []string) []string {
	keys := make([]string, 0, len(pkg))
	seen := make(map[string]bool, len(order))
	for _, k := range order {
		if _, ok := pkg[k]; !ok {
			continue // removed by the tooling (e.g. an emptied overrides block)
		}
		if seen[k] {
			continue // tolerate a (technically invalid) duplicate source key
		}
		seen[k] = true
		keys = append(keys, k)
	}
	var extra []string
	for k := range pkg {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	return append(keys, extra...)
}

// orderedRawObject implements json.Marshaler to emit a map[string]json.RawMessage
// with a caller-chosen top-level key order — the write-side counterpart to
// topLevelKeyOrder. Marshaling a plain map always sorts keys alphabetically,
// which is the reordering writeRootManifest exists to avoid.
type orderedRawObject struct {
	keys   []string
	values map[string]json.RawMessage
}

func (o orderedRawObject) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, fmt.Errorf("marshal key %q: %w", k, err)
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(o.values[k])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// catalogModel is a read/write view over the Bun catalogs declared in a root
// package.json. Bun accepts catalogs in two placements — top-level
// ("catalog"/"catalogs") and nested under the workspaces object
// ("workspaces.catalog"/"workspaces.catalogs") — and this model reads whichever
// exists and writes each entry back to where it came from. The default catalog
// backs the bare "catalog:" protocol; named catalogs back "catalog:<name>".
type catalogModel struct {
	defaultCat   map[string]string            // default catalog entries (name → version)
	named        map[string]map[string]string // named catalog name → entries
	defaultAtTop bool                         // default catalog lives at top-level "catalog"
	namedAtTop   bool                         // named catalogs live at top-level "catalogs"
	wsObj        map[string]json.RawMessage   // workspaces object (nil for the array form)
}

// parseCatalogModel discovers the catalogs declared in pkg without mutating it.
// Discovery is delegated to internal/catalog — the single source of truth for
// Bun catalog placement, shared with the npm publisher — while this model layers
// on the write-back behavior the install/upgrade tooling needs.
func parseCatalogModel(pkg map[string]json.RawMessage) *catalogModel {
	c := catalog.Parse(pkg)
	return &catalogModel{
		defaultCat:   c.Default,
		named:        c.Named,
		defaultAtTop: c.DefaultAtTop,
		namedAtTop:   c.NamedAtTop,
		wsObj:        c.WsObj,
	}
}

// present reports whether the manifest declares any catalog.
func (m *catalogModel) present() bool {
	return m.defaultCat != nil || len(m.named) > 0
}

// scopedNames returns every catalog entry name — across the default and all
// named catalogs — that carries the given scope prefix. A catalog entry is
// version policy the workspace committed to, so the upgrader uses this to keep
// every declared @putnami/* entry current even when no member imports it.
func (m *catalogModel) scopedNames(prefix string) []string {
	seen := map[string]bool{}
	for name := range m.defaultCat {
		if strings.HasPrefix(name, prefix) {
			seen[name] = true
		}
	}
	for _, cat := range m.named {
		for name := range cat {
			if strings.HasPrefix(name, prefix) {
				seen[name] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	return out
}

// lookup returns the version recorded for name in any catalog, preferring the
// default catalog, and whether it was found.
func (m *catalogModel) lookup(name string) (string, bool) {
	if m.defaultCat != nil {
		if v, ok := m.defaultCat[name]; ok {
			return v, true
		}
	}
	for _, cat := range m.named {
		if v, ok := cat[name]; ok {
			return v, true
		}
	}
	return "", false
}

// lookupIn returns the version recorded for name in the selected catalog. The
// empty catalog name selects the default catalog; non-empty names select
// "catalogs.<name>".
func (m *catalogModel) lookupIn(catalogName, name string) (string, bool) {
	if catalogName == "" {
		if m.defaultCat == nil {
			return "", false
		}
		v, ok := m.defaultCat[name]
		return v, ok
	}
	if m.named == nil {
		return "", false
	}
	cat := m.named[catalogName]
	if cat == nil {
		return "", false
	}
	v, ok := cat[name]
	return v, ok
}

// set updates name to version in every catalog that already contains it and
// reports whether name was present in at least one catalog.
func (m *catalogModel) set(name, version string) bool {
	placed := false
	if m.defaultCat != nil {
		if _, ok := m.defaultCat[name]; ok {
			m.defaultCat[name] = version
			placed = true
		}
	}
	for _, cat := range m.named {
		if _, ok := cat[name]; ok {
			cat[name] = version
			placed = true
		}
	}
	return placed
}

// addToDefault inserts name→version into the default catalog, creating it when
// the manifest had only named catalogs (or none). A freshly created default
// catalog is placed under the workspaces object for the object form, otherwise
// at the top level.
func (m *catalogModel) addToDefault(name, version string) {
	if m.defaultCat == nil {
		m.defaultCat = map[string]string{}
		m.defaultAtTop = m.wsObj == nil
	}
	m.defaultCat[name] = version
}

// addToCatalog inserts name→version into the selected catalog. The empty
// catalog name selects the default catalog; non-empty names select or create a
// named catalog.
func (m *catalogModel) addToCatalog(catalogName, name, version string) {
	if catalogName == "" {
		m.addToDefault(name, version)
		return
	}
	if m.named == nil {
		m.named = map[string]map[string]string{}
		m.namedAtTop = m.newCatalogsAtTop()
	}
	if m.named[catalogName] == nil {
		m.named[catalogName] = map[string]string{}
	}
	m.named[catalogName][name] = version
}

func (m *catalogModel) newCatalogsAtTop() bool {
	if m.named != nil {
		return m.namedAtTop
	}
	if m.defaultCat != nil {
		return m.defaultAtTop
	}
	return m.wsObj == nil
}

// writeBack re-serializes the (possibly mutated) catalogs into pkg, preserving
// each catalog's original placement.
func (m *catalogModel) writeBack(pkg map[string]json.RawMessage) error {
	if m.defaultCat != nil {
		raw, err := json.Marshal(m.defaultCat)
		if err != nil {
			return fmt.Errorf("marshal catalog: %w", err)
		}
		if m.defaultAtTop {
			pkg["catalog"] = raw
		} else if m.wsObj != nil {
			m.wsObj["catalog"] = raw
		}
	}
	if m.named != nil {
		raw, err := json.Marshal(m.named)
		if err != nil {
			return fmt.Errorf("marshal catalogs: %w", err)
		}
		if m.namedAtTop {
			pkg["catalogs"] = raw
		} else if m.wsObj != nil {
			m.wsObj["catalogs"] = raw
		}
	}
	if m.wsObj != nil {
		raw, err := json.Marshal(m.wsObj)
		if err != nil {
			return fmt.Errorf("marshal workspaces: %w", err)
		}
		pkg["workspaces"] = raw
	}
	return nil
}

// stripPutnamiEntries removes every @putnami/* key from the named root object
// ("dependencies" or "overrides"), preserving all other entries, and reports
// whether anything was removed. The object is dropped entirely when it becomes
// empty so a catalog-mode manifest carries no redundant @putnami/* version
// policy outside the catalog. For "overrides", redirect values (see
// isRedirectOverride) are preserved — only plain version pins are stripped.
func stripPutnamiEntries(pkg map[string]json.RawMessage, key string, dryRun bool, emit *jsonl.Emitter) (bool, error) {
	raw, ok := pkg[key]
	if !ok {
		return false, nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return false, fmt.Errorf("parse %s: %w", key, err)
	}

	removed := false
	for name, val := range entries {
		if !strings.HasPrefix(name, putnamiScopePrefix) {
			continue
		}
		// A redirect override such as "catalog:" points a package's
		// workspace:* spec at the workspace catalog — load-bearing, not the
		// redundant version pin that EOVERRIDE dedup targets. Preserve
		// it so reconciliation never strips it out from under bun install.
		if key == "overrides" && isRedirectOverride(val) {
			continue
		}
		removed = true
		if dryRun {
			emit.Log("info", fmt.Sprintf("Would remove %s from %s (catalog is the source of truth)", name, key))
			continue
		}
		delete(entries, name)
		emit.Log("info", fmt.Sprintf("Removed %s from %s (catalog is the source of truth)", name, key))
	}

	if !removed || dryRun {
		return removed, nil
	}
	if len(entries) == 0 {
		delete(pkg, key)
		return true, nil
	}
	out, err := json.Marshal(entries)
	if err != nil {
		return false, fmt.Errorf("marshal %s: %w", key, err)
	}
	pkg[key] = out
	return true, nil
}

// isRedirectOverride reports whether an override value is a protocol redirect
// (e.g. "catalog:", "catalog:framework", "workspace:*", "npm:...") or a "$name"
// dependency alias rather than a plain version pin. Redirect overrides are
// load-bearing — notably "catalog:" points a package's workspace:* spec at the
// workspace catalog — so reconciliation must preserve them; only
// redundant version pins are the EOVERRIDE bait worth stripping.
func isRedirectOverride(raw json.RawMessage) bool {
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		// Non-string values (e.g. nested override objects) are never plain
		// version pins — preserve them.
		return true
	}
	return strings.ContainsRune(v, ':') || strings.HasPrefix(v, "$")
}
