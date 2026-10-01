package jobs

import "sort"

// DedupeSorted sorts items in place and removes adjacent duplicates.
func DedupeSorted(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	sort.Strings(items)
	out := items[:0]
	var prev string
	for i, item := range items {
		if i > 0 && item == prev {
			continue
		}
		out = append(out, item)
		prev = item
	}
	return out
}
