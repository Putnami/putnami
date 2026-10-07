package sdd

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	archproto "go.putnami.dev/protocol/architecture"
	capabilityproto "go.putnami.dev/protocol/capabilities"
	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
)

// This repository's own ARC/DARC adoption, ratcheted.
//
// The gate already answers "are the declarations consistent with the graph".
// These assertions answer the questions the gate cannot: is every project in the
// workspace CLASSIFIED, is the exclusion list owned, and does the evidence in the
// committed capability manifests belong to the projects that claim it.
//
// They deliberately verify WIRING, not protocol semantics. What a mode, a
// finding, or a coverage tier means is decided by
// `go.putnami.dev/protocol/architecture` and held to the cross-language corpus in
// protocols/architecture/fixtures/conformance. A second interpretation here would
// be a second opinion, and the drift between two opinions is what the corpus
// exists to prevent.

// adoptionMatrixPath is the committed classification of every project. The test
// and the document are held together below, so a project cannot be excluded in
// code without the reason being published.
const adoptionMatrixPath = "protocols/architecture/doc/repository-adoption-matrix.md"

// declaredDomains is the reviewed domain membership of this repository. Adding or
// removing a domain is an architecture decision, so it fails here until this list
// says so too.
var declaredDomains = []string{
	"agent-workflows",
	"cli",
	"extension-providers",
	"extension-sdk",
	"go-framework",
	"observability",
	"protocols",
	"public-docs",
	"sdd",
	"typescript-framework",
}

// excludedProjects is the frontier's other half: every workspace project that is
// deliberately NOT mapped to a domain, with the reason it is out.
//
// An exclusion is an owned decision, exactly like a domain membership. Samples
// are proofs of the public API, templates are scaffold inputs and template
// proofs run those inputs against the framework; none of them owns a fact, and
// mapping them would inflate the coverage number without authorizing anything
// real. A sample that grows into a first-party workload stops being a
// sample — the fix is to map it, not to widen this reason.
// The standalone Intelligence collector is excluded separately: no repository
// project consumes its anonymous payload, and no existing domain owns the
// external report's assessment semantics.
var excludedProjects = map[string]string{
	"/go/samples/application":                              reasonSample,
	"/go/samples/capabilities-proof":                       reasonSample,
	"/go/samples/library":                                  reasonSample,
	"/go/samples/migrations-feature":                       reasonSample,
	"/go/samples/service-to-service":                       reasonSample,
	"/go/samples/service-to-service/clients/ts":            reasonGeneratedClient,
	"/go/samples/service-to-service/consumer-ts":           reasonSample,
	"/go/samples/simple-api":                               reasonSample,
	"/go/samples/task-api":                                 reasonSample,
	"/go/samples/unit-of-work-proof":                       reasonSample,
	"/go/templates/go-library":                             reasonTemplate,
	"/go/templates/go-server":                              reasonTemplate,
	"/go/templates/proof":                                  reasonTemplateProof,
	"/intelligence/agent-readiness":                        reasonAgentReadinessCollector,
	"/python/samples/application":                          reasonSample,
	"/python/samples/library":                              reasonSample,
	"/python/templates/python-library":                     reasonTemplate,
	"/python/templates/python-server":                      reasonTemplate,
	"/typescript/samples/01-hello-world":                   reasonSample,
	"/typescript/samples/02-rest-api":                      reasonSample,
	"/typescript/samples/03-web":                           reasonSample,
	"/typescript/samples/04-configuration":                 reasonSample,
	"/typescript/samples/05-dependency-injection":          reasonSample,
	"/typescript/samples/06-database":                      reasonSample,
	"/typescript/samples/07-authentication":                reasonSample,
	"/typescript/samples/08-real-time":                     reasonSample,
	"/typescript/samples/09-events":                        reasonSample,
	"/typescript/samples/10-service-to-service":            reasonSample,
	"/typescript/samples/10-service-to-service/clients/go": reasonGeneratedClient,
	"/typescript/samples/10-service-to-service/clients/ts": reasonGeneratedClient,
	"/typescript/samples/11-storage":                       reasonSample,
	"/typescript/samples/12-caching":                       reasonSample,
	"/typescript/samples/13-fullstack-app":                 reasonSample,
	"/typescript/samples/14-capabilities":                  reasonSample,
	"/typescript/templates/proof":                          reasonTemplateProof,
	"/typescript/templates/typescript-library":             reasonTemplate,
	"/typescript/templates/typescript-server":              reasonTemplate,
	"/typescript/templates/typescript-web":                 reasonTemplate,
}

