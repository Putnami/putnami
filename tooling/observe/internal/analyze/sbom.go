package analyze

import (
	"path/filepath"
	"slices"
	"strings"

	readers "go.putnami.dev/evidence/readers"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/tooling/observe/internal/lockdiff"
	"go.putnami.dev/tooling/observe/internal/reports"
)

// sbomFacts reads the software bills of materials (SBOM) of the commit and of
// the base, and gives each dependency change the licenses and known
// vulnerabilities each side's SBOM lists for it. A side without a readable
// SBOM is unknown and gets no state.
func (r *Result) sbomFacts(in Input) {
	head := r.readSBOM(in, reports.KindSBOM)
	base := r.readSBOM(in, reports.KindBaseSBOM)
	if head == nil && base == nil {
		return
	}
	r.SBOM = &evidence.SBOM{Head: head.document(), Base: base.document()}
	for i := range r.DependencyChanges {
		change := &r.DependencyChanges[i]
		if base != nil && change.Change != evidence.DependencyAdded {
			change.Before = base.state(change.Ecosystem, change.Name, change.From)
		}
		if head != nil && change.Change != evidence.DependencyRemoved {
			change.After = head.state(change.Ecosystem, change.Name, change.To)
		}
	}
}

// readSBOM reads the SBOM report of a kind, nil when there is none or it
// cannot be read.
func (r *Result) readSBOM(in Input, kind string) *sbomIndex {
	for _, report := range in.Reports {
		if report.Kind != kind {
			continue
		}
		abs := report.Path
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(in.Root, filepath.FromSlash(report.Path))
		}
		sbom, diags := readers.ReadFile(abs, readers.ReadSBOM)
		if r.unread(report, diags) {
			return nil
		}
		return newSBOMIndex(sbom)
	}
	return nil
}

// sbomIndex finds the packages of one SBOM by ecosystem, name and version.
type sbomIndex struct {
	sbom     readers.SBOM
	packages map[packageKey][]int
}

type packageKey struct {
	ecosystem, name, version string
}

// purlEcosystems maps a package URL type to the ecosystem of the lockfile
// readers; another type keeps its name.
var purlEcosystems = map[string]string{
	"npm":      lockdiff.EcosystemNPM,
	"composer": lockdiff.EcosystemComposer,
	"golang":   lockdiff.EcosystemGo,
	"pypi":     lockdiff.EcosystemPyPI,
	"cargo":    lockdiff.EcosystemCargo,
}

func newSBOMIndex(sbom readers.SBOM) *sbomIndex {
	index := &sbomIndex{sbom: sbom, packages: map[packageKey][]int{}}
	for i, p := range sbom.Packages {
		// A package without a package URL matches by name and version alone.
		key := packageKey{name: p.Name, version: normalVersion(p.Version)}
		if purl, err := readers.ParsePURL(p.PURL); err == nil {
			key = packageKey{ecosystem: purlEcosystem(purl.Type), name: purl.Name, version: normalVersion(purl.Version)}
			if key.ecosystem == lockdiff.EcosystemPyPI {
				// The Python lockfile readers normalize their names.
				key.name = lockdiff.PyPIName(key.name)
			}
			if key.version == "" {
				key.version = normalVersion(p.Version)
			}
		}
		index.packages[key] = append(index.packages[key], i)
	}
	return index
}

func purlEcosystem(typ string) string {
	if ecosystem, ok := purlEcosystems[typ]; ok {
		return ecosystem
	}
	return typ
}

// normalVersion drops the v prefix that Go and some Composer versions carry
// and some SBOM writers drop.
func normalVersion(version string) string {
	return strings.TrimPrefix(strings.TrimSpace(version), "v")
}

func (s *sbomIndex) document() *evidence.SBOMDocument {
	if s == nil {
		return nil
	}
	return &evidence.SBOMDocument{Format: s.sbom.Format, SpecVersion: s.sbom.SpecVersion}
}

// state is what the SBOM lists for a package at the versions a lockfile
// locks, joined by ", " when there are several. It is nil when the SBOM lists
// the package at none of them.
func (s *sbomIndex) state(ecosystem, name, versions string) *evidence.DependencyState {
	split := strings.Split(versions, ", ")
	found := make([]int, 0, len(split))
	for _, version := range split {
		version = normalVersion(version)
		found = append(found, s.packages[packageKey{ecosystem, name, version}]...)
		found = append(found, s.packages[packageKey{"", name, version}]...)
	}
	if len(found) == 0 {
		return nil
	}
	state := &evidence.DependencyState{}
	severities := map[string]string{}
	for _, i := range found {
		p := s.sbom.Packages[i]
		for _, license := range p.Licenses {
			if license = bounded(license); !slices.Contains(state.Licenses, license) && len(state.Licenses) < evidence.MaxLicenses {
				state.Licenses = append(state.Licenses, license)
			}
		}
		for _, v := range p.Vulnerabilities {
			id := bounded(v.ID)
			if current, seen := severities[id]; !seen || readers.MoreSevere(v.Severity, current) {
				severities[id] = v.Severity
			}
		}
	}
	for id, severity := range severities {
		state.Vulnerabilities = append(state.Vulnerabilities, evidence.Vulnerability{ID: id, Severity: severity})
	}
	// The most severe first, so the bound keeps them.
	slices.SortFunc(state.Vulnerabilities, func(a, b evidence.Vulnerability) int {
		if a.Severity != b.Severity {
			if readers.MoreSevere(a.Severity, b.Severity) {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	if len(state.Vulnerabilities) > evidence.MaxVulnerabilities {
		state.Vulnerabilities = state.Vulnerabilities[:evidence.MaxVulnerabilities]
	}
	return state
}
