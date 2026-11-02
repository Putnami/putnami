package readers

import (
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// vulnerabilityIDs returns the ids of the vulnerabilities of the package
// named name at version, sorted.
func vulnerabilityIDs(t *testing.T, sbom SBOM, name, version string) []string {
	t.Helper()
	for _, p := range sbom.Packages {
		if p.Name == name && p.Version == version {
			var ids []string
			for _, v := range p.Vulnerabilities {
				ids = append(ids, v.ID+" "+v.Severity)
			}
			slices.Sort(ids)
			return ids
		}
	}
	t.Fatalf("%s@%s is not in the SBOM", name, version)
	return nil
}

func TestARealCycloneDXDocumentReadsToItsExpectedPackages(t *testing.T) {
	spectest.Proves(t, feature, "cyclonedx-reads-components", "a-real-cyclonedx-document-reads-to-its-expected-packages")
	for _, fixture := range []string{"testdata/sbom/base.cdx.json", "testdata/sbom/head.cdx.json"} {
		sbom, diagnostics := ReadSBOM(read(t, fixture))
		if len(diagnostics) != 0 {
			t.Fatalf("%s: %v", fixture, diagnostics)
		}
		if sbom.Format != FormatCycloneDX || sbom.SpecVersion != "1.5" {
			t.Errorf("%s: format %s %s", fixture, sbom.Format, sbom.SpecVersion)
		}
		golden(t, fixture, sbom)
	}
	base, _ := ReadSBOM(read(t, "testdata/sbom/base.cdx.json"))
	head, _ := ReadSBOM(read(t, "testdata/sbom/head.cdx.json"))
	// lodash 4.17.21 fixes two of the five advisories of 4.17.20.
	if got := vulnerabilityIDs(t, base, "lodash", "4.17.20"); len(got) != 5 {
		t.Errorf("lodash 4.17.20: %v, want 5 advisories", got)
	}
	if got := vulnerabilityIDs(t, head, "lodash", "4.17.21"); !slices.Equal(got, []string{"GHSA-f23m-r3pf-42rh medium", "GHSA-r5fr-rjxr-66jc high", "GHSA-xxjr-mmjv-4gpg medium"}) {
		t.Errorf("lodash 4.17.21: %v", got)
	}
	if got := vulnerabilityIDs(t, base, "minimist", "1.2.5"); !slices.Equal(got, []string{"GHSA-xvch-5gv4-984h critical"}) {
		t.Errorf("minimist 1.2.5: %v", got)
	}
	for _, p := range head.Packages {
		if !slices.Equal(p.Licenses, []string{"MIT"}) || !strings.HasPrefix(p.PURL, "pkg:npm/") {
			t.Errorf("%s: licenses %v, purl %s", p.Name, p.Licenses, p.PURL)
		}
	}
}

func TestAVulnerabilityGoesToTheComponentsItAffects(t *testing.T) {
	spectest.Proves(t, feature, "cyclonedx-reads-components", "a-vulnerability-goes-to-the-components-it-affects")
	document := `{
  "bomFormat": "CycloneDX",
  "specVersion": "1.6",
  "components": [
    {"bom-ref": "ref-a", "name": "a", "version": "1.0.0", "purl": "pkg:npm/a@1.0.0",
     "licenses": [{"license": {"id": "MIT"}}, {"license": {"name": "Custom"}}, {"expression": "MIT OR Apache-2.0"}, {"license": {"id": "MIT"}}],
     "components": [{"bom-ref": "ref-b", "group": "@scope", "name": "b", "version": "2.0.0", "purl": "pkg:npm/%40scope/b@2.0.0"}]},
    {"bom-ref": "ref-a2", "name": "a", "version": "2.0.0", "purl": "pkg:npm/a@2.0.0"},
    {"name": "c", "version": "v1.2.0", "purl": "pkg:golang/example.com/c@v1.2.0"}
  ],
  "vulnerabilities": [
    {"id": "CVE-1", "ratings": [{"severity": "low"}, {"severity": "critical"}, {"severity": "bogus"}], "affects": [{"ref": "ref-a"}]},
    {"id": "CVE-2", "affects": [{"ref": "pkg:npm/a"}]},
    {"id": "CVE-3", "ratings": [{"severity": "HIGH"}], "affects": [{"ref": "urn:cdx:3e671687-395b-41f5-a30f-a58921a69b79/1#ref-b"}, {"ref": "pkg:golang/example.com/c@v1.2.0?type=module"}]},
    {"id": "CVE-4", "ratings": [{"severity": "medium"}], "affects": [{"ref": "pkg:npm/a@2.0.0"}, {"ref": "ref-a2"}]},
    {"id": "", "affects": [{"ref": "ref-a"}]},
    {"id": "CVE-5", "affects": [{"ref": "unknown-ref"}, {"ref": "urn:cdx:x/1"}]}
  ]
}`
	sbom, diagnostics := ReadSBOM([]byte(document))
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	for _, want := range []struct {
		name, version string
		ids           []string
	}{
		{"a", "1.0.0", []string{"CVE-1 critical", "CVE-2 unknown"}},
		{"@scope/b", "2.0.0", []string{"CVE-3 high"}},
		{"a", "2.0.0", []string{"CVE-2 unknown", "CVE-4 medium"}},
		{"c", "v1.2.0", []string{"CVE-3 high"}},
	} {
		if got := vulnerabilityIDs(t, sbom, want.name, want.version); !slices.Equal(got, want.ids) {
			t.Errorf("%s@%s: %v, want %v", want.name, want.version, got, want.ids)
		}
	}
	if got := sbom.Packages[0].Licenses; !slices.Equal(got, []string{"MIT", "Custom", "MIT OR Apache-2.0"}) {
		t.Errorf("licenses %v", got)
	}
	t.Run("the versions of an affects entry narrow it", func(t *testing.T) {
		// osv-scanner names a package without its version and states no
		// versions: every version the SBOM lists gets the advisory.
		document := `{
  "bomFormat": "CycloneDX",
  "specVersion": "1.5",
  "components": [
    {"name": "minimist", "version": "0.0.8", "purl": "pkg:npm/minimist@0.0.8"},
    {"name": "minimist", "version": "1.2.8", "purl": "pkg:npm/minimist@1.2.8"},
    {"name": "minimist", "purl": "pkg:npm/minimist@0.0.8?source=lockfile"}
  ],
  "vulnerabilities": [
    {"id": "GHSA-exact", "affects": [{"ref": "pkg:npm/minimist", "versions": [{"version": "0.0.8", "status": "affected"}]}]},
    {"id": "GHSA-unaffected", "affects": [{"ref": "pkg:npm/minimist", "versions": [{"version": "1.2.8", "status": "unaffected"}]}]},
    {"id": "GHSA-range", "affects": [{"ref": "pkg:npm/minimist", "versions": [{"version": "0.0.8"}, {"range": "vers:npm/<1.2.6"}]}]},
    {"id": "GHSA-unstated", "affects": [{"ref": "pkg:npm/minimist"}]}
  ]
}`
		sbom, diagnostics := ReadSBOM([]byte(document))
		if len(diagnostics) != 0 {
			t.Fatal(diagnostics)
		}
		if got := vulnerabilityIDs(t, sbom, "minimist", "0.0.8"); !slices.Equal(got, []string{"GHSA-exact unknown", "GHSA-range unknown", "GHSA-unaffected unknown", "GHSA-unstated unknown"}) {
			t.Errorf("minimist@0.0.8: %v", got)
		}
		if got := vulnerabilityIDs(t, sbom, "minimist", "1.2.8"); !slices.Equal(got, []string{"GHSA-range unknown", "GHSA-unstated unknown"}) {
			t.Errorf("minimist@1.2.8: %v", got)
		}
		// A component without a version takes the one of its package URL.
		if got := vulnerabilityIDs(t, sbom, "minimist", ""); !slices.Equal(got, []string{"GHSA-exact unknown", "GHSA-range unknown", "GHSA-unaffected unknown", "GHSA-unstated unknown"}) {
			t.Errorf("minimist without a version: %v", got)
		}
	})
	purl, err := ParsePURL(sbom.Packages[1].PURL)
	if err != nil || purl.Name != "@scope/b" || purl.Version != "2.0.0" || purl.Type != "npm" {
		t.Errorf("purl %+v, %v", purl, err)
	}
}

func TestARealSPDXDocumentReadsToItsExpectedPackages(t *testing.T) {
	spectest.Proves(t, feature, "spdx-reads-packages", "a-real-spdx-document-reads-to-its-expected-packages")
	for _, fixture := range []string{"testdata/sbom/base.spdx.json", "testdata/sbom/head.spdx.json", "testdata/sbom/head-advisories.spdx.json"} {
		sbom, diagnostics := ReadSBOM(read(t, fixture))
		if len(diagnostics) != 0 {
			t.Fatalf("%s: %v", fixture, diagnostics)
		}
		if sbom.Format != FormatSPDX || sbom.SpecVersion != "2.3" || len(sbom.Packages) != 2 {
			t.Errorf("%s: %s %s, %d packages: the described root package is left out", fixture, sbom.Format, sbom.SpecVersion, len(sbom.Packages))
		}
		golden(t, fixture, sbom)
	}
	head, _ := ReadSBOM(read(t, "testdata/sbom/head-advisories.spdx.json"))
	if got := vulnerabilityIDs(t, head, "zod", "3.23.8"); !slices.Equal(got, []string{"GHSA-mrhx-c7xc-m3w5 unknown"}) {
		t.Errorf("zod: %v", got)
	}
	// A cpe23Type reference is no advisory.
	if got := vulnerabilityIDs(t, head, "lodash", "4.17.21"); !slices.Equal(got, []string{"GHSA-f23m-r3pf-42rh unknown"}) {
		t.Errorf("lodash: %v", got)
	}
	// The concluded license wins; NOASSERTION and NONE are no license.
	sbom, _ := ReadSBOM([]byte(`{"spdxVersion": "SPDX-2.3", "documentDescribes": ["SPDXRef-root"], "packages": [
		{"SPDXID": "SPDXRef-root", "name": "root"},
		{"SPDXID": "SPDXRef-a", "name": "a", "versionInfo": "1", "licenseConcluded": "NOASSERTION", "licenseDeclared": "Apache-2.0"},
		{"SPDXID": "SPDXRef-b", "name": "b", "versionInfo": "1", "licenseConcluded": "NONE", "licenseDeclared": "NOASSERTION",
		 "externalRefs": [{"referenceCategory": "PACKAGE_MANAGER", "referenceType": "purl", "referenceLocator": "pkg:composer/acme/b@1"}]}]}`))
	if len(sbom.Packages) != 2 || !slices.Equal(sbom.Packages[0].Licenses, []string{"Apache-2.0"}) || sbom.Packages[1].Licenses != nil || sbom.Packages[1].PURL != "pkg:composer/acme/b@1" {
		t.Errorf("packages %+v", sbom.Packages)
	}
	// An advisory locator without a last segment names no advisory.
	for locator, want := range map[string]string{"https://osv.dev/vulnerability/GHSA-x/": "GHSA-x", "https://osv.dev/": "", "": "", "/": ""} {
		if got := advisoryID(locator); got != want {
			t.Errorf("advisoryID(%q) = %q, want %q", locator, got, want)
		}
	}
}

func TestAnUnsupportedSBOMIsMalformed(t *testing.T) {
	spectest.Proves(t, feature, "cyclonedx-reads-components", "an-unsupported-sbom-is-malformed")
	for label, document := range map[string]string{
		"CycloneDX 1.3":      `{"bomFormat": "CycloneDX", "specVersion": "1.3"}`,
		"CycloneDX 1.7":      `{"bomFormat": "CycloneDX", "specVersion": "1.7"}`,
		"SPDX 2.2":           `{"spdxVersion": "SPDX-2.2"}`,
		"neither":            `{"name": "package.json"}`,
		"not an object":      `[1, 2]`,
		"wrong member types": `{"bomFormat": "CycloneDX", "specVersion": "1.5", "components": {}}`,
		"wrong SPDX types":   `{"spdxVersion": "SPDX-2.3", "packages": "x"}`,
		"CycloneDX XML":      `<bom xmlns="http://cyclonedx.org/schema/bom/1.5"/>`,
	} {
		if _, diagnostics := ReadSBOM([]byte(document)); len(diagnostics) != 1 || diagnostics[0].Code != CodeMalformed {
			t.Errorf("%s: %v, want %s", label, diagnostics, CodeMalformed)
		}
	}
	for _, raw := range []string{"", "npm/a@1", "pkg:", "pkg:npm", "pkg:npm/", "pkg:npm/%zz@1", "pkg:npm/a@%zz"} {
		if _, err := ParsePURL(raw); err == nil {
			t.Errorf("ParsePURL(%q) accepted", raw)
		}
	}
	if !MoreSevere(SeverityCritical, SeverityHigh) || MoreSevere("bogus", SeverityUnknown) || !MoreSevere(SeverityNone, "bogus") {
		t.Error("severity order")
	}
}