const (
	reasonSample                  = "sample: an executable proof of the public API, owned by the language vertical it demonstrates"
	reasonTemplate                = "template: scaffold input packaged by @putnami/scaffold, owned by the language vertical"
	reasonGeneratedClient         = "generated client: produced by @putnami/clientgen inside a sample, owned by the sample"
	reasonTemplateProof           = "template proof: renders the templates beside it against the workspace framework and runs their tests, owned by the language vertical"
	reasonAgentReadinessCollector = "standalone Intelligence repository collector: no repository project consumes its anonymous payload or owns the external report's assessment semantics, owned by intelligence"
)

// expectedEvidenceRows is every DARC evidence row this repository commits, as the
// exact triple the gate joins on.
//
// It is pinned because the alternative — asserting "at least one row exists" —
// would pass while a domain silently stopped enforcing a contract. Ordering and
// human-facing text are NOT pinned: the row's identity is its owner, its import
// and its mode.
var expectedEvidenceRows = []evidenceRow{
	{Project: "/sites/putnami.dev", Import: "public-docs.application-runtime.v1", Mode: "reference"},
	{Project: "/sites/putnami.dev", Import: "public-docs.go-framework-documentation.v1", Mode: "snapshot"},
	{Project: "/sites/putnami.dev", Import: "public-docs.method-documentation.v1", Mode: "snapshot"},
	{Project: "/sites/putnami.dev", Import: "public-docs.python-surface-documentation.v1", Mode: "snapshot"},
	{Project: "/sites/putnami.dev", Import: "public-docs.typescript-framework-documentation.v1", Mode: "snapshot"},
	{Project: "/sites/putnami.dev", Import: "public-docs.workspace-documentation.v1", Mode: "snapshot"},
	{Project: "/sites/telemetry.putnami.dev", Import: "observability.application-runtime.v1", Mode: "reference"},
	{Project: "/sites/telemetry.putnami.dev", Import: "observability.protocol-contracts.v1", Mode: "reference"},
}

type evidenceRow struct {
	Project string
	Import  string
	Mode    string
}

// TestEveryWorkspaceProjectIsClassified is the frontier ratchet.
//
// A project added to this workspace is mapped to exactly one domain or listed as
// an owned exclusion. There is no third state, and "nobody got round to it" is
// the state this test exists to make impossible: an unclassified project would
// otherwise sit outside ARC coverage while the domain count kept rising.
func TestEveryWorkspaceProjectIsClassified(t *testing.T) {
	root := workspaceRoot(t)
	discovered := discoverProjects(t, root)
	if len(discovered) < 100 {
		t.Fatalf("discovered %d projects; this workspace has more than 100, so the walk is broken and every assertion below would pass vacuously", len(discovered))
	}
	mapped := mappedProjects(t, root)

	var unclassified, both []string
	for _, id := range discovered {
		_, isMapped := mapped[id]
		_, isExcluded := excludedProjects[id]
		switch {
		case isMapped && isExcluded:
			both = append(both, id)
		case !isMapped && !isExcluded:
			unclassified = append(unclassified, id)
		}
	}
	if len(unclassified) > 0 {
		t.Errorf("these projects belong to no domain and carry no exclusion reason:\n  %s\nmap each one in a putnami.architecture.json, or add it to excludedProjects with the reason it is out of the frontier",
			strings.Join(unclassified, "\n  "))
	}
	if len(both) > 0 {
		t.Errorf("these projects are both mapped to a domain and excluded:\n  %s", strings.Join(both, "\n  "))
	}

	known := make(map[string]bool, len(discovered))
	for _, id := range discovered {
		known[id] = true
	}
	var phantomExclusion, phantomMembership []string
	for id := range excludedProjects {
		if !known[id] {
			phantomExclusion = append(phantomExclusion, id)
		}
	}
	for id := range mapped {
		if !known[id] {
			phantomMembership = append(phantomMembership, id)
		}
	}
	sort.Strings(phantomExclusion)
	sort.Strings(phantomMembership)
	if len(phantomExclusion) > 0 {
		t.Errorf("these exclusions name projects the workspace no longer contains:\n  %s", strings.Join(phantomExclusion, "\n  "))
	}
	if len(phantomMembership) > 0 {
		t.Errorf("these domain memberships name projects the workspace no longer contains:\n  %s", strings.Join(phantomMembership, "\n  "))
	}
}

