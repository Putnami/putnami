package observe

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	readers "go.putnami.dev/evidence/readers"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/analyze"
	"go.putnami.dev/tooling/observe/internal/reports"
)

// sbomEnv is a GitHub Actions pull request job of the sbom demo.
var sbomEnv = map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY": "Putnami/demo-observe-sbom", "GITHUB_REPOSITORY_ID": "1106", "GITHUB_RUN_ID": "41335501400", "GITHUB_JOB": "test", "GITHUB_REF": "refs/pull/1/merge"}

// observeSBOMDemo observes the sbom demo. inRepository writes SBOM files of
// the demo, or the given content, into the working tree, as a CI would; head
// and base name SBOM
// files written outside the repository, empty for none.
func observeSBOMDemo(t *testing.T, inRepository map[string]string, head, base string) Observation {
	t.Helper()
	root, baseSHA := demoRepository(t, "sbom")
	outside := t.TempDir()
	documents := map[string]string{}
	for _, f := range readDemo(t, "sbom") {
		if name, ok := strings.CutPrefix(f.name, "sbom/"); ok {
			documents[name] = f.content
		}
	}
	for target, name := range inRepository {
		content, ok := documents[name]
		if !ok {
			content = name // the content itself
		}
		writeDemoFile(t, root, target, content)
	}
	opts := Options{
		Dir:      root,
		Env:      func(key string) string { return sbomEnv[key] },
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Resolve: func(p string) string {
			if filepath.IsAbs(p) {
				return p
			}
			return filepath.Join(root, p)
		},
		Producer: Producer{Name: "@putnami/observe", Version: "1.0.0"},
	}
	opts.Overrides.Base = baseSHA
	for flag, name := range map[*[]string]string{&opts.SBOM: head, &opts.BaseSBOM: base} {
		if name != "" {
			writeDemoFile(t, outside, name, documents[name])
			*flag = []string{filepath.Join(outside, name)}
		}
	}
	return Run(context.Background(), opts)
}

func summary(t *testing.T, o Observation) string {
	t.Helper()
	var text bytes.Buffer
	if err := WriteText(&text, o); err != nil {
		t.Fatal(err)
	}
	return text.String()
}

func requireLines(t *testing.T, text string, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if !strings.Contains(text, line) {
			t.Errorf("the summary lacks %q:\n%s", line, text)
		}
	}
}

