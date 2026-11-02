package readers

import (
	"encoding/json"
	"errors"
	"net/url"
	"path"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Software bill of materials (SBOM) formats.
const (
	FormatCycloneDX = "cyclonedx"
	FormatSPDX      = "spdx"
)

// CycloneDXVersions and SPDXVersions are the spec versions ReadSBOM reads.
var (
	CycloneDXVersions = []string{"1.4", "1.5", "1.6"}
	SPDXVersions      = []string{"2.3"}
)

// Vulnerability severities, from the most to the least severe, unknown last.
const (
	SeverityCritical = "critical"
	SeverityHigh     = "high"
	SeverityMedium   = "medium"
	SeverityLow      = "low"
	SeverityInfo     = "info"
	SeverityNone     = "none"
	SeverityUnknown  = "unknown"
)

// Severities orders the severities from the most to the least severe.
var Severities = []string{SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow, SeverityInfo, SeverityNone, SeverityUnknown}

// SBOM is one software bill of materials.
type SBOM struct {
	// Format is cyclonedx or spdx.
	Format string `json:"format"`
	// SpecVersion is the version of the format the document states: 1.5, 2.3.
	SpecVersion string `json:"specVersion"`
	// Packages are the packages the document lists, in document order.
	Packages []Package `json:"packages"`
}

// Package is one package an SBOM lists.
type Package struct {
	// Name is the package's name as the document states it.
	Name string `json:"name"`
	// Version is the package's version as the document states it.
	Version string `json:"version,omitempty"`
	// PURL is the package URL, when the document states one.
	PURL string `json:"purl,omitempty"`
	// Licenses are the package's licenses: SPDX ids, expressions or names.
	Licenses []string `json:"licenses,omitempty"`
	// Vulnerabilities are the package's known vulnerabilities, by id.
	Vulnerabilities []Vulnerability `json:"vulnerabilities,omitempty"`
}

// Vulnerability is one known vulnerability.
type Vulnerability struct {
	// ID is the advisory's id: CVE-..., GHSA-..., GO-...
	ID string `json:"id"`
	// Severity is one of Severities.
	Severity string `json:"severity"`
}

// MoreSevere reports whether severity a ranks above b. An unknown value ranks
// last.
func MoreSevere(a, b string) bool {
	return severityRank(a) < severityRank(b)
}

func severityRank(severity string) int {
	if rank := slices.Index(Severities, severity); rank >= 0 {
		return rank
	}
	return len(Severities)
}

// ReadSBOM reads a CycloneDX 1.4 to 1.6 or an SPDX 2.3 JSON document.
func ReadSBOM(data []byte) (SBOM, []diag.Diagnostic) {
	if d := tooLarge(data); d != nil {
		return SBOM{}, d
	}
	var head struct {
		BOMFormat   string `json:"bomFormat"`
		SPDXVersion string `json:"spdxVersion"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return SBOM{}, malformed("the SBOM is not a JSON object: %v", err)
	}
	switch {
	case head.BOMFormat == "CycloneDX":
		return readCycloneDX(data)
	case head.SPDXVersion != "":
		return readSPDX(data)
	}
	return SBOM{}, malformed("the document is neither CycloneDX (bomFormat) nor SPDX (spdxVersion)")
}

type cdxComponent struct {
	BOMRef     string         `json:"bom-ref"`
	Group      string         `json:"group"`
	Name       string         `json:"name"`
	Version    string         `json:"version"`
	PURL       string         `json:"purl"`
	Licenses   []cdxLicense   `json:"licenses"`
	Components []cdxComponent `json:"components"`
}

type cdxLicense struct {
	License *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"license"`
	Expression string `json:"expression"`
}

func readCycloneDX(data []byte) (SBOM, []diag.Diagnostic) {
	var bom struct {
		SpecVersion     string         `json:"specVersion"`
		Components      []cdxComponent `json:"components"`
		Vulnerabilities []struct {
			ID      string `json:"id"`
			Ratings []struct {
				Severity string `json:"severity"`
			} `json:"ratings"`
			Affects []struct {
				Ref      string               `json:"ref"`
				Versions []cdxAffectedVersion `json:"versions"`
			} `json:"affects"`
		} `json:"vulnerabilities"`
	}
	if err := json.Unmarshal(data, &bom); err != nil {
		return SBOM{}, malformed("the CycloneDX document does not decode: %v", err)
	}
	if !slices.Contains(CycloneDXVersions, bom.SpecVersion) {
		return SBOM{}, malformed("CycloneDX %q is not read: specVersion %s are", bom.SpecVersion, strings.Join(CycloneDXVersions, ", "))
	}
	out := SBOM{Format: FormatCycloneDX, SpecVersion: bom.SpecVersion, Packages: []Package{}}
	byRef := map[string][]int{}
	byPURL := map[string][]int{}
	var walk func([]cdxComponent)
	walk = func(components []cdxComponent) {
		for _, c := range components {
			name := c.Name
			if c.Group != "" {
				name = c.Group + "/" + c.Name
			}
			index := len(out.Packages)
			out.Packages = append(out.Packages, Package{Name: name, Version: c.Version, PURL: c.PURL, Licenses: cdxLicenses(c.Licenses)})
			if c.BOMRef != "" {
				byRef[c.BOMRef] = append(byRef[c.BOMRef], index)
			}
			if purl, err := ParsePURL(c.PURL); err == nil {
				byPURL[purl.versioned()] = append(byPURL[purl.versioned()], index)
				byPURL[purl.unversioned()] = append(byPURL[purl.unversioned()], index)
			}
			walk(c.Components)
		}
	}
	walk(bom.Components)
	for _, v := range bom.Vulnerabilities {
		if strings.TrimSpace(v.ID) == "" {
			continue
		}
		severity := SeverityUnknown
		for _, rating := range v.Ratings {
			if s := strings.ToLower(rating.Severity); slices.Contains(Severities, s) && s != SeverityUnknown && MoreSevere(s, severity) {
				severity = s
			}
		}
		affected := map[int]bool{}
		for _, a := range v.Affects {
			for _, index := range affectedVersions(affectedPackages(a.Ref, byRef, byPURL), a.Versions, out.Packages) {
				affected[index] = true
			}
		}
		for index := range affected {
			addVulnerability(&out.Packages[index], Vulnerability{ID: v.ID, Severity: severity})
		}
	}
	return out, nil
}

// affectedPackages resolves an affects ref: a bom-ref, the bom-ref part of a
// BOM-Link (urn:cdx:serial/version#ref), else a package URL with or without
// its version.
func affectedPackages(ref string, byRef, byPURL map[string][]int) []int {
	if indexes, ok := byRef[ref]; ok {
		return indexes
	}
	if strings.HasPrefix(ref, "urn:cdx:") {
		if _, fragment, ok := strings.Cut(ref, "#"); ok {
			if decoded, err := url.PathUnescape(fragment); err == nil {
				return byRef[decoded]
			}
		}
		return nil
	}
	purl, err := ParsePURL(ref)
	if err != nil {
		return nil
	}
	if purl.Version != "" {
		return byPURL[purl.versioned()]
	}
	return byPURL[purl.unversioned()]
}

// cdxAffectedVersion is one entry of an affects ref's versions: an exact
// version or a range, affected unless its status says otherwise.
type cdxAffectedVersion struct {
	Version string `json:"version"`
	Range   string `json:"range"`
	Status  string `json:"status"`
}

// affectedVersions keeps the packages an affects entry's versions name. A
// version stated unaffected leaves; when exact affected versions are stated
// and no affected range, only those stay. Without versions, as osv-scanner
// writes them, every version of the package the ref names stays.
func affectedVersions(indexes []int, versions []cdxAffectedVersion, packages []Package) []int {
	affected, unaffected := map[string]bool{}, map[string]bool{}
	ranged := false
	for _, v := range versions {
		status := strings.ToLower(strings.TrimSpace(v.Status))
		switch {
		case v.Range != "":
			ranged = ranged || status != "unaffected"
		case v.Version == "":
		case status == "unaffected":
			unaffected[strings.TrimPrefix(v.Version, "v")] = true
		default:
			affected[strings.TrimPrefix(v.Version, "v")] = true
		}
	}
	return slices.DeleteFunc(slices.Clone(indexes), func(index int) bool {
		version := strings.TrimPrefix(packages[index].Version, "v")
		return unaffected[version] || (len(affected) > 0 && !ranged && !affected[version])
	})
}

func addVulnerability(p *Package, v Vulnerability) {
	for i, existing := range p.Vulnerabilities {
		if existing.ID == v.ID {
			if MoreSevere(v.Severity, existing.Severity) {
				p.Vulnerabilities[i].Severity = v.Severity
			}
			return
		}
	}
	p.Vulnerabilities = append(p.Vulnerabilities, v)
}

func cdxLicenses(list []cdxLicense) []string {
	var out []string
	for _, l := range list {
		var value string
		switch {
		case l.Expression != "":
			value = l.Expression
		case l.License != nil && l.License.ID != "":
			value = l.License.ID
		case l.License != nil:
			value = l.License.Name
		}
		if value = strings.TrimSpace(value); value != "" && !slices.Contains(out, value) {
			out = append(out, value)
		}
	}
	return out
}

func readSPDX(data []byte) (SBOM, []diag.Diagnostic) {
	var doc struct {
		SPDXVersion       string   `json:"spdxVersion"`
		DocumentDescribes []string `json:"documentDescribes"`
		Packages          []struct {
			SPDXID           string `json:"SPDXID"`
			Name             string `json:"name"`
			VersionInfo      string `json:"versionInfo"`
			LicenseConcluded string `json:"licenseConcluded"`
			LicenseDeclared  string `json:"licenseDeclared"`
			ExternalRefs     []struct {
				Category string `json:"referenceCategory"`
				Type     string `json:"referenceType"`
				Locator  string `json:"referenceLocator"`
			} `json:"externalRefs"`
		} `json:"packages"`
		Relationships []struct {
			Element string `json:"spdxElementId"`
			Related string `json:"relatedSpdxElement"`
			Type    string `json:"relationshipType"`
		} `json:"relationships"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return SBOM{}, malformed("the SPDX document does not decode: %v", err)
	}
	version := strings.TrimPrefix(doc.SPDXVersion, "SPDX-")
	if !slices.Contains(SPDXVersions, version) {
		return SBOM{}, malformed("SPDX %q is not read: spdxVersion SPDX-%s is", doc.SPDXVersion, strings.Join(SPDXVersions, ", SPDX-"))
	}
	described := map[string]bool{}
	for _, id := range doc.DocumentDescribes {
		described[id] = true
	}
	for _, r := range doc.Relationships {
		if r.Element == "SPDXRef-DOCUMENT" && r.Type == "DESCRIBES" {
			described[r.Related] = true
		}
	}
	out := SBOM{Format: FormatSPDX, SpecVersion: version, Packages: []Package{}}
	for _, p := range doc.Packages {
		if described[p.SPDXID] {
			continue
		}
		pkg := Package{Name: p.Name, Version: p.VersionInfo}
		if license := spdxLicense(p.LicenseConcluded); license != "" {
			pkg.Licenses = []string{license}
		} else if license := spdxLicense(p.LicenseDeclared); license != "" {
			pkg.Licenses = []string{license}
		}
		for _, ref := range p.ExternalRefs {
			category := strings.ReplaceAll(ref.Category, "_", "-")
			switch {
			case category == "PACKAGE-MANAGER" && ref.Type == "purl" && pkg.PURL == "":
				pkg.PURL = ref.Locator
			case category == "SECURITY" && ref.Type == "advisory":
				if id := advisoryID(ref.Locator); id != "" {
					addVulnerability(&pkg, Vulnerability{ID: id, Severity: SeverityUnknown})
				}
			}
		}
		out.Packages = append(out.Packages, pkg)
	}
	return out, nil
}

func spdxLicense(value string) string {
	value = strings.TrimSpace(value)
	if value == "NOASSERTION" || value == "NONE" {
		return ""
	}
	return value
}

// advisoryID names an advisory by the last segment of its locator:
// https://nvd.nist.gov/vuln/detail/CVE-2021-23337 is CVE-2021-23337.
func advisoryID(locator string) string {
	locator = strings.TrimSpace(locator)
	if parsed, err := url.Parse(locator); err == nil && parsed.Scheme != "" {
		locator = parsed.Path
	}
	id := path.Base(strings.TrimRight(locator, "/"))
	if id == "." || id == "/" {
		return ""
	}
	return id
}

// PURL is a parsed package URL: pkg:type/namespace/name@version, without its
// qualifiers and subpath.
type PURL struct {
	Type    string
	Name    string
	Version string
}

// ParsePURL reads a package URL. The name joins the namespace and the name
// with a slash, decoded: pkg:npm/%40scope/pkg is @scope/pkg. The type is
// lowercase.
func ParsePURL(raw string) (PURL, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(raw), "pkg:")
	if !ok {
		return PURL{}, errNotPURL
	}
	rest = strings.TrimLeft(rest, "/")
	if cut := strings.IndexAny(rest, "?#"); cut >= 0 {
		rest = rest[:cut]
	}
	typ, rest, ok := strings.Cut(rest, "/")
	if !ok || typ == "" || rest == "" {
		return PURL{}, errNotPURL
	}
	var version string
	if at := strings.LastIndexByte(rest, '@'); at > 0 {
		rest, version = rest[:at], rest[at+1:]
	}
	segments := strings.Split(strings.Trim(rest, "/"), "/")
	for i, segment := range segments {
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			return PURL{}, errNotPURL
		}
		segments[i] = decoded
	}
	decodedVersion, err := url.PathUnescape(version)
	if err != nil {
		return PURL{}, errNotPURL
	}
	return PURL{Type: strings.ToLower(typ), Name: strings.Join(segments, "/"), Version: decodedVersion}, nil
}

func (p PURL) unversioned() string { return p.Type + "/" + p.Name }
func (p PURL) versioned() string   { return p.unversioned() + "@" + p.Version }

var errNotPURL = errors.New("not a package URL")
