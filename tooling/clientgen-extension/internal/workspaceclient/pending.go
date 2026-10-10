package workspaceclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// PendingTransportCensusFile is the workspace-root census of handwritten
// transport callsites that predate this guard.
//
// WHY IT EXISTS, AND WHY IT GOES AWAY. Wiring the check into `validate` turns
// every handwritten first-party transport in the repository into a failing
// gate on the day it lands — 114 of them when this shipped, none of which the
// guard itself introduced. Refusing to ship the guard until they are all gone
// would leave the repository unguarded for exactly as long as the migration
// takes, which is when a NEW bypass is cheapest to add.
//
// So the guard blocks on drift, on an invalid manifest and on a NEW
// handwritten callsite, and reports the census entries without failing. This
// is not an allowlist and the distinction is load-bearing:
//
//   - it names a COUNT per exact (code, file, transport, symbol), never a
//     folder, a file or a transport as a class, so a second `http.Get` in a
//     censused file is a new callsite and fails;
//   - it is exhaustive — an entry it does not list fails — rather than a
//     pattern that keeps matching things nobody enumerated;
//   - it only shrinks: an entry that no longer matches a real callsite is a
//     failure, so removing a bypass forces the census down and it can never
//     drift upward;
//   - it declares what closes it, and closing it deletes this file.
//
// The migration that classifies every remaining entry in the owning project's
// clientgen.framework.json or clientgen.external.json brings it to zero.
const PendingTransportCensusFile = "clientgen.pending.json"

// pendingCensusVersion is the census document version. A document written for
// a different version is refused rather than read leniently: a census that
// silently means something else than the guard thinks is worse than none.
const pendingCensusVersion = 1

// pendingCensus is the parsed census document.
type pendingCensus struct {
	Version  int                `json:"version"`
	Reason   string             `json:"reason"`
	ClosedBy string             `json:"closedBy"`
	Entries  []PendingTransport `json:"entries"`

	counts   map[string]int
	observed map[string]int
	source   string
	invalid  []Finding
}

// PendingTransport is one censused callsite class and how many of it the
// workspace had when the guard landed.
type PendingTransport struct {
	Code      string `json:"code"`
	Path      string `json:"path"`
	Transport string `json:"transport"`
	Symbol    string `json:"symbol"`
	Count     int    `json:"count"`
}

// pendingEligibleCodes are the handwritten-transport verdicts a census entry
// may carry. Drift, forged bytes and invalid manifests are deliberately absent:
// those are never pre-existing debt, they are a broken workspace.
var pendingEligibleCodes = map[string]bool{
	"clientgen.unclassified-transport":             true,
	"clientgen.unclassified-handwritten-transport": true,
	"clientgen.handwritten-first-party-client":     true,
}

func pendingKey(code, path, transport, symbol string) string {
	return code + "\x00" + path + "\x00" + transport + "\x00" + symbol
}

func (p PendingTransport) key() string { return pendingKey(p.Code, p.Path, p.Transport, p.Symbol) }

// loadPendingCensus reads and validates the census. An absent document is a
// valid empty census: a workspace that never had a bypass must not have to
// declare one, and every handwritten callsite then fails.
func loadPendingCensus(files workspaceFiles) *pendingCensus {
	census := &pendingCensus{counts: map[string]int{}, observed: map[string]int{}, source: PendingTransportCensusFile}
	data, err := files.Read(PendingTransportCensusFile)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			census.invalid = append(census.invalid, Finding{Code: "clientgen.invalid-pending-census", Path: census.source,
				Message: fmt.Sprintf("pending transport census could not be read: %v", err)})
		}
		return census
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(census); err != nil {
		census.invalid = append(census.invalid, Finding{Code: "clientgen.invalid-pending-census", Path: census.source,
			Message: fmt.Sprintf("pending transport census is not a valid document: %v", err)})
		return census
	}
	census.invalid = append(census.invalid, validatePendingCensus(census)...)
	for _, entry := range census.Entries {
		census.counts[entry.key()] += entry.Count
	}
	return census
}

