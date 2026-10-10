package sdd

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"

	protocolcli "go.putnami.dev/protocol/cli"
	featureproto "go.putnami.dev/protocol/features"
	featureengine "go.putnami.dev/tooling/sdd/extension/internal/features"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// The bounds of one feature catalog page. A page stops at the limit or before
// its indented JSON would pass FeatureCatalogPageMaxBytes, and holds at least one
// entry. Every free-text field is truncated and every list an entry or the
// header carries is capped, so only a header that names more selected projects
// than the budget holds, or one entry larger than the budget, passes it.
const (
	FeatureCatalogDefaultLimit = 50
	FeatureCatalogMaxLimit     = 200
	FeatureCatalogPageMaxBytes = 64 << 10

	featureCatalogPageSlack         = 16
	featureCatalogNameRunes         = 128
	featureCatalogSummaryRunes      = 160
	featureCatalogConflictLimit     = 8
	featureCatalogUnreadableLimit   = 20
	featureCatalogReasonRunes       = 256
	featureCatalogCursorPrefix      = "after:"
	featureCatalogEntryIndentPrefix = "    "
)

// FeatureCatalogPage is the MCP answer of sdd.list_features: one page of short
// entries in id order. An agent reads a feature's declarations, requirements
// and implementations with sdd.feature_context.
type FeatureCatalogPage struct {
	Compatibility string                    `json:"compatibility"`
	Revision      featureengine.Revision    `json:"revision"`
	Selection     FeatureCatalogSelection   `json:"selection"`
	Query         string                    `json:"query,omitempty"`
	Graphs        int                       `json:"graphs"`
	Manifests     int                       `json:"manifests"`
	Total         int                       `json:"total"`
	Features      []FeatureCatalogEntry     `json:"features"`
	Next          string                    `json:"next,omitempty"`
	Unreadable    []FeatureDesignGraphIssue `json:"unreadable,omitempty"`
	// UnreadableCount is the number of unreadable authorities, including those
	// past the capped Unreadable list.
	UnreadableCount int `json:"unreadableCount,omitempty"`
}

// FeatureCatalogSelection is the selection a page answers. An unscoped page
// carries the project count only; a scoped page also names the projects the
// caller selected.
type FeatureCatalogSelection struct {
	Mode            string   `json:"mode"`
	Scoped          bool     `json:"scoped"`
	Baseline        string   `json:"baseline,omitempty"`
	BaselineSource  string   `json:"baselineSource,omitempty"`
	Projects        []string `json:"projects,omitempty"`
	ProjectCount    int      `json:"projectCount"`
	EmptyImpact     bool     `json:"emptyImpact,omitempty"`
	Features        int      `json:"features"`
	ExternalRecords []string `json:"externalRecords,omitempty"`
}

// FeatureCatalogEntry is one feature, short. Name and owner are truncated to
// featureCatalogNameRunes, Summary is the outcome on one line truncated to
// featureCatalogSummaryRunes, and ConflictingSources holds at most
// featureCatalogConflictLimit paths of ConflictCount.
type FeatureCatalogEntry struct {
	ID                 string                     `json:"id"`
	Name               string                     `json:"name"`
	Summary            string                     `json:"summary"`
	Owner              string                     `json:"owner"`
	Target             featureproto.MaturityStage `json:"target,omitempty"`
	Counts             FeatureCatalogEntryCounts  `json:"counts"`
	ConflictingSources []string                   `json:"conflictingSources,omitempty"`
	ConflictCount      int                        `json:"conflictCount,omitempty"`
}

// FeatureCatalogEntryCounts sizes what sdd.feature_context returns for the
// feature: the projects that declare it, its declarations, and the distinct
// requirement ids its manifests declare.
type FeatureCatalogEntryCounts struct {
	Projects     int `json:"projects"`
	Declarations int `json:"declarations"`
	Requirements int `json:"requirements"`
}