// validEvidence checks the evidence document of o validates and parses back,
// and returns it.
func validEvidence(t *testing.T, o Observation) *evidence.Document {
	t.Helper()
	doc := Evidence(o, now)
	if problems := evidence.ValidateDocument(doc); diag.HasErrors(problems) {
		t.Fatalf("the evidence document does not validate: %v", problems)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, problems := evidence.ParseAndValidate(data); diag.HasErrors(problems) {
		t.Fatalf("the written evidence document does not parse back: %v", problems)
	}
	return doc
}

func dependency(t *testing.T, changes []evidence.DependencyChange, name string) evidence.DependencyChange {
	t.Helper()
	for _, c := range changes {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no dependency change for %s in %+v", name, changes)
	return evidence.DependencyChange{}
}

func ids(state *evidence.DependencyState) []string {
	if state == nil {
		return nil
	}
	var out []string
	for _, v := range state.Vulnerabilities {
		out = append(out, v.ID+" "+v.Severity)
	}
	return out
}

func hasCode(o Observation, code string) bool {
	for _, d := range o.Diagnostics {
		if d.Code == code {
			return true
		}
	}
	return false
}

func noWarnings(t *testing.T, o Observation) {
	t.Helper()
	for _, d := range o.Diagnostics {
		if d.Severity != diag.Info && !strings.HasPrefix(d.Code, "observe.report_missing") {
			t.Errorf("diagnostic %s: %s", d.Code, d.Message)
		}
	}
}

func TestACycloneDXPairGivesEachChangedDependencyItsFacts(t *testing.T) {
	spectest.Proves(t, analysesFeature, "dependency-sbom-facts", "a-cyclonedx-pair-gives-each-changed-dependency-its-facts")
	o := observeSBOMDemo(t, nil, "head.cdx.json", "base.cdx.json")
	noWarnings(t, o)
	doc := validEvidence(t, o)
	want := &evidence.SBOM{
		Head: &evidence.SBOMDocument{Format: evidence.SBOMCycloneDX, SpecVersion: "1.5"},
		Base: &evidence.SBOMDocument{Format: evidence.SBOMCycloneDX, SpecVersion: "1.5"},
	}
	if !reflect.DeepEqual(doc.SBOM, want) {
		t.Fatalf("sbom %+v", doc.SBOM)
	}
	lodash := dependency(t, doc.DependencyChanges, "lodash")
	if got := ids(lodash.Before); !reflect.DeepEqual(got, []string{"GHSA-35jh-r3h4-6jhm high", "GHSA-r5fr-rjxr-66jc high", "GHSA-29mw-wpgm-hmr9 medium", "GHSA-f23m-r3pf-42rh medium", "GHSA-xxjr-mmjv-4gpg medium"}) {
		t.Errorf("lodash before %v", got)
	}
	if got := ids(lodash.After); !reflect.DeepEqual(got, []string{"GHSA-r5fr-rjxr-66jc high", "GHSA-f23m-r3pf-42rh medium", "GHSA-xxjr-mmjv-4gpg medium"}) {
		t.Errorf("lodash after %v", got)
	}
	if !reflect.DeepEqual(lodash.Before.Licenses, []string{"MIT"}) || !reflect.DeepEqual(lodash.After.Licenses, []string{"MIT"}) {
		t.Errorf("lodash licenses %v -> %v", lodash.Before.Licenses, lodash.After.Licenses)
	}
	minimist := dependency(t, doc.DependencyChanges, "minimist")
	if minimist.After != nil || !reflect.DeepEqual(ids(minimist.Before), []string{"GHSA-xvch-5gv4-984h critical"}) {
		t.Errorf("minimist %+v -> %+v", minimist.Before, minimist.After)
	}
	zod := dependency(t, doc.DependencyChanges, "zod")
	if zod.Before != nil || !reflect.DeepEqual(ids(zod.After), []string{"GHSA-mrhx-c7xc-m3w5 high"}) {
		t.Errorf("zod %+v -> %+v", zod.Before, zod.After)
	}
	requireLines(t, summary(t, o), "  dependencies  3 changes, SBOM head CycloneDX 1.5, base CycloneDX 1.5\n")
}

func TestTheSummaryCountsTheVulnerabilitiesPerDependency(t *testing.T) {
	spectest.Proves(t, analysesFeature, "dependency-sbom-facts", "the-summary-counts-the-vulnerabilities-per-dependency")
	o := observeSBOMDemo(t, nil, "head.cdx.json", "base.cdx.json")
	text := summary(t, o)
	requireLines(t, text,
		"    upgraded   lodash 4.17.20 -> 4.17.21, in package-lock.json\n"+
			"               fixes 2 (GHSA-35jh-r3h4-6jhm high, GHSA-29mw-wpgm-hmr9 medium), introduces 0, keeps 3; license MIT\n",
		"    removed    minimist 1.2.5, in package-lock.json\n"+
			"               fixes 1 (GHSA-xvch-5gv4-984h critical); license MIT\n",
		"    added      zod 3.23.8, in package-lock.json\n"+
			"               introduces 1 (GHSA-mrhx-c7xc-m3w5 high); license MIT\n",
	)
	if !strings.Contains(text, "(base)\n") {
		t.Errorf("the reports list does not mark the base SBOM:\n%s", text)
	}

	t.Run("without an SBOM the summary is unchanged", func(t *testing.T) {
		o := observeSBOMDemo(t, nil, "", "")
		if o.Analysis.SBOM != nil || Evidence(o, now).SBOM != nil {
			t.Fatalf("sbom %+v", o.Analysis.SBOM)
		}
		text := summary(t, o)
		requireLines(t, text, "  dependencies  3 changes\n", "    upgraded   lodash 4.17.20 -> 4.17.21, in package-lock.json\n    removed ")
		if strings.Contains(text, "SBOM") {
			t.Errorf("the summary names an SBOM:\n%s", text)
		}
	})

	t.Run("a long list names five and counts the rest", func(t *testing.T) {
		list := make([]evidence.Vulnerability, 0, 7)
		for _, id := range []string{"a", "b", "c", "d", "e", "f", "g"} {
			list = append(list, evidence.Vulnerability{ID: id, Severity: evidence.SeverityLow})
		}
		if got := vulnerabilityList(list); got != "7 (a low, b low, c low, d low, e low, and 2 more)" {
			t.Errorf("list %q", got)
		}
	})

	t.Run("a license change and an unlisted package are named", func(t *testing.T) {
		sbom := &evidence.SBOM{Head: &evidence.SBOMDocument{}, Base: &evidence.SBOMDocument{}}
		upgraded := evidence.DependencyChange{Change: evidence.DependencyUpgraded,
			Before: &evidence.DependencyState{Licenses: []string{"MIT"}}, After: &evidence.DependencyState{Licenses: []string{"BUSL-1.1"}}}
		if got := dependencyFacts(upgraded, sbom); got != "fixes 0, introduces 0, keeps 0; license MIT -> BUSL-1.1" {
			t.Errorf("facts %q", got)
		}
		dropped := evidence.DependencyChange{Change: evidence.DependencyUpgraded,
			Before: &evidence.DependencyState{Licenses: []string{"MIT"}}, After: &evidence.DependencyState{}}
		if got := dependencyFacts(dropped, sbom); got != "fixes 0, introduces 0, keeps 0; license MIT -> none" {
			t.Errorf("facts %q", got)
		}
		unlisted := evidence.DependencyChange{Change: evidence.DependencyUpgraded}
		if got := dependencyFacts(unlisted, sbom); got != "not listed in the base SBOM; not listed in the head SBOM" {
			t.Errorf("facts %q", got)
		}
		baseOnly := &evidence.SBOM{Base: &evidence.SBOMDocument{}}
		upgraded.After = nil
		if got := dependencyFacts(upgraded, baseOnly); got != "0 known at the base, head unknown; license MIT" {
			t.Errorf("facts %q", got)
		}
		added := evidence.DependencyChange{Change: evidence.DependencyAdded}
		if got := dependencyFacts(added, baseOnly); got != "head unknown" {
			t.Errorf("facts %q", got)
		}
	})
}

func TestTheReaderSeveritiesAreTheProtocolOnes(t *testing.T) {
	if !slices.Equal(readers.Severities, evidence.Severities) {
		t.Fatalf("reader severities %v, protocol severities %v", readers.Severities, evidence.Severities)
	}
	if !slices.Equal([]string{readers.FormatCycloneDX, readers.FormatSPDX}, evidence.SBOMFormats) {
		t.Fatalf("reader formats differ from the protocol's %v", evidence.SBOMFormats)
	}
}

func TestWithoutABaseSBOMTheBaseIsUnknown(t *testing.T) {
	spectest.Proves(t, analysesFeature, "dependency-sbom-facts", "without-a-base-sbom-the-base-is-unknown")
	o := observeSBOMDemo(t, nil, "head.cdx.json", "")
	noWarnings(t, o)
	doc := validEvidence(t, o)
	if doc.SBOM == nil || doc.SBOM.Base != nil || doc.SBOM.Head == nil {
		t.Fatalf("sbom %+v", doc.SBOM)
	}
	for _, c := range doc.DependencyChanges {
		if c.Before != nil {
			t.Errorf("%s has a base state without a base SBOM: %+v", c.Name, c.Before)
		}
	}
	requireLines(t, summary(t, o),
		"  dependencies  3 changes, SBOM head CycloneDX 1.5, base unknown\n",
		"               3 (GHSA-r5fr-rjxr-66jc high, GHSA-f23m-r3pf-42rh medium, GHSA-xxjr-mmjv-4gpg medium) known at the head, base unknown; license MIT\n",
		"    removed    minimist 1.2.5, in package-lock.json\n               base unknown\n",
		"               introduces 1 (GHSA-mrhx-c7xc-m3w5 high); license MIT\n",
	)
}

func TestAnSPDXDocumentGivesLicencesAndAdvisories(t *testing.T) {
	spectest.Proves(t, analysesFeature, "dependency-sbom-facts", "an-spdx-document-gives-licenses-and-advisories")
	// The head SBOM is discovered in the working tree; the base one is named.
	o := observeSBOMDemo(t, map[string]string{"sbom.spdx.json": "head-advisories.spdx.json"}, "", "base.spdx.json")
	noWarnings(t, o)
	doc := validEvidence(t, o)
	want := &evidence.SBOM{
		Head: &evidence.SBOMDocument{Format: evidence.SBOMSPDX, SpecVersion: "2.3"},
		Base: &evidence.SBOMDocument{Format: evidence.SBOMSPDX, SpecVersion: "2.3"},
	}
	if !reflect.DeepEqual(doc.SBOM, want) {
		t.Fatalf("sbom %+v", doc.SBOM)
	}
	lodash := dependency(t, doc.DependencyChanges, "lodash")
	if lodash.Before == nil || len(lodash.Before.Vulnerabilities) != 0 || !reflect.DeepEqual(lodash.Before.Licenses, []string{"MIT"}) {
		t.Errorf("lodash before %+v", lodash.Before)
	}
	if got := ids(lodash.After); !reflect.DeepEqual(got, []string{"GHSA-f23m-r3pf-42rh unknown"}) {
		t.Errorf("lodash after %v", got)
	}
	var discovered bool
	for _, r := range o.Reports {
		discovered = discovered || (r.Kind == reports.KindSBOM && r.Path == "sbom.spdx.json" && r.Source == reports.SourceDiscovered)
	}
	if !discovered {
		t.Errorf("the head SBOM was not discovered: %+v", o.Reports)
	}
	requireLines(t, summary(t, o),
		"  dependencies  3 changes, SBOM head SPDX 2.3, base SPDX 2.3\n",
		"               fixes 0, introduces 1 (GHSA-f23m-r3pf-42rh unknown), keeps 0; license MIT\n",
	)

	t.Run("several head SBOMs leave the head unknown", func(t *testing.T) {
		o := observeSBOMDemo(t, map[string]string{"bom.json": "head-advisories.spdx.json", "other.cdx.json": "head.cdx.json"}, "", "")
		if !hasCode(o, reports.CodeSBOMAmbiguous) || o.Analysis.SBOM != nil {
			t.Fatalf("diagnostics %v, sbom %+v", o.Diagnostics, o.Analysis.SBOM)
		}
		validEvidence(t, o)
	})

	t.Run("an SBOM the reader refuses leaves its side unknown", func(t *testing.T) {
		o := observeSBOMDemo(t, map[string]string{"sbom.cdx.json": `{"bomFormat": "CycloneDX", "specVersion": "1.7"}`}, "", "base.cdx.json")
		if !hasCode(o, analyze.CodeReportUnread) {
			t.Fatalf("diagnostics %v", o.Diagnostics)
		}
		doc := validEvidence(t, o)
		if doc.SBOM == nil || doc.SBOM.Head != nil || doc.SBOM.Base == nil {
			t.Fatalf("sbom %+v", doc.SBOM)
		}
		if zod := dependency(t, doc.DependencyChanges, "zod"); zod.After != nil {
			t.Errorf("zod after %+v", zod.After)
		}
	})
}
