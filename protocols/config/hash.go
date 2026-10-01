package config

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
)

// HashPrefix is the canonical prefix for schema hashes.
const HashPrefix = "sha256:"

// HashLength is the number of hex characters kept from the SHA-256 digest.
const HashLength = 16

// ComputeSchemaHash computes the canonical hash for a set of config blocks.
//
// The algorithm is:
//  1. Normalize blocks into a canonical form (sorted paths, sorted fields).
//  2. Serialize to JSON with sorted keys (Go json.Marshal guarantees this for structs).
//  3. SHA-256 hash the JSON bytes.
//  4. Return "sha256:" + first 16 hex characters.
//
// Both Go and TS extractors must implement this exact algorithm to produce
// identical hashes for equivalent inputs.
func ComputeSchemaHash(blocks []Block) string {
	canonical := CanonicalizeBlocks(blocks)
	data, _ := json.Marshal(canonical)
	h := sha256.Sum256(data)
	return fmt.Sprintf("%s%x", HashPrefix, h[:HashLength/2])
}

// CanonicalizeBlocks returns a copy of the blocks sorted by path,
// with fields within each block sorted by name. This ensures that
// hash computation is deterministic regardless of extraction order.
//
// The canonicalizer recurses through every composite slot (Fields, Items,
// Values) so nested objects, array items, and map values participate in the
// hash. Both Go and TS extractors must apply the exact same recursive rule.
func CanonicalizeBlocks(blocks []Block) []Block {
	out := make([]Block, len(blocks))
	for i, b := range blocks {
		out[i] = Block{Path: b.Path, Fields: canonicalizeFields(b.Fields)}
	}
	sort.Slice(out, func(a, c int) bool {
		return out[a].Path < out[c].Path
	})
	return out
}

func canonicalizeFields(fields []FieldSchema) []FieldSchema {
	out := make([]FieldSchema, len(fields))
	for i, f := range fields {
		out[i] = canonicalizeField(f)
	}
	sort.Slice(out, func(a, c int) bool {
		return out[a].Name < out[c].Name
	})
	return out
}

func canonicalizeField(f FieldSchema) FieldSchema {
	if len(f.Constraints) > 1 {
		sorted := make([]string, len(f.Constraints))
		copy(sorted, f.Constraints)
		sort.Strings(sorted)
		f.Constraints = sorted
	}
	if len(f.Fields) > 0 {
		f.Fields = canonicalizeFields(f.Fields)
	}
	if f.Items != nil {
		child := canonicalizeField(*f.Items)
		f.Items = &child
	}
	if f.Values != nil {
		child := canonicalizeField(*f.Values)
		f.Values = &child
	}
	return f
}
