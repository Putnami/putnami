package sdd

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	protocolcli "go.putnami.dev/protocol/cli"
	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// largeCatalogWorkspace declares features features, one per project, across
// projects projects. Every declaration carries a long outcome, relations and
// requirements, so a page that kept declarations would grow with the catalog.
func largeCatalogWorkspace(t *testing.T, features, projects int) (*workspace.Workspace, Selection) {
	t.Helper()
	root := t.TempDir()
	members := make([]*workspace.Project, 0, projects)
	ids := make([]string, 0, projects)
	for index := range projects {
		path := fmt.Sprintf("p%04d", index)
		members = append(members, appProject("@scale/"+path, path))
		ids = append(ids, "/"+path)
		if index >= features {
			continue
		}
		writeFixtureFile(t, filepath.Join(root, path, featureproto.ManifestFilename), fmt.Sprintf(`{
  "protocolVersion": 1,
  "namespace": "%[1]s",
  "features": [{
    "id": "%[1]s/capability",
    "type": "feature",
    "name": "Capability %[1]s",
    "outcome": "%[2]s",
    "owner": "team-%[1]s",
    "target": "coded",
    "relations": [{"kind":"dependsOn","target":"platform/releases"},{"kind":"dependsOn","target":"platform/billing"}],
    "requirements": [
      {"id":"implementation","stage":"coded","evidenceKinds":["artifact"]},
      {"id":"contract","stage":"coded","evidenceKinds":["capability"]},
      {"id":"journey","stage":"wired","evidenceKinds":["artifact"]}
    ]
  }]
}`, path, strings.Repeat("Operators complete the documented journey end to end. ", 8)))
	}
	return fixtureWorkspace("scale", root, members...), Selection{Mode: "all", ProjectIDs: ids}
}

func encodePage(t *testing.T, page FeatureCatalogPage) []byte {
	t.Helper()
	encoded, err := json.MarshalIndent(page, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// TestFeatureCatalogPageStaysBoundedAndPagesEveryFeatureOnce is the acceptance:
// on a catalog ten times larger than a typical workspace, every page stays under
// FeatureCatalogPageMaxBytes, and following next returns every feature once, in
// id order.
func TestFeatureCatalogPageStaysBoundedAndPagesEveryFeatureOnce(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "bounded-feature-catalog",
		"catalog-pages-stay-under-their-bound-and-return-every-feature-once")
	const features, projects = 600, 1500
	ws, selection := largeCatalogWorkspace(t, features, projects)

	for _, limit := range []int{FeatureCatalogDefaultLimit, FeatureCatalogMaxLimit} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			seen := make(map[string]bool, features)
			budgetStops := 0
			previous := ""
			cursor := ""
			for pages := 0; ; pages++ {
				if pages > features {
					t.Fatal("paging did not terminate")
				}
				page, err := BuildFeatureCatalogPage(ws, "", selection, cursor, limit)
				if err != nil {
					t.Fatal(err)
				}
				if size := len(encodePage(t, page)); size > FeatureCatalogPageMaxBytes {
					t.Fatalf("page %d is %d bytes, above the %d byte bound", pages, size, FeatureCatalogPageMaxBytes)
				}
				if page.Total != features {
					t.Fatalf("total = %d, want %d", page.Total, features)
				}
				if page.Selection.Projects != nil || page.Selection.ProjectCount != projects {
					t.Fatalf("unscoped selection = %+v, want the project count alone", page.Selection)
				}
				wantLimit := limit
				if len(page.Features) == 0 || len(page.Features) > wantLimit {
					t.Fatalf("page %d holds %d entries, want 1 to %d", pages, len(page.Features), wantLimit)
				}
				for _, entry := range page.Features {
					if seen[entry.ID] {
						t.Fatalf("%s returned twice", entry.ID)
					}
					if entry.ID <= previous {
						t.Fatalf("%s follows %s: entries are not in id order", entry.ID, previous)
					}
					seen[entry.ID] = true
					previous = entry.ID
				}
				if page.Next == "" {
					break
				}
				if len(page.Features) < wantLimit {
					budgetStops++
				}
				cursor = page.Next
			}
			if len(seen) != features {
				t.Fatalf("paging returned %d features, want %d", len(seen), features)
			}
			if limit == FeatureCatalogMaxLimit && budgetStops == 0 {
				t.Fatal("no page stopped on the byte budget, so this case does not prove it")
			}
		})
	}
}