// TestDomainMembershipIsExclusiveAndReviewed holds the domain list itself.
//
// Two domains claiming one project is not a naming accident: it means two owners
// believe they decide the same facts, and the gate would authorize a dependency
// through whichever manifest it read first.
func TestDomainMembershipIsExclusiveAndReviewed(t *testing.T) {
	root := workspaceRoot(t)
	owner := map[string]string{}
	var duplicates []string
	domains := map[string]bool{}
	for _, source := range repositoryManifests(t, root) {
		domains[source.Manifest.Domain] = true
		for _, project := range source.Manifest.Projects {
			if previous, claimed := owner[project]; claimed {
				duplicates = append(duplicates, project+" ("+previous+" and "+source.Manifest.Domain+")")
				continue
			}
			owner[project] = source.Manifest.Domain
		}
	}
	sort.Strings(duplicates)
	if len(duplicates) > 0 {
		t.Errorf("these projects are claimed by more than one domain:\n  %s", strings.Join(duplicates, "\n  "))
	}

	got := make([]string, 0, len(domains))
	for domain := range domains {
		got = append(got, domain)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(declaredDomains, ",") {
		t.Errorf("declared domains = %v, want %v; adding or retiring a domain is an architecture decision, so update declaredDomains in the same change", got, declaredDomains)
	}
}

// TestRepositoryCarriesNoArchitectureDebt keeps the posture this adoption started
// from.
//
// A baseline records debt that already existed at adoption; a waiver tolerates a
// later violation. Both are legitimate and neither exists here, because every
// wave landed with its declarations complete. Introducing one is a decision, not
// a shortcut, so it fails here until it is made deliberately.
func TestRepositoryCarriesNoArchitectureDebt(t *testing.T) {
	root := workspaceRoot(t)
	for _, name := range []string{"architecture.baseline.json", "architecture.waivers.json"} {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			t.Errorf("%s exists: this repository adopted ARC with zero findings and no debt record; if a wave genuinely needs one, record the owner, reason and removal condition and update this assertion", name)
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat %s: %v", name, err)
		}
	}
}