// BuildFeatureCatalogPage answers one page of the catalog BuildFeatureCatalogResult
// discovers. cursor is empty for the first page, otherwise the Next of the
// previous page, and limit is between 1 and FeatureCatalogMaxLimit. A cursor
// names the last id it returned, so a page never repeats or skips a feature
// that exists on the revision both pages read.
func BuildFeatureCatalogPage(ws *workspace.Workspace, query string, selection Selection, cursor string, limit int) (FeatureCatalogPage, error) {
	if limit < 1 || limit > FeatureCatalogMaxLimit {
		return FeatureCatalogPage{}, protocolcli.Usagef("limit must be between 1 and %d", FeatureCatalogMaxLimit)
	}
	after, err := decodeFeatureCatalogCursor(cursor)
	if err != nil {
		return FeatureCatalogPage{}, err
	}
	catalog, err := BuildFeatureCatalogResult(ws, query, selection)
	if err != nil {
		return FeatureCatalogPage{}, err
	}
	page := FeatureCatalogPage{
		Compatibility: catalog.Compatibility,
		Revision:      catalog.Revision,
		Selection:     newFeatureCatalogSelection(catalog.Selection),
		Query:         catalog.Query,
		Graphs:        catalog.Graphs,
		Manifests:     catalog.Manifests,
		Total:         len(catalog.Features),
		Features:      []FeatureCatalogEntry{},
	}
	page.UnreadableCount = len(catalog.Unreadable)
	for index, issue := range catalog.Unreadable {
		if index == featureCatalogUnreadableLimit {
			break
		}
		issue.Reason = truncateRunes(issue.Reason, featureCatalogReasonRunes)
		page.Unreadable = append(page.Unreadable, issue)
	}

	header, err := json.MarshalIndent(page, "", "  ")
	if err != nil {
		return FeatureCatalogPage{}, err
	}
	used := len(header) + featureCatalogPageSlack
	features := catalog.Features
	start := sort.Search(len(features), func(i int) bool { return features[i].ID > after })
	for index := start; index < len(features); index++ {
		if len(page.Features) == limit {
			page.Next = encodeFeatureCatalogCursor(page.Features[len(page.Features)-1].ID)
			break
		}
		entry := newFeatureCatalogEntry(features[index])
		size := featureCatalogEntrySize(entry)
		next := 0
		if index+1 < len(features) {
			next = featureCatalogNextSize(entry.ID)
		}
		if len(page.Features) > 0 && used+size+next > FeatureCatalogPageMaxBytes {
			page.Next = encodeFeatureCatalogCursor(page.Features[len(page.Features)-1].ID)
			break
		}
		page.Features = append(page.Features, entry)
		used += size
	}
	return page, nil
}

func newFeatureCatalogSelection(report SelectionReport) FeatureCatalogSelection {
	selection := FeatureCatalogSelection{
		Mode:            report.Mode,
		Scoped:          report.Scoped,
		Baseline:        report.Baseline,
		BaselineSource:  report.BaselineSource,
		ProjectCount:    len(report.Projects),
		EmptyImpact:     report.EmptyImpact,
		Features:        report.Features,
		ExternalRecords: report.ExternalRecords,
	}
	if report.Scoped {
		selection.Projects = report.Projects
	}
	return selection
}

func newFeatureCatalogEntry(feature FeatureDesignSummary) FeatureCatalogEntry {
	entry := FeatureCatalogEntry{
		ID:      feature.ID,
		Name:    truncateRunes(feature.Name, featureCatalogNameRunes),
		Summary: truncateRunes(strings.Join(strings.Fields(feature.Outcome), " "), featureCatalogSummaryRunes),
		Owner:   truncateRunes(feature.Owner, featureCatalogNameRunes),
		Counts: FeatureCatalogEntryCounts{
			Projects:     len(feature.Projects),
			Declarations: len(feature.Declarations),
		},
		ConflictCount: len(feature.ConflictingSources),
	}
	requirements := make(map[string]bool)
	for _, declaration := range feature.Declarations {
		if entry.Target == "" {
			entry.Target = declaration.Target
		}
		for _, requirement := range declaration.Requirements {
			requirements[requirement.ID] = true
		}
	}
	entry.Counts.Requirements = len(requirements)
	if len(feature.ConflictingSources) > featureCatalogConflictLimit {
		entry.ConflictingSources = feature.ConflictingSources[:featureCatalogConflictLimit]
	} else {
		entry.ConflictingSources = feature.ConflictingSources
	}
	return entry
}

// featureCatalogEntrySize is the bytes an entry adds to the indented answer:
// its encoding at the depth of the features array, its line break and its
// separator.
func featureCatalogEntrySize(entry FeatureCatalogEntry) int {
	encoded, err := json.MarshalIndent(entry, featureCatalogEntryIndentPrefix, "  ")
	if err != nil {
		return FeatureCatalogPageMaxBytes
	}
	return len(encoded) + len(featureCatalogEntryIndentPrefix) + 2
}

// featureCatalogNextSize is the bytes the next member adds to the indented
// answer when the page ends after the entry with lastID.
func featureCatalogNextSize(lastID string) int {
	return len(",\n  \"next\": \"\"") + len(encodeFeatureCatalogCursor(lastID))
}

func encodeFeatureCatalogCursor(lastID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(featureCatalogCursorPrefix + lastID))
}

func decodeFeatureCatalogCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	after, found := strings.CutPrefix(string(decoded), featureCatalogCursorPrefix)
	if err != nil || !found || after == "" {
		return "", protocolcli.Usagef("cursor is not a next value this tool returned")
	}
	return after, nil
}

// truncateRunes keeps at most limit runes of text, the last one an ellipsis
// when it cuts.
func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit-1]) + "…"
}