// TestFeatureCatalogEntryIsShort pins the entry shape: truncated text, the
// manifest target, and counts in place of declarations.
func TestFeatureCatalogEntryIsShort(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "bounded-feature-catalog",
		"catalog-entries-are-short-and-capped")
	ws, selection := largeCatalogWorkspace(t, 1, 1)
	page, err := BuildFeatureCatalogPage(ws, "", selection, "", FeatureCatalogDefaultLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Features) != 1 || page.Next != "" {
		t.Fatalf("page = %+v, want the one feature and no next", page)
	}
	entry := page.Features[0]
	if entry.ID != "p0000/capability" || entry.Name != "Capability p0000" || entry.Owner != "team-p0000" {
		t.Fatalf("entry = %+v", entry)
	}
	if utf8.RuneCountInString(entry.Summary) != featureCatalogSummaryRunes || !strings.HasSuffix(entry.Summary, "…") {
		t.Fatalf("summary = %q, want the outcome truncated to %d runes", entry.Summary, featureCatalogSummaryRunes)
	}
	if entry.Target != featureproto.MaturityCoded {
		t.Fatalf("target = %q, want coded", entry.Target)
	}
	if entry.Counts != (FeatureCatalogEntryCounts{Projects: 1, Declarations: 1, Requirements: 3}) {
		t.Fatalf("counts = %+v", entry.Counts)
	}
	encoded := string(encodePage(t, page))
	for _, dropped := range []string{`"declarations": [`, `"requirements": [`, `"relations"`, `"outcome"`} {
		if strings.Contains(encoded, dropped) {
			t.Errorf("the page carries %s, which sdd.feature_context serves", dropped)
		}
	}
}

// TestFeatureCatalogEntryCapsConflictingSources keeps one feature declared by
// many manifests from growing its entry with the number of projects.
func TestFeatureCatalogEntryCapsConflictingSources(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "bounded-feature-catalog",
		"catalog-entries-are-short-and-capped")
	root := t.TempDir()
	const declarers = 12
	members := make([]*workspace.Project, 0, declarers)
	for index := range declarers {
		path := fmt.Sprintf("d%02d", index)
		members = append(members, appProject("@dup/"+path, path))
		writeFixtureFile(t, filepath.Join(root, path, featureproto.ManifestFilename), fmt.Sprintf(`{
  "protocolVersion": 1,
  "namespace": "shared",
  "features": [{"id":"shared/thing","type":"feature","name":"Thing %s","outcome":"Something happens","owner":"shared","target":"modeled"}]
}`, path))
	}
	page, err := BuildFeatureCatalogPage(fixtureWorkspace("dup", root, members...), "", Selection{Mode: "all"}, "", FeatureCatalogDefaultLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Features) != 1 {
		t.Fatalf("features = %+v, want one merged identity", page.Features)
	}
	entry := page.Features[0]
	if entry.ConflictCount != declarers-1 || len(entry.ConflictingSources) != featureCatalogConflictLimit {
		t.Fatalf("conflicts = %d with %d sources, want %d with %d",
			entry.ConflictCount, len(entry.ConflictingSources), declarers-1, featureCatalogConflictLimit)
	}
	if entry.Counts.Declarations != declarers || entry.Counts.Projects != declarers {
		t.Fatalf("counts = %+v", entry.Counts)
	}
}

// TestFeatureCatalogPageKeepsScopedProjects keeps the ids of a scoped selection,
// which the caller chose and which explain the narrowing.
func TestFeatureCatalogPageKeepsScopedProjects(t *testing.T) {
	page, err := BuildFeatureCatalogPage(manyProjectWorkspace(t), "", billingSelection(), "", FeatureCatalogDefaultLimit)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Selection.Scoped || page.Selection.ProjectCount != 1 ||
		len(page.Selection.Projects) != 1 || page.Selection.Projects[0] != "/billing" {
		t.Fatalf("selection = %+v, want the one selected project", page.Selection)
	}
	if page.Total != 1 || len(page.Features) != 1 || page.Features[0].ID != "billing/invoice" {
		t.Fatalf("features = %+v", page.Features)
	}
}

// TestFeatureCatalogPageRejectsAnInvalidLimitOrCursor makes a bad argument a
// usage error instead of a silently different page.
func TestFeatureCatalogPageRejectsAnInvalidLimitOrCursor(t *testing.T) {
	ws, selection := largeCatalogWorkspace(t, 1, 1)
	for _, call := range []struct {
		name   string
		cursor string
		limit  int
	}{
		{name: "zero limit", limit: 0},
		{name: "negative limit", limit: -1},
		{name: "limit above the maximum", limit: FeatureCatalogMaxLimit + 1},
		{name: "cursor that is not base64", cursor: "%%%", limit: 1},
		{name: "cursor without the tool's prefix", cursor: "YWJj", limit: 1},
		{name: "cursor without an id", cursor: encodeFeatureCatalogCursor(""), limit: 1},
	} {
		t.Run(call.name, func(t *testing.T) {
			_, err := BuildFeatureCatalogPage(ws, "", selection, call.cursor, call.limit)
			if !errors.Is(err, protocolcli.ErrUsage) {
				t.Fatalf("err = %v, want a usage error", err)
			}
		})
	}
}