// TestEvidenceBelongsToTheProjectThatClaimsIt is the evidence-ownership ratchet.
//
// Every committed `domainAccess` row must name an import its own domain declares,
// in the same mode. The gate reports that too, but only for the workspace it
// resolves; this pins the exact rows so a row that appears without a runtime
// behind it fails a test rather than raising a coverage number.
func TestEvidenceBelongsToTheProjectThatClaimsIt(t *testing.T) {
	root := workspaceRoot(t)
	mapped := mappedProjects(t, root)
	rows := committedEvidence(t, root, mapped)

	if len(rows) == 0 {
		t.Fatal("no committed capability manifest carries a domainAccess row; the assertions below would pass vacuously")
	}

	got := make([]string, 0, len(rows))
	for _, row := range rows {
		got = append(got, row.Project+" "+row.Import+" "+row.Mode)
	}
	want := make([]string, 0, len(expectedEvidenceRows))
	for _, row := range expectedEvidenceRows {
		want = append(want, row.Project+" "+row.Import+" "+row.Mode)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("committed evidence rows =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}

	declared := declaredImports(t, root)
	for _, row := range rows {
		domain, ok := mapped[row.Project]
		if !ok {
			t.Errorf("%s emits evidence for %s but belongs to no domain", row.Project, row.Import)
			continue
		}
		if mode, found := declared[domain][row.Import]; !found {
			t.Errorf("%s emits evidence for %s, which domain %q does not declare", row.Project, row.Import, domain)
		} else if mode != row.Mode {
			t.Errorf("%s emits %s in mode %q; domain %q declares mode %q", row.Project, row.Import, row.Mode, domain, mode)
		}
	}
}

// TestEvidenceAdoptionIsAtomicPerDomain re-derives the rule the gate applies.
//
// `architecture.declared_without_evidence` fires only inside a domain that
// already emits. Once a domain implements one import, every ACTIVE import it
// declares is expected to be implemented too — partial adoption inside an
// opted-in domain is exactly what that check catches. A domain that emits
// nothing is honest and stays outside coverage, which is why eight of the ten
// domains here are absent from this assertion rather than failing it.
func TestEvidenceAdoptionIsAtomicPerDomain(t *testing.T) {
	root := workspaceRoot(t)
	mapped := mappedProjects(t, root)
	rows := committedEvidence(t, root, mapped)

	implemented := map[string]map[string]bool{}
	for _, row := range rows {
		domain := mapped[row.Project]
		if implemented[domain] == nil {
			implemented[domain] = map[string]bool{}
		}
		implemented[domain][row.Import] = true
	}
	if len(implemented) == 0 {
		t.Fatal("no domain emits evidence; this assertion would pass vacuously")
	}

	var missing []string
	for _, source := range repositoryManifests(t, root) {
		domain := source.Manifest.Domain
		if implemented[domain] == nil {
			continue
		}
		for _, imported := range source.Manifest.Imports {
			if imported.Status != archproto.StatusActive {
				continue
			}
			if !implemented[domain][imported.ID] {
				missing = append(missing, domain+" declares "+imported.ID+" and implements none of it")
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these domains emit evidence but leave an active import unimplemented:\n  %s\nevidence is atomic per domain: implement it in the same wave, or mark the import planned if enforcement does not exist yet",
			strings.Join(missing, "\n  "))
	}
}

// TestAdoptionMatrixPublishesEveryExclusion holds the document and the code
// together. An exclusion that lives only in a Go map is a decision nobody outside
// this file can read.
func TestAdoptionMatrixPublishesEveryExclusion(t *testing.T) {
	root := workspaceRoot(t)
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(adoptionMatrixPath))) //nolint:gosec // a committed document inside this repository
	if err != nil {
		t.Fatalf("read the adoption matrix: %v", err)
	}
	matrix := string(data)
	var undocumented []string
	for id := range excludedProjects {
		if !strings.Contains(matrix, id) {
			undocumented = append(undocumented, id)
		}
	}
	sort.Strings(undocumented)
	if len(undocumented) > 0 {
		t.Errorf("%s does not mention these excluded projects:\n  %s", adoptionMatrixPath, strings.Join(undocumented, "\n  "))
	}
	for _, domain := range declaredDomains {
		if !strings.Contains(matrix, domain) {
			t.Errorf("%s does not mention the %q domain", adoptionMatrixPath, domain)
		}
	}
}

// architectureReadmePath is the protocol README that summarizes this
// repository's adoption.
const architectureReadmePath = "protocols/architecture/README.md"

var (
	matrixProjectRow = regexp.MustCompile("(?m)^\\| `(/[^`]+)` \\|")
	matrixHeading    = regexp.MustCompile("(?m)^### `([a-z0-9-]+)` — (\\d+) projects?$")
	matrixDomainRow  = regexp.MustCompile("(?m)^\\| `([a-z0-9-]+)` \\| `[a-z0-9-]+` \\| (\\d+) \\|")
	readmeDomainRow  = regexp.MustCompile("(?m)^\\| `([a-z0-9-]+)` \\| [a-z0-9-]+ \\| (\\d+) \\|")
	adoptionTotals   = regexp.MustCompile(`maps \*\*(\d+) of its (\d+) projects`)
	excludedTotal    = regexp.MustCompile(`(?:other \*\*|excludes the other )(\d+)`)
)

// TestAdoptionMatrixPublishesEveryMembershipAndItsCounts holds the published
// numbers to the manifests. Every workspace project is one row of the matrix,
// and every count the matrix and the protocol README print — the totals, the
// excluded share and each domain's size — is the one the committed manifests
// give, so a project mapped or excluded without its row, or a stale count,
// fails here.
func TestAdoptionMatrixPublishesEveryMembershipAndItsCounts(t *testing.T) {
	root := workspaceRoot(t)
	discovered := discoverProjects(t, root)
	mapped := mappedProjects(t, root)
	perDomain := map[string]int{}
	for _, domain := range mapped {
		perDomain[domain]++
	}
	matrix, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(adoptionMatrixPath))) //nolint:gosec // a committed document inside this repository
	if err != nil {
		t.Fatalf("read the adoption matrix: %v", err)
	}
	readme, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(architectureReadmePath))) //nolint:gosec // a committed document inside this repository
	if err != nil {
		t.Fatalf("read the architecture README: %v", err)
	}

	rows := map[string]int{}
	for _, match := range matrixProjectRow.FindAllStringSubmatch(string(matrix), -1) {
		rows[match[1]]++
	}
	known := map[string]bool{}
	for _, id := range discovered {
		known[id] = true
		if rows[id] != 1 {
			t.Errorf("%s has %d rows in %s, want exactly one", id, rows[id], adoptionMatrixPath)
		}
	}
	for id := range rows {
		if !known[id] {
			t.Errorf("%s has a row in %s but is not a workspace project", id, adoptionMatrixPath)
		}
	}

	for path, document := range map[string]string{adoptionMatrixPath: string(matrix), architectureReadmePath: string(readme)} {
		totals := adoptionTotals.FindStringSubmatch(document)
		if totals == nil {
			t.Errorf("%s states no adoption totals", path)
			continue
		}
		if totals[1] != strconv.Itoa(len(mapped)) || totals[2] != strconv.Itoa(len(discovered)) {
			t.Errorf("%s says %s of %s projects are mapped; the manifests map %d of %d", path, totals[1], totals[2], len(mapped), len(discovered))
		}
		if excluded := excludedTotal.FindStringSubmatch(document); excluded == nil || excluded[1] != strconv.Itoa(len(excludedProjects)) {
			t.Errorf("%s states excluded total %v, want %d", path, excluded, len(excludedProjects))
		}
	}

	for _, count := range []struct {
		path    string
		pattern *regexp.Regexp
		text    string
	}{
		{adoptionMatrixPath + " heading", matrixHeading, string(matrix)},
		{adoptionMatrixPath + " domain table", matrixDomainRow, string(matrix)},
		{architectureReadmePath + " domain table", readmeDomainRow, string(readme)},
	} {
		seen := map[string]bool{}
		for _, match := range count.pattern.FindAllStringSubmatch(count.text, -1) {
			seen[match[1]] = true
			if want := strconv.Itoa(perDomain[match[1]]); match[2] != want {
				t.Errorf("%s gives domain %q %s projects; its manifest maps %s", count.path, match[1], match[2], want)
			}
		}
		for _, domain := range declaredDomains {
			if !seen[domain] {
				t.Errorf("%s gives no project count for domain %q", count.path, domain)
			}
		}
	}

	// Everything this comparison reads is a declared test input, so a new
	// project, a moved membership or an edited count re-runs it.
	read := []string{adoptionMatrixPath, architectureReadmePath}
	for _, source := range repositoryManifests(t, root) {
		read = append(read, source.Path)
	}
	for _, id := range discovered {
		for _, name := range []string{"putnami.json", "go.mod", "package.json", "schema/" + capabilityproto.ManifestFilename} {
			rel := strings.TrimPrefix(id, "/") + "/" + name
			if fileExists(filepath.Join(root, filepath.FromSlash(rel))) {
				read = append(read, rel)
			}
		}
	}
	projectRoot := filepath.Join(root, "tooling", "sdd-extension")
	patterns := declaredTestInputs(t, projectRoot)
	for _, rel := range read {
		fromProject, err := filepath.Rel(projectRoot, filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if !wsproto.SelectsPath(filepath.ToSlash(fromProject), patterns) {
			t.Errorf("%s is read by the adoption tests but is not a declared test input (options.test.filePatterns in tooling/sdd-extension/putnami.json)", rel)
		}
	}
}

