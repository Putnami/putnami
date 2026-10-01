package doccov

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// maxUndocumentedRatio is the coverage bar: a protocol package fails when more
// than this fraction of its exported wire-struct fields are undocumented. It
// mirrors the audit rubric (>10% undocumented is a finding). A package on the
// allowlist below is exempt until its backfill lands.
const maxUndocumentedRatio = 0.10

// pendingBackfill is the allowlist of protocol packages that are TOLERATED
// above the coverage bar because their wire-field documentation is not written
// yet. Every package NOT listed here must stay AT OR BELOW the bar, so adding an
// undocumented wire field to a documented package fails this guard.
//
// The list only ever shrinks. It is an admission of debt, not a permission: the
// ratchet in TestWireFieldDocCoverage fails an allowlisted package that has
// already reached the bar, so a backfill is not finished until its entry is
// deleted here and the package becomes guarded. Adding a NEW entry is a
// deliberate, reviewable act — it moves a package from guarded to tolerated,
// which is the only direction that loses coverage.
//
// A package that is absent is guarded, whether it was documented from the start
// (database, gomod, oci, registry, template, workspace) or backfilled later
// (see documentedPackages).
var pendingBackfill = map[string]bool{
	// Moderate — an external spec or partial docs soften the gap.
	"platform":  true, // Backfill pending: platform HTTP surface.
	"telemetry": true, // Backfill pending: OTLP/JSON envelopes.
	"migration": true, // Backfill pending: migration contract.
	// Low — schema descriptions cover automation, Go source lags.
	"extension":   true, // Backfill pending: extension manifest model.
	"infra":       true, // Backfill pending: infra requirements.
	"storage":     true, // Backfill pending: storage manifest/binding.
	"sitecontent": true, // Backfill pending: site-content bundle.
}

// documentedPackages are packages whose wire-field docs were written to clear
// the bar after the guard already existed. They are the ones most at risk of a
// silent relapse: their debt is recent, so "just allowlist it again" is a
// plausible reflex the next time a field lands undocumented.
//
// TestDocumentedPackagesAreGuarded therefore pins two things at once — that none
// of them is on pendingBackfill, and that each genuinely passes the bar — so a
// relapse fails as a named regression here instead of being absorbed silently by
// the allowlist.
var documentedPackages = []string{"cache", "config", "diagnostic", "events", "job", "runtime"}

// wirelessContracts are guarded (non-allowlisted) protocol packages that
// legitimately expose no JSON wire structs — their contract is consts plus
// validation functions, so a zero field count is expected, not a scanner
// regression. Every OTHER guarded package must scan at least one wire field;
// see the Total()==0 guard in TestWireFieldDocCoverage.
var wirelessContracts = map[string]bool{}

