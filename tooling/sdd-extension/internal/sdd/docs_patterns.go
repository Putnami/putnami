package sdd

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
)

// EvidenceOptionNamespace is the workspace option block that lists where the
// repository's documents are: options.evidence.documents.
const EvidenceOptionNamespace = "evidence"

// DocumentTypeDocs is the document type whose entries select the documents
// the link check reads.
const DocumentTypeDocs = "docs"

// documentTypes are the types an entry of options.evidence.documents may
// name (ADR 0006 of protocols/evidence). Only docs entries are read here; the
// other types belong to the checks of their documents.
var documentTypes = []string{"spec", "decision", "architecture", DocumentTypeDocs}

// documentEntryMembers are the members an entry may hold.
var documentEntryMembers = []string{"type", "format", "patterns"}

// DocsPatternSet is one docs entry of options.evidence.documents: its
// position in the list, which names it in a finding, and its patterns.
type DocsPatternSet struct {
	Field    string
	Patterns []string
}

// ConfiguredDocsPatterns returns the docs entries of options.evidence.documents,
// in declaration order. configured is false when the workspace lists no
// entry of type docs, which keeps the link check on its default documents.
// Every entry is checked, whatever its type: one that is not an object, whose
// type is not a document type, that holds a member other than type, format
// and patterns, or whose patterns hold no include is an error naming it. A
// configuration the check cannot read is never read as no configuration.
func ConfiguredDocsPatterns(workspaceOptions map[string]map[string]any) (sets []DocsPatternSet, configured bool, err error) {
	block, ok := workspaceOptions[EvidenceOptionNamespace]
	if !ok {
		return nil, false, nil
	}
	raw, ok := block["documents"]
	if !ok {
		return nil, false, nil
	}
	entries, ok := raw.([]any)
	if !ok {
		return nil, false, fmt.Errorf("options.evidence.documents must be a list of entries")
	}
	for index, rawEntry := range entries {
		field := fmt.Sprintf("options.evidence.documents[%d]", index)
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("%s must be an object with a type and patterns", field)
		}
		var unknown []string
		for name := range entry {
			if !slices.Contains(documentEntryMembers, name) {
				unknown = append(unknown, name)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			return nil, false, fmt.Errorf("%s holds %q, which is not a member of a documents entry", field, unknown[0])
		}
		kind, _ := entry["type"].(string)
		if !slices.Contains(documentTypes, kind) {
			return nil, false, fmt.Errorf("%s has type %q; a documents entry has type %s", field, kind, strings.Join(documentTypes, ", "))
		}
		if format, present := entry["format"]; present {
			if name, ok := format.(string); !ok || strings.TrimSpace(name) == "" {
				return nil, false, fmt.Errorf("%s.format must be a non-empty format name", field)
			}
		}
		patterns, err := entryPatterns(field, entry["patterns"])
		if err != nil {
			return nil, false, err
		}
		if kind == DocumentTypeDocs {
			sets = append(sets, DocsPatternSet{Field: field, Patterns: patterns})
		}
	}
	return sets, len(sets) > 0, nil
}

// entryPatterns reads one entry's patterns: a list of strings in the
// workspace file-pattern grammar with at least one include.
func entryPatterns(field string, raw any) ([]string, error) {
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s.patterns must be a list of file patterns", field)
	}
	patterns := make([]string, 0, len(list))
	for index, item := range list {
		pattern, ok := item.(string)
		if !ok || strings.TrimSpace(pattern) == "" {
			return nil, fmt.Errorf("%s.patterns[%d] must be a non-empty file pattern", field, index)
		}
		patterns = append(patterns, pattern)
	}
	if includes, _ := wsproto.SplitFilePatterns(patterns); len(includes) == 0 {
		return nil, fmt.Errorf("%s.patterns holds no include pattern, so it selects nothing", field)
	}
	return patterns, nil
}

// unionSorted merges sorted path lists, each path once.
func unionSorted(lists ...[]string) []string {
	seen := map[string]bool{}
	var merged []string
	for _, list := range lists {
		for _, path := range list {
			if !seen[path] {
				seen[path] = true
				merged = append(merged, path)
			}
		}
	}
	sort.Strings(merged)
	return merged
}
