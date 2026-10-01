package architecture

import (
	"fmt"
	"net/url"
	"sort"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
)

// RatchetOptions supplies the immutable comparison baseline and an explicit
// instant whose UTC calendar date evaluates waiver expiry. An expiring waiver
// fails closed when Today is zero. PreviousBaselineKnown distinguishes a
// missing baseline at initial adoption from an intentionally empty prior one.
type RatchetOptions struct {
	PreviousBaseline      *Baseline
	PreviousBaselineKnown bool
	Today                 time.Time
}

// ApplyRatchet classifies current violations as new, known debt, or waived;
// adds stale-entry and frozen-baseline-growth failures; and returns sorted
// findings plus a deterministic summary.
func ApplyRatchet(current []Finding, baseline *Baseline, waivers *WaiverFile, options RatchetOptions) ([]Finding, RatchetSummary) {
	findings := cloneFindings(current)
	baselineRecords := debtMap(nil)
	if baseline != nil {
		baselineRecords = debtMap(baseline.Findings)
	}
	waiverRecords := debtMap(nil)
	if waivers != nil {
		waiverRecords = debtMap(waivers.Waivers)
	}
	currentIDs := make(map[string]bool, len(findings))
	for index := range findings {
		finding := &findings[index]
		currentIDs[finding.ID] = true
		if _, exists := baselineRecords[finding.ID]; exists {
			finding.Severity = diag.Warning
			finding.Disposition = DispositionKnownDebt
			continue
		}
		if waiver, exists := waiverRecords[finding.ID]; exists {
			expired, evaluable := waiverExpiry(waiver, options.Today)
			if !evaluable {
				finding.Severity = diag.Error
				finding.Disposition = DispositionNew
				finding.Message = fmt.Sprintf("%s; temporary waiver expiring on %s was not applied because the UTC evaluation date is unavailable", finding.Message, waiver.Expires)
				continue
			}
			if expired {
				finding.Severity = diag.Error
				finding.Disposition = DispositionExpiredWaiver
				finding.Code = ErrorCodeExpiredWaiver
				finding.Message = fmt.Sprintf("temporary waiver for %s expired on %s", finding.ID, waiver.Expires)
				continue
			}
			finding.Severity = diag.Warning
			finding.Disposition = DispositionWaived
			continue
		}
		finding.Severity = diag.Error
		finding.Disposition = DispositionNew
	}

	for _, record := range sortedDebtRecords(baselineRecords) {
		if currentIDs[record.Finding] {
			continue
		}
		findings = append(findings, Finding{
			ID:          derivedFindingID(ErrorCodeStaleBaseline, record.Finding),
			Code:        ErrorCodeStaleBaseline,
			Severity:    diag.Error,
			Disposition: DispositionStaleBaseline,
			Message:     fmt.Sprintf("baseline entry %s is no longer observed and must be removed", record.Finding),
		})
	}
	for _, record := range sortedDebtRecords(waiverRecords) {
		if currentIDs[record.Finding] {
			continue
		}
		findings = append(findings, Finding{
			ID:          derivedFindingID(ErrorCodeStaleWaiver, record.Finding),
			Code:        ErrorCodeStaleWaiver,
			Severity:    diag.Error,
			Disposition: DispositionStaleWaiver,
			Message:     fmt.Sprintf("waiver entry %s has no current violation and must be removed", record.Finding),
		})
	}

	if options.PreviousBaselineKnown {
		previous := debtMap(nil)
		if options.PreviousBaseline != nil {
			previous = debtMap(options.PreviousBaseline.Findings)
		}
		for _, record := range sortedDebtRecords(baselineRecords) {
			if _, existed := previous[record.Finding]; existed {
				continue
			}
			findings = append(findings, Finding{
				ID:          derivedFindingID(ErrorCodeBaselineGrowth, record.Finding),
				Code:        ErrorCodeBaselineGrowth,
				Severity:    diag.Error,
				Disposition: DispositionBaselineGrowth,
				Message:     fmt.Sprintf("frozen architecture baseline grew with %s; use a temporary waiver for post-adoption debt", record.Finding),
			})
		}
	}

	sort.Slice(findings, func(i, j int) bool { return findings[i].ID < findings[j].ID })
	return findings, summarizeRatchet(findings)
}