// declaredTestInputs returns the test filePatterns the project at projectRoot
// declares in its putnami.json.
func declaredTestInputs(t *testing.T, projectRoot string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(projectRoot, "putnami.json")) //nolint:gosec // a committed project config inside this repository
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Options struct {
			Test struct {
				FilePatterns []string `json:"filePatterns"`
			} `json:"test"`
		} `json:"options"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("parse %s/putnami.json: %v", projectRoot, err)
	}
	return config.Options.Test.FilePatterns
}

// --- workspace reading ------------------------------------------------------

// skippedDirectories are the trees a project walk must not enter: dependency and
// build output, tool state, and the fixture projects that exist to be broken.
var skippedDirectories = map[string]bool{
	"node_modules": true,
	"vendor":       true,
	"testdata":     true,
	".gen":         true,
}

// discoverProjects returns every workspace project id, by the two ways a project
// enters this workspace: it carries its own project-schema putnami.json, or a
// language provider discovers it from a module manifest alone (the generated
// cross-language clients under the samples).
func discoverProjects(t *testing.T, root string) []string {
	t.Helper()
	var ids []string
	err := filepath.WalkDir(root, func(pathname string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if pathname != root && (skippedDirectories[name] || strings.HasPrefix(name, ".")) {
			return filepath.SkipDir
		}
		if pathname == root {
			return nil
		}
		relative, err := filepath.Rel(root, pathname)
		if err != nil {
			return nil
		}
		id := "/" + filepath.ToSlash(relative)
		if isProjectConfig(t, filepath.Join(pathname, "putnami.json")) {
			ids = append(ids, id)
			return nil
		}
		if fileExists(filepath.Join(pathname, "putnami.json")) {
			// A scope document groups projects; it is not one.
			return nil
		}
		if fileExists(filepath.Join(pathname, "go.mod")) || fileExists(filepath.Join(pathname, "package.json")) {
			ids = append(ids, id)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the workspace: %v", err)
	}
	sort.Strings(ids)
	return ids
}

// isProjectConfig separates a project from the scope documents that group them.
// Both are named putnami.json and only the $schema tells them apart.
func isProjectConfig(t *testing.T, pathname string) bool {
	t.Helper()
	data, err := os.ReadFile(pathname) //nolint:gosec // a workspace-relative project manifest inside this repository
	if err != nil {
		return false
	}
	var document struct {
		Schema string `json:"$schema"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse %s: %v", pathname, err)
	}
	return strings.HasSuffix(document.Schema, "/putnami-project.json")
}

