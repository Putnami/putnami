// Package catalog reads the Bun workspace catalogs declared in a root
// package.json. Bun lets a member depend on a shared version through the
// "catalog:" protocol; the concrete version lives in a catalog on the root
// manifest. This package is the single source of truth for discovering those
// catalogs — both the install/upgrade tooling and the npm publisher resolve
// "catalog:" specifiers through it.
package catalog

import (
	"encoding/json"
	"strings"
)

// Protocol is the Bun dependency protocol that resolves a version from the
// workspace catalog. The bare form "catalog:" selects the default catalog;
// "catalog:<name>" selects a named catalog.
const Protocol = "catalog:"

// Catalogs is a read-only view over the Bun catalogs declared in a root
// package.json. Bun accepts catalogs in two placements — top-level
// ("catalog"/"catalogs") and nested under the workspaces object
// ("workspaces.catalog"/"workspaces.catalogs") — and Parse reads whichever
// exists. The default catalog backs the bare "catalog:" protocol; named
// catalogs back "catalog:<name>". The placement fields (DefaultAtTop/
// NamedAtTop/WsObj) let write-capable callers round-trip entries back to where
// they came from.
type Catalogs struct {
	Default      map[string]string            // default catalog entries (name → version)
	Named        map[string]map[string]string // named catalog name → entries
	DefaultAtTop bool                         // default catalog lives at top-level "catalog"
	NamedAtTop   bool                         // named catalogs live at top-level "catalogs"
	WsObj        map[string]json.RawMessage   // workspaces object (nil for the array form)
}

// Parse discovers the catalogs declared in pkg without mutating it.
func Parse(pkg map[string]json.RawMessage) *Catalogs {
	c := &Catalogs{}

	if cat := UnmarshalStringMap(pkg["catalog"]); cat != nil {
		c.Default = cat
		c.DefaultAtTop = true
	}
	if named := unmarshalNamedCatalogs(pkg["catalogs"]); named != nil {
		c.Named = named
		c.NamedAtTop = true
	}

	// "workspaces" is either an array (no nested catalogs) or an object that may
	// carry "catalog"/"catalogs". Unmarshalling an array into a map fails, which
	// is how the array form is detected.
	if raw, ok := pkg["workspaces"]; ok {
		var wsObj map[string]json.RawMessage
		if json.Unmarshal(raw, &wsObj) == nil && wsObj != nil {
			c.WsObj = wsObj
			if c.Default == nil {
				if cat := UnmarshalStringMap(wsObj["catalog"]); cat != nil {
					c.Default = cat
					c.DefaultAtTop = false
				}
			}
			if c.Named == nil {
				if named := unmarshalNamedCatalogs(wsObj["catalogs"]); named != nil {
					c.Named = named
					c.NamedAtTop = false
				}
			}
		}
	}
	return c
}

// Present reports whether the manifest declares any catalog.
func (c *Catalogs) Present() bool {
	return c.Default != nil || len(c.Named) > 0
}

// Lookup returns the version recorded for name in any catalog, preferring the
// default catalog, and whether it was found.
func (c *Catalogs) Lookup(name string) (string, bool) {
	if c.Default != nil {
		if v, ok := c.Default[name]; ok {
			return v, true
		}
	}
	for _, cat := range c.Named {
		if v, ok := cat[name]; ok {
			return v, true
		}
	}
	return "", false
}

// LookupIn returns the version recorded for name in the selected catalog. The
// empty catalog name selects the default catalog; non-empty names select
// "catalogs.<name>".
func (c *Catalogs) LookupIn(catalogName, name string) (string, bool) {
	if catalogName == "" {
		if c.Default == nil {
			return "", false
		}
		v, ok := c.Default[name]
		return v, ok
	}
	if c.Named == nil {
		return "", false
	}
	cat := c.Named[catalogName]
	if cat == nil {
		return "", false
	}
	v, ok := cat[name]
	return v, ok
}

// NameFromSpec reports whether spec uses the catalog: protocol and, if so,
// returns the selected catalog name ("" for the default catalog).
func NameFromSpec(spec string) (string, bool) {
	if !strings.HasPrefix(spec, Protocol) {
		return "", false
	}
	return strings.TrimPrefix(spec, Protocol), true
}

// UnmarshalStringMap decodes a JSON object of strings, or returns nil when raw is
// empty or not such an object.
func UnmarshalStringMap(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

func unmarshalNamedCatalogs(raw json.RawMessage) map[string]map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]map[string]string
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}