// TestFeatureCatalogPageFollowsAQueryAcrossPages pages a filtered catalog: the
// total counts the matches, and following next returns each match once.
func TestFeatureCatalogPageFollowsAQueryAcrossPages(t *testing.T) {
	ws, selection := largeCatalogWorkspace(t, 30, 30)
	// "p001" matches p0010 to p0019 only.
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
		page, err := BuildFeatureCatalogPage(ws, "p001", selection, cursor, 3)
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 10 || len(page.Features) > 3 {
			t.Fatalf("page holds %d of total %d, want at most 3 of 10", len(page.Features), page.Total)
		}
		for _, entry := range page.Features {
			if !strings.HasPrefix(entry.ID, "p001") || seen[entry.ID] {
				t.Fatalf("%s is not a new match", entry.ID)
			}
			seen[entry.ID] = true
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if len(seen) != 10 {
		t.Fatalf("paging returned %d matches, want 10", len(seen))
	}
}

// TestFeatureCatalogEntryTruncatesNameAndOwner keeps authored text that the
// manifest allows up to 512 characters from widening an entry.
func TestFeatureCatalogEntryTruncatesNameAndOwner(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "bounded-feature-catalog",
		"catalog-entries-are-short-and-capped")
	root := t.TempDir()
	long := strings.Repeat("n", 300)
	writeFixtureFile(t, filepath.Join(root, "app", featureproto.ManifestFilename), `{
  "protocolVersion": 1,
  "namespace": "app",
  "features": [{"id":"app/thing","type":"feature","name":"`+long+`","outcome":"Line one   line two","owner":"`+long+`","target":"modeled"}]
}`)
	page, err := BuildFeatureCatalogPage(fixtureWorkspace("long", root, appProject("@long/app", "app")),
		"", Selection{Mode: "all"}, "", FeatureCatalogDefaultLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Features) != 1 {
		t.Fatalf("page = %+v, unreadable %+v", page.Features, page.Unreadable)
	}
	entry := page.Features[0]
	for field, value := range map[string]string{"name": entry.Name, "owner": entry.Owner} {
		if utf8.RuneCountInString(value) != featureCatalogNameRunes || !strings.HasSuffix(value, "…") {
			t.Errorf("%s has %d runes, want %d ending in an ellipsis", field, utf8.RuneCountInString(value), featureCatalogNameRunes)
		}
	}
	if entry.Summary != "Line one line two" {
		t.Errorf("summary = %q, want the outcome on one line", entry.Summary)
	}
}

// TestFeatureCatalogPageCapsUnreadableAuthorities keeps many broken design
// graphs from growing the page, while unreadableCount still counts them all.
func TestFeatureCatalogPageCapsUnreadableAuthorities(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "bounded-feature-catalog",
		"catalog-entries-are-short-and-capped")
	root := t.TempDir()
	const broken = featureCatalogUnreadableLimit + 5
	members := make([]*workspace.Project, 0, broken)
	for index := range broken {
		path := fmt.Sprintf("b%02d", index)
		members = append(members, appProject("@broken/"+path, path))
		writeFixtureFile(t,
			filepath.Join(root, path, ".gen", filepath.FromSlash(featureproto.DesignGraphArtifact)),
			`{"compatibility": not-json`)
	}
	// A feature id outside its namespace is quoted whole in the diagnostic, so
	// this manifest's reason is longer than the cap.
	members = append(members, appProject("@broken/app", "app"))
	writeFixtureFile(t, filepath.Join(root, "app", featureproto.ManifestFilename), `{
  "protocolVersion": 1,
  "namespace": "app",
  "features": [{"id":"other/`+strings.Repeat("x", 400)+`","type":"feature","name":"Thing","outcome":"Something happens","owner":"app","target":"modeled"}]
}`)
	page, err := BuildFeatureCatalogPage(fixtureWorkspace("broken", root, members...),
		"", Selection{Mode: "all"}, "", FeatureCatalogDefaultLimit)
	if err != nil {
		t.Fatal(err)
	}
	if page.UnreadableCount != broken+1 || len(page.Unreadable) != featureCatalogUnreadableLimit {
		t.Fatalf("unreadable = %d listed of %d, want %d of %d",
			len(page.Unreadable), page.UnreadableCount, featureCatalogUnreadableLimit, broken+1)
	}
	truncated := false
	for _, issue := range page.Unreadable {
		runes := utf8.RuneCountInString(issue.Reason)
		if runes > featureCatalogReasonRunes {
			t.Errorf("%s reason has %d runes, above %d", issue.Path, runes, featureCatalogReasonRunes)
		}
		if issue.Path == "app/"+featureproto.ManifestFilename {
			truncated = runes == featureCatalogReasonRunes && strings.HasSuffix(issue.Reason, "…")
		}
	}
	if !truncated {
		t.Errorf("the long manifest reason was not truncated to %d runes: %+v", featureCatalogReasonRunes, page.Unreadable)
	}
}

func TestTruncateRunesCutsOnARuneBoundary(t *testing.T) {
	if got := truncateRunes("abc", 3); got != "abc" {
		t.Errorf("truncateRunes kept %q, want the text whole", got)
	}
	if got := truncateRunes("ééééé", 3); got != "éé…" {
		t.Errorf("truncateRunes = %q, want two runes and an ellipsis", got)
	}
}