// Observations are the derived repository facts a detector supplies for one
// evaluation. They are EVIDENCE in the ADR 0001 sense: tooling observes them,
// this package compares them with the declarations, and neither can authorize
// anything the declarations do not.
type Observations struct {
	// Edges are the exact cross-domain project dependencies of the resolved
	// graph.
	Edges []ObservedEdge
	// Evidence are the framework implementations of declared imports.
	Evidence []EvidenceRecord
}

// BuildSnapshot performs declared/observed comparison and applies the ratchet.
func BuildSnapshot(graph Graph, observed Observations, baseline *Baseline, waivers *WaiverFile, options RatchetOptions) *Snapshot {
	edges := canonicalObservedEdges(observed.Edges)
	evidence := canonicalEvidence(observed.Evidence)
	current := append(CompareObserved(graph, edges), CompareEvidence(graph, evidence)...)
	findings, summary := ApplyRatchet(current, baseline, waivers, options)
	return CanonicalSnapshot(&Snapshot{
		Schema:          "https://putnami.dev/schemas/putnami-architecture-snapshot.json",
		ProtocolVersion: ProtocolVersion,
		Coverage:        EvidenceCoverage(evidence),
		Graph:           graph,
		Observed:        edges,
		Evidence:        evidence,
		Findings:        findings,
		Ratchet:         summary,
	})
}

// HasBlockingFindings reports whether any ratcheted finding remains an error.
func HasBlockingFindings(findings []Finding) bool {
	for _, finding := range findings {
		if finding.Severity == diag.Error {
			return true
		}
	}
	return false
}

func cloneFindings(input []Finding) []Finding {
	// orEmpty, not append-to-nil: `findings` is required in the published
	// snapshot, so a clean evaluation reports an empty array rather than null.
	out := orEmpty(input)
	for index := range out {
		if input[index].Edge != nil {
			copy := *input[index].Edge
			out[index].Edge = &copy
		}
	}
	return out
}

func debtMap(records []DebtRecord) map[string]DebtRecord {
	result := make(map[string]DebtRecord, len(records))
	for _, record := range records {
		if _, exists := result[record.Finding]; !exists {
			result[record.Finding] = record
		}
	}
	return result
}

func sortedDebtRecords(records map[string]DebtRecord) []DebtRecord {
	result := make([]DebtRecord, 0, len(records))
	for _, record := range records {
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Finding < result[j].Finding })
	return result
}

func waiverExpiry(record DebtRecord, today time.Time) (expired bool, evaluable bool) {
	if record.Expires == "" {
		return false, true
	}
	if today.IsZero() {
		return false, false
	}
	expires, err := time.Parse("2006-01-02", record.Expires)
	if err != nil {
		return false, false
	}
	utcToday, err := time.Parse("2006-01-02", today.UTC().Format("2006-01-02"))
	if err != nil {
		return false, false
	}
	return expires.Before(utcToday), true
}

func derivedFindingID(code, source string) string {
	return code + ":" + url.PathEscape(source)
}

func summarizeRatchet(findings []Finding) RatchetSummary {
	var summary RatchetSummary
	for _, finding := range findings {
		switch finding.Disposition {
		case DispositionNew:
			summary.New++
		case DispositionKnownDebt:
			summary.KnownDebt++
		case DispositionWaived:
			summary.Waived++
		case DispositionStaleBaseline:
			summary.StaleBaseline++
		case DispositionStaleWaiver:
			summary.StaleWaiver++
		case DispositionBaselineGrowth:
			summary.BaselineGrowth++
		case DispositionExpiredWaiver:
			summary.ExpiredWaiver++
		}
	}
	return summary
}
