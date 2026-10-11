package workspaceclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func censusFinding(path string, line int) Finding {
	return Finding{Code: "clientgen.unclassified-transport", Path: path, Line: line, Column: 3,
		Message: "transport callsite must be associated with a generated binding or an explicit external authority"}
}

func censusCallsites(entries ...TransportCallsite) map[string]TransportCallsite {
	index := map[string]TransportCallsite{}
	for _, entry := range entries {
		index[callsiteKey(entry.Path, entry.Line, entry.Column)] = entry
	}
	return index
}

func writeCensus(t *testing.T, root, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, PendingTransportCensusFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const censusOneHTTPGet = `{
  "version": 1,
  "reason": "predates the guard",
  "closedBy": "the callsite classification migration",
  "entries": [
    {"code": "clientgen.unclassified-transport", "path": "svc/legacy.go", "transport": "http", "symbol": "http.Get", "count": 1}
  ]
}`

// TestACensusedCallsiteReportsAndANewOneBlocks is the whole point of the
// document: the debt the workspace already had stays visible without failing,
// and anything the change under test adds fails.
func TestACensusedCallsiteReportsAndANewOneBlocks(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "censused-pending-transports", "a-censused-callsite-reports-without-failing")
	spectest.Proves(t, clientgenFeature, "censused-pending-transports", "a-callsite-the-census-does-not-account-for-fails")
	root := t.TempDir()
	writeCensus(t, root, censusOneHTTPGet)

	old := censusFinding("svc/legacy.go", 10)
	fresh := censusFinding("svc/legacy.go", 42)
	callsites := censusCallsites(
		TransportCallsite{Path: "svc/legacy.go", Line: 10, Column: 3, Transport: "http", Symbol: "http.Get"},
		TransportCallsite{Path: "svc/legacy.go", Line: 42, Column: 3, Transport: "http", Symbol: "http.Get"},
	)

	blocking, pending := classifyPendingTransports([]Finding{old, fresh}, callsites, loadPendingCensus(workspaceFiles{root: root}))
	if len(pending) != 1 || pending[0].Line != 10 {
		t.Fatalf("pending = %+v, want exactly the censused callsite", pending)
	}
	if !strings.Contains(pending[0].Message, PendingTransportCensusFile) {
		t.Fatalf("a pending finding must say where its exemption comes from: %q", pending[0].Message)
	}
	if len(blocking) != 1 || blocking[0].Line != 42 {
		t.Fatalf("blocking = %+v, want exactly the second callsite in the censused file", blocking)
	}
}

// TestTheCensusIsNotAFileOrTransportAllowlist: the count is per exact class. A
// different symbol in a listed file, and a listed symbol in another file, both
// fail.
func TestTheCensusIsNotAFileOrTransportAllowlist(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "censused-pending-transports", "the-census-counts-exact-callsite-classes-not-files-or-transports")
	root := t.TempDir()
	writeCensus(t, root, censusOneHTTPGet)

	otherSymbol := censusFinding("svc/legacy.go", 11)
	otherFile := censusFinding("svc/other.go", 12)
	callsites := censusCallsites(
		TransportCallsite{Path: "svc/legacy.go", Line: 11, Column: 3, Transport: "http", Symbol: "http.Post"},
		TransportCallsite{Path: "svc/other.go", Line: 12, Column: 3, Transport: "http", Symbol: "http.Get"},
	)
	blocking, pending := classifyPendingTransports([]Finding{otherSymbol, otherFile}, callsites, loadPendingCensus(workspaceFiles{root: root}))
	if len(pending) != 0 {
		t.Fatalf("pending = %+v, want none: neither callsite matches the censused class", pending)
	}
	if len(blocking) != 3 {
		// two new callsites plus the stale entry the census now over-records
		t.Fatalf("blocking = %+v, want both new callsites and the stale entry", blocking)
	}
}

// TestTheCensusOnlyShrinks is the ratchet: removing a bypass without removing
// its entry leaves a permanent exemption behind, so it fails.
func TestTheCensusOnlyShrinks(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "censused-pending-transports", "an-entry-that-matches-nothing-fails-so-the-census-only-shrinks")
	root := t.TempDir()
	writeCensus(t, root, censusOneHTTPGet)
	blocking, pending := classifyPendingTransports(nil, nil, loadPendingCensus(workspaceFiles{root: root}))
	if len(pending) != 0 {
		t.Fatalf("pending = %+v, want none", pending)
	}
	if len(blocking) != 1 || blocking[0].Code != "clientgen.stale-pending-transport" {
		t.Fatalf("blocking = %+v, want one stale-pending-transport", blocking)
	}
}

