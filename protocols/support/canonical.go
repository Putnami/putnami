package support

import (
	"encoding/json"
	"sort"
)

// CanonicalCatalog returns a sorted copy without mutating caller-owned data.
// Entries sort by kind and then id so equivalent catalogs have identical bytes.
func CanonicalCatalog(input *Catalog) *Catalog {
	if input == nil {
		return nil
	}
	out := *input
	out.Entries = append([]Entry(nil), input.Entries...)
	sort.Slice(out.Entries, func(i, j int) bool {
		if out.Entries[i].Kind != out.Entries[j].Kind {
			return out.Entries[i].Kind < out.Entries[j].Kind
		}
		return out.Entries[i].ID < out.Entries[j].ID
	})
	return &out
}

// MarshalCatalog returns canonical two-space-indented JSON with one trailing
// newline.
func MarshalCatalog(catalog *Catalog) ([]byte, error) {
	data, err := json.MarshalIndent(CanonicalCatalog(catalog), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