func validatePendingCensus(census *pendingCensus) []Finding {
	var findings []Finding
	field := func(name, message string) Finding {
		return Finding{Code: "clientgen.invalid-pending-census", Path: census.source,
			Message: fmt.Sprintf("%s: %s", name, message)}
	}
	if census.Version != pendingCensusVersion {
		findings = append(findings, field("version", fmt.Sprintf("must be %d", pendingCensusVersion)))
	}
	if strings.TrimSpace(census.Reason) == "" {
		findings = append(findings, field("reason", "must say why these callsites are not yet migrated"))
	}
	if strings.TrimSpace(census.ClosedBy) == "" {
		findings = append(findings, field("closedBy", "must name the work that deletes this census"))
	}
	previous := ""
	for index, entry := range census.Entries {
		name := fmt.Sprintf("entries[%d]", index)
		if !pendingEligibleCodes[entry.Code] {
			findings = append(findings, field(name+".code", fmt.Sprintf("%q is not a handwritten-transport verdict", entry.Code)))
		}
		if strings.TrimSpace(entry.Path) == "" || strings.TrimSpace(entry.Transport) == "" || strings.TrimSpace(entry.Symbol) == "" {
			findings = append(findings, field(name, "path, transport and symbol are required"))
		}
		if entry.Count < 1 {
			findings = append(findings, field(name+".count", "must be at least 1"))
		}
		key := entry.key()
		if previous != "" && key <= previous {
			findings = append(findings, field(name, "entries must be unique and sorted by code, path, transport and symbol"))
		}
		previous = key
	}
	return findings
}

// classifyPendingTransports splits handwritten-transport findings into the ones
// the census already accounts for and the ones that block. A censused class
// covers its recorded COUNT of findings in canonical order; the surplus is new
// and fails.
//
// A recorded entry that matches nothing is itself a failure. That is the
// ratchet: the census can only shrink, and a migration that removes a bypass
// without removing its entry is caught rather than leaving a permanent
// exemption behind.
func classifyPendingTransports(
	findings []Finding,
	callsites map[string]TransportCallsite,
	census *pendingCensus,
) (blocking, pending []Finding) {
	blocking = append(blocking, census.invalid...)
	remaining := map[string]int{}
	for key, count := range census.counts {
		remaining[key] = count
	}
	for _, finding := range findings {
		callsite, ok := callsites[callsiteKey(finding.Path, finding.Line, finding.Column)]
		if !ok || !pendingEligibleCodes[finding.Code] {
			blocking = append(blocking, finding)
			continue
		}
		key := pendingKey(finding.Code, callsite.Path, callsite.Transport, callsite.Symbol)
		if remaining[key] <= 0 {
			blocking = append(blocking, finding)
			continue
		}
		remaining[key]--
		census.observed[key]++
		pending = append(pending, Finding{Code: finding.Code, Path: finding.Path, Line: finding.Line,
			Column: finding.Column, ServiceID: finding.ServiceID,
			Message: finding.Message + " (censused in " + census.source + " until " + census.ClosedBy + ")"})
	}
	for key, left := range remaining {
		if left <= 0 {
			continue
		}
		parts := strings.Split(key, "\x00")
		blocking = append(blocking, Finding{Code: "clientgen.stale-pending-transport", Path: parts[1],
			Message: fmt.Sprintf("%s records %d more %s callsite(s) on %s than the workspace has; "+
				"remove the entry rather than leaving an exemption behind",
				census.source, left, parts[2], parts[3])})
	}
	sort.Slice(pending, func(i, j int) bool { return findingSortKey(pending[i]) < findingSortKey(pending[j]) })
	return blocking, pending
}

func callsiteKey(path string, line, column int) string {
	return fmt.Sprintf("%s\x00%d\x00%d", path, line, column)
}

// callsiteIndex maps the coordinates a finding reports back to the callsite the
// scanner detected, so the census can be keyed by transport and symbol rather
// than by a line number that moves with every unrelated edit above it.
func callsiteIndex(adaptations []Adaptation) map[string]TransportCallsite {
	index := make(map[string]TransportCallsite, len(adaptations))
	for _, adaptation := range adaptations {
		if adaptation.Callsite == nil {
			continue
		}
		index[callsiteKey(adaptation.Callsite.Path, adaptation.Callsite.Line, adaptation.Callsite.Column)] = *adaptation.Callsite
	}
	return index
}