// TestAnAbsentCensusBlocksEverything: a workspace that never declared debt has
// none, and every handwritten callsite fails. The document is opt-in evidence,
// never a prerequisite.
func TestAnAbsentCensusBlocksEverything(t *testing.T) {
	root := t.TempDir()
	finding := censusFinding("svc/legacy.go", 10)
	callsites := censusCallsites(TransportCallsite{Path: "svc/legacy.go", Line: 10, Column: 3, Transport: "http", Symbol: "http.Get"})
	blocking, pending := classifyPendingTransports([]Finding{finding}, callsites, loadPendingCensus(workspaceFiles{root: root}))
	if len(pending) != 0 || len(blocking) != 1 {
		t.Fatalf("blocking = %+v pending = %+v, want the callsite to fail", blocking, pending)
	}
}

// TestAMalformedCensusFailsClosed: an unreadable, unknown-version, unexplained,
// unsorted or unknown-field census is refused rather than read leniently. A
// census that silently means something else than the guard thinks is worse than
// no census at all.
func TestAMalformedCensusFailsClosed(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "censused-pending-transports", "a-malformed-census-fails-instead-of-being-read-leniently")
	cases := map[string]string{
		"not json":        `{`,
		"unknown field":   `{"version":1,"reason":"r","closedBy":"c","entries":[],"allow":["**"]}`,
		"wrong version":   `{"version":2,"reason":"r","closedBy":"c","entries":[]}`,
		"no reason":       `{"version":1,"reason":"","closedBy":"c","entries":[]}`,
		"no closing work": `{"version":1,"reason":"r","closedBy":"","entries":[]}`,
		"unsorted": `{"version":1,"reason":"r","closedBy":"c","entries":[
			{"code":"clientgen.unclassified-transport","path":"b.go","transport":"http","symbol":"http.Get","count":1},
			{"code":"clientgen.unclassified-transport","path":"a.go","transport":"http","symbol":"http.Get","count":1}]}`,
		"zero count": `{"version":1,"reason":"r","closedBy":"c","entries":[
			{"code":"clientgen.unclassified-transport","path":"a.go","transport":"http","symbol":"http.Get","count":0}]}`,
		"drift is not debt": `{"version":1,"reason":"r","closedBy":"c","entries":[
			{"code":"clientgen.generated-file-drift","path":"a.go","transport":"http","symbol":"http.Get","count":1}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeCensus(t, root, body)
			blocking, _ := classifyPendingTransports(nil, nil, loadPendingCensus(workspaceFiles{root: root}))
			invalid := false
			for _, finding := range blocking {
				invalid = invalid || finding.Code == "clientgen.invalid-pending-census"
			}
			if !invalid {
				t.Fatalf("a %s census was accepted: %+v", name, blocking)
			}
		})
	}
}

// TestTheCommittedCensusIsValidAndOnlyCoversHandwrittenTransports reads the
// repository's own document. A census this repository cannot parse would
// silently exempt nothing and fail every gate; one that grew a drift verdict
// would exempt the thing the guard exists for.
func TestTheCommittedCensusIsValidAndOnlyCoversHandwrittenTransports(t *testing.T) {
	root := repositoryRootForTest(t)
	census := loadPendingCensus(workspaceFiles{root: root})
	if len(census.invalid) != 0 {
		t.Fatalf("the committed census is invalid: %+v", census.invalid)
	}
	if len(census.Entries) == 0 {
		t.Skip("the census is empty; nothing to assert until it is repopulated or deleted")
	}
	if !strings.Contains(census.ClosedBy, "#") {
		t.Fatalf("closedBy = %q; the census must name the work that deletes it", census.ClosedBy)
	}
	total := 0
	for _, entry := range census.Entries {
		if !pendingEligibleCodes[entry.Code] {
			t.Fatalf("entry %+v carries a verdict that is never pre-existing debt", entry)
		}
		total += entry.Count
	}
	t.Logf("committed census: %d entries covering %d callsites", len(census.Entries), total)
}