// TestWireFieldDocCoverage is the deterministic doc-coverage guard. It scans
// every sibling protocols/* package's Go source (as text) plus its schemas and
// asserts that non-allowlisted packages keep at most maxUndocumentedRatio of
// their exported wire-struct fields undocumented. Newly-added undocumented wire
// fields in a documented package therefore fail here.
func TestWireFieldDocCoverage(t *testing.T) {
	protocolsDir := protocolsRoot(t)
	pkgs := protocolPackages(t, protocolsDir)
	if len(pkgs) == 0 {
		t.Fatalf("no protocol packages found under %s", protocolsDir)
	}

	for _, pkg := range pkgs {
		t.Run(pkg, func(t *testing.T) {
			report, err := ScanPackage(filepath.Join(protocolsDir, pkg))
			if err != nil {
				t.Fatalf("scan %s: %v", pkg, err)
			}
			ratio := report.UndocumentedRatio()
			allowed := pendingBackfill[pkg]

			// A guarded (non-allowlisted) package must actually expose wire fields,
			// unless it is a known fieldless contract. If the scanner regresses to
			// seeing zero structs, ratio == 0 sails under the bar and the coverage
			// guard silently passes — so treat an empty scan as a failure for every
			// guarded package, not only the acute four.
			if !allowed && !wirelessContracts[pkg] && report.Total() == 0 {
				t.Fatalf("%s: scanned 0 wire fields; the scanner is not seeing this package's structs, so the doc-coverage guard would silently pass (if this package is genuinely fieldless, add it to wirelessContracts)", pkg)
			}

			if !allowed && ratio > maxUndocumentedRatio {
				undoc := report.Undocumented()
				t.Errorf("%s: %d/%d wire fields undocumented (%.1f%%), over the %.0f%% bar; document them or add the package to pendingBackfill:\n%s",
					pkg, len(undoc), report.Total(), ratio*100, maxUndocumentedRatio*100, formatUndocumented(undoc))
			}

			// Ratchet: if an allowlisted package has already reached the bar, it
			// should be removed from the allowlist so it cannot silently regress.
			if allowed && report.Total() > 0 && ratio <= maxUndocumentedRatio {
				t.Errorf("%s is on pendingBackfill but only %.1f%% of its %d wire fields are undocumented (at/below the %.0f%% bar); remove it from the allowlist",
					pkg, ratio*100, report.Total(), maxUndocumentedRatio*100)
			}
		})
	}
}

// TestDocumentedPackagesAreGuarded pins the outcome of every completed
// backfill: the package is documented (at or below the bar) and is NOT tolerated
// by the allowlist, so a later regression in its wire-field docs fails
// TestWireFieldDocCoverage instead of being absorbed by re-allowlisting it.
func TestDocumentedPackagesAreGuarded(t *testing.T) {
	protocolsDir := protocolsRoot(t)
	for _, pkg := range documentedPackages {
		t.Run(pkg, func(t *testing.T) {
			if pendingBackfill[pkg] {
				t.Fatalf("%s is documented and must not be on the pendingBackfill allowlist: re-allowlisting a documented package hides a doc regression instead of failing it", pkg)
			}
			report, err := ScanPackage(filepath.Join(protocolsDir, pkg))
			if err != nil {
				t.Fatalf("scan %s: %v", pkg, err)
			}
			if report.Total() == 0 {
				t.Fatalf("%s: scanned 0 wire fields; the scanner is not seeing this package's structs", pkg)
			}
			if ratio := report.UndocumentedRatio(); ratio > maxUndocumentedRatio {
				t.Errorf("%s: backfill incomplete — %.1f%% of %d wire fields undocumented (over the %.0f%% bar):\n%s",
					pkg, ratio*100, report.Total(), maxUndocumentedRatio*100, formatUndocumented(report.Undocumented()))
			}
		})
	}
}

// formatUndocumented renders undocumented fields as stable, greppable lines.
func formatUndocumented(fields []FieldReport) string {
	lines := make([]string, 0, len(fields))
	for _, f := range fields {
		lines = append(lines, "  "+f.Struct+"."+f.Field+" (json:\""+f.JSONName+"\")")
	}
	return strings.Join(lines, "\n")
}

// protocolsRoot returns the absolute path to the protocols/ directory that
// contains this guard package (this test lives at protocols/doccov).
func protocolsRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// go test runs with the package dir as the working directory.
	return filepath.Dir(wd)
}

// protocolPackages lists the sibling protocol package directory names under
// protocolsDir, in sorted order. A directory is a protocol package when it holds
// a go.mod. The guard package itself is excluded (it defines no wire contract).
func protocolPackages(t *testing.T, protocolsDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(protocolsDir)
	if err != nil {
		t.Fatalf("read protocols dir %s: %v", protocolsDir, err)
	}
	var pkgs []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "doccov" {
			continue
		}
		if _, err := os.Stat(filepath.Join(protocolsDir, e.Name(), "go.mod")); err != nil {
			continue
		}
		pkgs = append(pkgs, e.Name())
	}
	sort.Strings(pkgs)
	return pkgs
}
