// Package project provides utilities for reading and writing package.json
// and resolving project configuration.
package project

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PackageJSON represents a package.json with both typed fields and raw preservation.
type PackageJSON struct {
	Name                 string            `json:"name,omitempty"`
	Version              string            `json:"version,omitempty"`
	Main                 string            `json:"main,omitempty"`
	Types                string            `json:"types,omitempty"`
	Bin                  json.RawMessage   `json:"bin,omitempty"`
	Exports              json.RawMessage   `json:"exports,omitempty"`
	Dependencies         map[string]string `json:"dependencies,omitempty"`
	DevDependencies      map[string]string `json:"devDependencies,omitempty"`
	PeerDependencies     map[string]string `json:"peerDependencies,omitempty"`
	OptionalDependencies map[string]string `json:"optionalDependencies,omitempty"`

	// Raw preserves all fields for round-trip fidelity
	Raw map[string]json.RawMessage `json:"-"`
}

// readPackageJSON reads and parses a package.json file.
func readPackageJSON(path string) (*PackageJSON, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// Parse raw map first
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing package.json: %w", err)
	}

	// Parse typed fields
	var pkg PackageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, fmt.Errorf("parsing package.json: %w", err)
	}
	pkg.Raw = raw

	return &pkg, nil
}

// ReadPackageJSONSafe reads package.json, returning nil if not found.
func ReadPackageJSONSafe(path string) *PackageJSON {
	pkg, err := readPackageJSON(path)
	if err != nil {
		return nil
	}
	return pkg
}

// exportsPlaceholder is a sentinel value substituted into the marshaled
// package.json in place of the exports field, then swapped for the raw
// exports bytes so their internal key ordering survives serialization.
// TypeScript requires the "types" condition to appear first within an
// exports condition object, but json.MarshalIndent sorts map keys
// alphabetically — so exports must be written as pre-ordered raw bytes.
const exportsPlaceholder = "\x00putnami-exports-placeholder\x00"

// WritePackageJSON writes a package.json preserving unknown fields.
func WritePackageJSON(path string, pkg *PackageJSON) error {
	// Start from raw to preserve unknown fields
	out := make(map[string]any)
	for k, v := range pkg.Raw {
		var parsed any
		_ = json.Unmarshal(v, &parsed)
		out[k] = parsed
	}

	// Override with typed fields
	if pkg.Name != "" {
		out["name"] = pkg.Name
	}
	if pkg.Version != "" {
		out["version"] = pkg.Version
	}
	if pkg.Main != "" {
		out["main"] = pkg.Main
	}
	if pkg.Types != "" {
		out["types"] = pkg.Types
	}
	if pkg.Bin != nil {
		var bin any
		_ = json.Unmarshal(pkg.Bin, &bin)
		out["bin"] = bin
	}
	// Preserve the exact key ordering of the raw exports bytes: substitute a
	// placeholder, then swap in the re-indented raw bytes after marshaling so
	// json.MarshalIndent cannot re-sort the condition keys (notably "types",
	// which must stay first for TypeScript).
	var exportsRaw []byte
	if pkg.Exports != nil {
		exportsRaw = indentRaw(pkg.Exports, "  ", "  ")
		out["exports"] = exportsPlaceholder
	}
	if pkg.Dependencies != nil {
		out["dependencies"] = pkg.Dependencies
	}
	if pkg.DevDependencies != nil {
		out["devDependencies"] = pkg.DevDependencies
	}
	if pkg.PeerDependencies != nil {
		out["peerDependencies"] = pkg.PeerDependencies
	}
	if pkg.OptionalDependencies != nil {
		out["optionalDependencies"] = pkg.OptionalDependencies
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}

	if exportsRaw != nil {
		// The placeholder was marshaled as a JSON string (quoted, escaped).
		quoted, qerr := json.Marshal(exportsPlaceholder)
		if qerr == nil {
			data = bytes.Replace(data, quoted, exportsRaw, 1)
		}
	}

	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}

// indentRaw returns raw JSON re-indented to sit at the given nesting, using
// the provided prefix on continuation lines and indent per level. Key order
// is preserved. On any error it falls back to the raw bytes unchanged.
func indentRaw(raw json.RawMessage, prefix, indent string) []byte {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, prefix, indent); err != nil {
		return raw
	}
	return buf.Bytes()
}

// GetExportsMap parses exports as a map. Returns nil if not an object.
func (p *PackageJSON) GetExportsMap() map[string]json.RawMessage {
	if p.Exports == nil {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(p.Exports, &m) != nil {
		return nil
	}
	return m
}

// GetBinMap parses bin as a map. Returns nil if not an object.
func (p *PackageJSON) GetBinMap() map[string]string {
	if p.Bin == nil {
		return nil
	}
	var m map[string]string
	if json.Unmarshal(p.Bin, &m) == nil {
		return m
	}
	// Might be a string (single bin)
	return nil
}

// GetBinString returns bin as a string if it's a single value.
func (p *PackageJSON) GetBinString() string {
	if p.Bin == nil {
		return ""
	}
	var s string
	if json.Unmarshal(p.Bin, &s) == nil {
		return s
	}
	return ""
}

// ResolveExportPath resolves a specific export condition path (e.g., "./serve").
func ResolveExportPath(exports json.RawMessage, key string) string {
	if exports == nil {
		return ""
	}

	var m map[string]json.RawMessage
	if json.Unmarshal(exports, &m) != nil {
		return ""
	}

	raw, ok := m[key]
	if !ok {
		return ""
	}

	// Try string first
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}

	// Try object with conditions
	var conditions map[string]string
	if json.Unmarshal(raw, &conditions) == nil {
		// Priority: default > bun > node
		for _, cond := range []string{"default", "bun", "node"} {
			if v, ok := conditions[cond]; ok {
				return v
			}
		}
	}

	return ""
}

// FileExists checks if a path exists.
func FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// StripPreReleaseSuffix removes the pre-release portion from a semver string.
func StripPreReleaseSuffix(version string) string {
	if idx := strings.IndexByte(version, '-'); idx >= 0 {
		return version[:idx]
	}
	return version
}

// RelativePath computes the relative path from base to target.
func RelativePath(base, target string) string {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return target
	}
	return rel
}