func fileExists(pathname string) bool {
	info, err := os.Stat(pathname)
	return err == nil && !info.IsDir()
}

// repositoryManifests reads every committed domain manifest through the same
// strict reader the gate uses. Fixture manifests carry non-authority filenames
// and are not discovered, exactly as the CLI does not discover them.
func repositoryManifests(t *testing.T, root string) []archproto.ManifestSource {
	t.Helper()
	var sources []archproto.ManifestSource
	err := filepath.WalkDir(root, func(pathname string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			name := entry.Name()
			if pathname != root && (skippedDirectories[name] || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != archproto.ManifestFilename {
			return nil
		}
		data, err := os.ReadFile(pathname) //nolint:gosec // a workspace-relative domain manifest inside this repository
		if err != nil {
			t.Fatalf("read %s: %v", pathname, err)
		}
		manifest, diagnostics := archproto.ParseAndValidateManifest(data)
		if diag.HasErrors(diagnostics) || manifest == nil {
			t.Fatalf("%s does not parse through the strict reader: %v", pathname, diag.Errors(diagnostics))
		}
		relative, _ := filepath.Rel(root, pathname)
		sources = append(sources, archproto.ManifestSource{Path: filepath.ToSlash(relative), Manifest: manifest})
		return nil
	})
	if err != nil {
		t.Fatalf("walk the workspace: %v", err)
	}
	if len(sources) == 0 {
		t.Fatal("no domain manifest found; every assertion built on this would pass vacuously")
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
	return sources
}

// mappedProjects returns project id -> domain for every mapped project.
func mappedProjects(t *testing.T, root string) map[string]string {
	t.Helper()
	mapped := map[string]string{}
	for _, source := range repositoryManifests(t, root) {
		for _, project := range source.Manifest.Projects {
			mapped[project] = source.Manifest.Domain
		}
	}
	return mapped
}

// declaredImports returns domain -> import id -> mode for every declared import.
func declaredImports(t *testing.T, root string) map[string]map[string]string {
	t.Helper()
	declared := map[string]map[string]string{}
	for _, source := range repositoryManifests(t, root) {
		modes := map[string]string{}
		for _, imported := range source.Manifest.Imports {
			modes[imported.ID] = string(imported.Mode)
		}
		declared[source.Manifest.Domain] = modes
	}
	return declared
}

// committedEvidence reads the domainAccess rows out of every mapped project's
// committed capability manifest, attributing each row to the project its
// contribution identity names rather than to the manifest that contains it.
func committedEvidence(t *testing.T, root string, mapped map[string]string) []evidenceRow {
	t.Helper()
	nameToID := projectNames(t, root)
	var rows []evidenceRow
	ids := make([]string, 0, len(mapped))
	for id := range mapped {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		pathname := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(id, "/")), "schema", capabilityproto.ManifestFilename)
		data, err := os.ReadFile(pathname) //nolint:gosec // a committed capability manifest inside this repository
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", pathname, err)
		}
		document, diagnostics := capabilityproto.ParseAndValidateManifestDocument(data)
		if diag.HasErrors(diagnostics) || document == nil {
			t.Fatalf("%s does not parse: %v", pathname, diag.Errors(diagnostics))
		}
		if document.V2 == nil {
			continue
		}
		for _, row := range document.V2.DomainAccess {
			ownerID, known := nameToID[row.Identity.OwnerProject]
			if !known {
				t.Errorf("%s attributes a domainAccess row to %q, which is not a project in this workspace", pathname, row.Identity.OwnerProject)
				continue
			}
			rows = append(rows, evidenceRow{Project: ownerID, Import: row.Import, Mode: row.Mode})
		}
	}
	return rows
}

// projectNames maps a project's declared name onto its id. A capability row names
// its owner by name, and the gate resolves it the same way.
func projectNames(t *testing.T, root string) map[string]string {
	t.Helper()
	names := map[string]string{}
	for _, id := range discoverProjects(t, root) {
		config := readProjectConfig(t, filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(id, "/")), "putnami.json"))
		if config == nil || config.Name == "" {
			continue
		}
		names[config.Name] = id
	}
	return names
}
