package support

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// workspaceRoot returns the checked-in workspace root, or skips. The gates in
// this file read sibling projects, which only exist when the module is tested
// inside the workspace; a standalone module test proves the parser instead.
func workspaceRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(root, "putnami.workspace.json")); err != nil {
		if os.IsNotExist(err) {
			t.Skip("workspace root sentinel is absent; module is being tested standalone")
		}
		t.Fatalf("inspect workspace root sentinel: %v", err)
	}
	return root
}

func TestConformanceFixtures(t *testing.T) {
	forEachFixture(t, "fixtures/valid/*.json", func(t *testing.T, path string, data []byte) {
		catalog, diagnostics := ParseAndValidateCatalog(data)
		if catalog == nil || diag.HasErrors(diagnostics) {
			t.Fatalf("valid fixture %s failed: %v", path, diagnostics)
		}
		canonical, err := MarshalCatalog(catalog)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, canonical) {
			t.Fatalf("valid fixture %s is not canonical\n--- fixture ---\n%s\n--- canonical ---\n%s", path, data, canonical)
		}
	})
	forEachFixture(t, "fixtures/invalid/*.json", func(t *testing.T, path string, data []byte) {
		_, diagnostics := ParseAndValidateCatalog(data)
		if !diag.HasErrors(diagnostics) {
			t.Fatalf("invalid fixture %s produced no error", path)
		}
		for _, finding := range diag.Errors(diagnostics) {
			if !ValidDiagnosticCodes[finding.Code] {
				t.Errorf("invalid fixture %s produced unknown code %q", path, finding.Code)
			}
		}
	})
}

// reviewedPackageDecisions are the four package classifications reviewed as
// product policy. They are pinned exactly: the catalog may gain other subjects,
// but these four decisions may not drift silently.
var reviewedPackageDecisions = map[string]Status{
	"@putnami/cli":        StatusStable,
	"@putnami/go":         StatusStable,
	"@putnami/python":     StatusExperimental,
	"@putnami/typescript": StatusStable,
}

// productDiscoveryProtocolDecisions are the protocol classifications recorded
// by doc/adr/0001-support-entry-ownership-and-evidence.md. Pinning them here
// makes a promotion a deliberate edit in both the reviewed catalog and the
// module gate, never a one-line status change nobody had to justify.
var productDiscoveryProtocolDecisions = map[string]Status{
	"go.putnami.dev/protocol/agentcontext": StatusPreview,
	"go.putnami.dev/protocol/capabilities": StatusPreview,
	"go.putnami.dev/protocol/contracts":    StatusPreview,
	"go.putnami.dev/protocol/features":     StatusPreview,
	"go.putnami.dev/protocol/support":      StatusPreview,
}

// subjectIdentity is the (kind, id) pair that names one classified subject.
// The pin below is keyed by identity rather than by Entry on purpose: an Entry
// also carries the status and the two optional claims, so copying one out of
// the catalog would silently key the comparison on fields the pin does not own.
type subjectIdentity struct {
	Kind SubjectKind
	ID   string
}

// reviewedClaims are the optional independent claims reviewed alongside a
// stable promise. Absence is meaningful in both directions: a nil Default and
// an empty Parity mean the reviewed entry makes no claim on that axis, and the
// gate fails if the catalog starts making one.
type reviewedClaims struct {
	Default *bool
	Parity  ParityStatus
}

// reviewedStableSubjects is every subject the repository currently promises to
// support, with the claims reviewed alongside each promise. `stable` is the
// only status that makes a promise an outside consumer can rely on, so it is
// the only one pinned exhaustively: promoting a subject, dropping a promise, or
// changing what a promise claims fails here until the same pull request updates
// this map, which is the review that
// doc/adr/0002-promotion-and-demotion-criteria.md requires. Preview and
// experimental stay unpinned on purpose — a subject that is still moving must
// be able to move.
var reviewedStableSubjects = map[subjectIdentity]reviewedClaims{
	// Windows consumers on windows/amd64. The evidence is in the
	// owning project's "Install on Windows" support section.
	{SubjectKindFeature, "cli/windows-consumers"}:                {},
	{SubjectKindPackage, "@putnami/application"}:                 {},
	{SubjectKindPackage, "@putnami/cli"}:                         {},
	{SubjectKindPackage, "@putnami/cli-protocol"}:                {},
	{SubjectKindPackage, "@putnami/client"}:                      {},
	{SubjectKindPackage, "@putnami/database"}:                    {},
	{SubjectKindPackage, "@putnami/document"}:                    {},
	{SubjectKindPackage, "@putnami/events"}:                      {},
	{SubjectKindPackage, "@putnami/go"}:                          {},
	{SubjectKindPackage, "@putnami/migration"}:                   {},
	{SubjectKindPackage, "@putnami/runtime"}:                     {},
	{SubjectKindPackage, "@putnami/scaffold"}:                    {},
	{SubjectKindPackage, "@putnami/storage"}:                     {},
	{SubjectKindPackage, "@putnami/typescript"}:                  {},
	{SubjectKindPackage, "@putnami/ui"}:                          {},
	{SubjectKindPackage, "@putnami/utils"}:                       {},
	{SubjectKindPackage, "@putnami/web"}:                         {},
	{SubjectKindPackage, "go-library"}:                           {},
	{SubjectKindPackage, "go-server"}:                            {},
	{SubjectKindPackage, "go.putnami.dev/api"}:                   {},
	{SubjectKindPackage, "go.putnami.dev/app"}:                   {},
	{SubjectKindPackage, "go.putnami.dev/cache"}:                 {},
	{SubjectKindPackage, "go.putnami.dev/client"}:                {},
	{SubjectKindPackage, "go.putnami.dev/config"}:                {},
	{SubjectKindPackage, "go.putnami.dev/ctxutil"}:               {},
	{SubjectKindPackage, "go.putnami.dev/database"}:              {},
	{SubjectKindPackage, "go.putnami.dev/errors"}:                {},
	{SubjectKindPackage, "go.putnami.dev/events"}:                {},
	{SubjectKindPackage, "go.putnami.dev/grpc"}:                  {},
	{SubjectKindPackage, "go.putnami.dev/http"}:                  {},
	{SubjectKindPackage, "go.putnami.dev/inject"}:                {},
	{SubjectKindPackage, "go.putnami.dev/keyringstore"}:          {},
	{SubjectKindPackage, "go.putnami.dev/logger"}:                {},
	{SubjectKindPackage, "go.putnami.dev/migratecli"}:            {},
	{SubjectKindPackage, "go.putnami.dev/migration"}:             {},
	{SubjectKindPackage, "go.putnami.dev/openapi"}:               {},
	{SubjectKindPackage, "go.putnami.dev/parallel"}:              {},
	{SubjectKindPackage, "go.putnami.dev/platform"}:              {},
	{SubjectKindPackage, "go.putnami.dev/proto"}:                 {},
	{SubjectKindPackage, "go.putnami.dev/schema"}:                {},
	{SubjectKindPackage, "go.putnami.dev/security"}:              {},
	{SubjectKindPackage, "go.putnami.dev/storage"}:               {},
	{SubjectKindPackage, "go.putnami.dev/telemetry"}:             {},
	{SubjectKindPackage, "putnami-extension-sdk"}:                {},
	{SubjectKindPackage, "typescript-library"}:                   {},
	{SubjectKindPackage, "typescript-server"}:                    {},
	{SubjectKindPackage, "typescript-web"}:                       {},
	{SubjectKindProtocol, "go.putnami.dev/protocol/cache"}:       {},
	{SubjectKindProtocol, "go.putnami.dev/protocol/cli"}:         {},
	{SubjectKindProtocol, "go.putnami.dev/protocol/database"}:    {},
	{SubjectKindProtocol, "go.putnami.dev/protocol/doctor"}:      {},
	{SubjectKindProtocol, "go.putnami.dev/protocol/extension"}:   {},
	{SubjectKindProtocol, "go.putnami.dev/protocol/http-routes"}: {},
	{SubjectKindProtocol, "go.putnami.dev/protocol/infra"}:       {},
	{SubjectKindProtocol, "go.putnami.dev/protocol/job"}:         {},
	{SubjectKindProtocol, "go.putnami.dev/protocol/migration"}:   {},
	{SubjectKindProtocol, "go.putnami.dev/protocol/platform"}:    {},
	{SubjectKindProtocol, "go.putnami.dev/protocol/runtime"}:     {},
	{SubjectKindProtocol, "go.putnami.dev/protocol/telemetry"}:   {},
	{SubjectKindProtocol, "go.putnami.dev/protocol/transaction"}: {},
	{SubjectKindProtocol, "go.putnami.dev/protocol/workspace"}:   {},
}

func TestRootCatalogIsTheCanonicalAuthority(t *testing.T) {
	root := workspaceRoot(t)
	committed, catalog := readRootCatalog(t, root)
	canonical, err := MarshalCatalog(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, canonical) {
		t.Fatalf("root support catalog is not canonical\n--- committed ---\n%s\n--- canonical ---\n%s", committed, canonical)
	}
	validateRootCatalogDecisions(t, root, catalog)
}

// readRootCatalog returns the committed bytes and the parsed catalog, failing
// on anything the strict reader rejects.
func readRootCatalog(t *testing.T, root string) ([]byte, *Catalog) {
	t.Helper()
	committed, err := os.ReadFile(filepath.Join(root, CatalogFilename))
	if err != nil {
		t.Fatal(err)
	}
	catalog, diagnostics := ParseAndValidateCatalog(committed)
	if catalog == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("root support catalog failed strict validation: %v", diagnostics)
	}
	return committed, catalog
}

func validateRootCatalogDecisions(t *testing.T, workspaceRoot string, catalog *Catalog) {
	t.Helper()
	seenReviewedPackages := make(map[string]bool, len(reviewedPackageDecisions))
	for _, entry := range catalog.Entries {
		switch entry.Kind {
		case SubjectKindPackage:
			wantStatus, ok := reviewedPackageDecisions[entry.ID]
			if !ok {
				continue
			}
			seenReviewedPackages[entry.ID] = true
			if entry.Status != wantStatus {
				t.Fatalf("root reviewed package decision drifted: %#v", entry)
			}
			if entry.ID == "@putnami/python" {
				if entry.Default == nil || *entry.Default || entry.Parity != ParityUnsupported {
					t.Fatalf("root Python decision drifted: %#v", entry)
				}
			} else if entry.Default != nil || entry.Parity != "" {
				t.Fatalf("root stable-package decision makes an unreviewed default or parity claim: %#v", entry)
			}
		case SubjectKindProtocol:
			// ADR 0001: a protocol entry id is the owning module's Go module
			// path, so exactly one in-repo project owns every classification.
			requireProtocolModule(t, workspaceRoot, entry.ID)
		default:
			// Feature ids name product behavior rather than a workspace path,
			// so the closed vocabularies validated above are the whole check.
		}
	}
	for id := range reviewedPackageDecisions {
		if !seenReviewedPackages[id] {
			t.Fatalf("root authority lost the reviewed package classification %q", id)
		}
	}
	// The package-only unit test above intentionally supplies a temporary
	// workspace root. It exercises the extensibility rule without pretending to
	// be the checked-in catalog, so only the real workspace gate applies the
	// product-discovery protocol decisions below.
	if _, err := os.Stat(filepath.Join(workspaceRoot, "putnami.workspace.json")); os.IsNotExist(err) {
		return
	}
	statuses := make(map[string]Status, len(catalog.Entries))
	for _, entry := range catalog.Entries {
		if entry.Kind == SubjectKindProtocol {
			statuses[entry.ID] = entry.Status
		}
	}
	for id, want := range productDiscoveryProtocolDecisions {
		got, ok := statuses[id]
		if !ok {
			t.Fatalf("root authority lost the reviewed classification of protocol %q", id)
		}
		if got != want {
			t.Fatalf("protocol %q is classified %q, but the reviewed decision is %q; promotion needs new evidence in doc/adr/", id, got, want)
		}
	}
	requireReviewedStableSet(t, catalog)
}

// requireReviewedStableSet compares the promises the catalog makes against the
// promises that were reviewed. A public support commitment is the one decision
// that must never arrive as a side effect of an unrelated edit, so every
// direction fails: an unreviewed promotion, a silently withdrawn promise, and a
// promise whose default or parity claim changed underneath it.
func requireReviewedStableSet(t *testing.T, catalog *Catalog) {
	t.Helper()
	const criteria = "doc/adr/0002-promotion-and-demotion-criteria.md"
	promised := make(map[subjectIdentity]bool, len(reviewedStableSubjects))
	for _, entry := range catalog.Entries {
		if entry.Status != StatusStable {
			continue
		}
		subject := subjectIdentity{Kind: entry.Kind, ID: entry.ID}
		promised[subject] = true
		claims, reviewed := reviewedStableSubjects[subject]
		if !reviewed {
			t.Errorf("%s %q is classified stable but is not in the reviewed stable set; a promotion needs the criteria in %s and an entry here",
				subject.Kind, subject.ID, criteria)
			continue
		}
		if drift := claims.drift(entry); drift != "" {
			t.Errorf("stable %s %q %s; the claims reviewed with a promise are pinned in the same map, per %s",
				subject.Kind, subject.ID, drift, criteria)
		}
	}
	for subject := range reviewedStableSubjects {
		if !promised[subject] {
			t.Errorf("%s %q was reviewed as stable but the catalog no longer promises it; a demotion needs the criteria in %s and removal here",
				subject.Kind, subject.ID, criteria)
		}
	}
}

// drift reports how an entry's optional claims differ from the reviewed ones,
// or an empty string when they agree. Both axes distinguish "no claim" from a
// claim, because the wire does: omitting a field says nothing rather than false.
func (claims reviewedClaims) drift(entry Entry) string {
	switch {
	case claims.Default == nil && entry.Default != nil:
		return fmt.Sprintf("makes an unreviewed default claim (default: %t)", *entry.Default)
	case claims.Default != nil && entry.Default == nil:
		return fmt.Sprintf("dropped its reviewed default claim (default: %t)", *claims.Default)
	case claims.Default != nil && *claims.Default != *entry.Default:
		return fmt.Sprintf("changed its default claim to %t, reviewed as %t", *entry.Default, *claims.Default)
	case claims.Parity != entry.Parity:
		return fmt.Sprintf("claims parity %q, reviewed as %q", entry.Parity, claims.Parity)
	default:
		return ""
	}
}

// TestClassifiedProtocolsDeclareTheirReviewedStatus keeps the catalog and the
// modules it classifies from drifting apart. ADR 0001 put evidence in the owning
// module's documentation rather than on the wire; that only stays trustworthy
// while the module repeats the status the catalog actually records.
func TestClassifiedProtocolsDeclareTheirReviewedStatus(t *testing.T) {
	root := workspaceRoot(t)
	_, catalog := readRootCatalog(t, root)
	checked := 0
	for _, entry := range catalog.Entries {
		if entry.Kind != SubjectKindProtocol {
			continue
		}
		name, ok := protocolModuleName(entry.ID)
		if !ok {
			continue
		}
		readmePath := filepath.Join(root, "protocols", name, "README.md")
		readme, err := os.ReadFile(readmePath)
		if err != nil {
			t.Errorf("protocol %q documents no status: %v", entry.ID, err)
			continue
		}
		checked++
		if reason := readStatusDeclaration(string(readme)).explain(entry.Status); reason != "" {
			t.Errorf("%s %s", readmePath, reason)
		}
	}
	if checked == 0 {
		t.Fatal("the root catalog classified no protocol module, so this gate proved nothing")
	}
}

func TestRootCatalogDecisionsAllowAdditionalPackageClassification(t *testing.T) {
	falseValue := false
	catalog := &Catalog{Entries: []Entry{
		{ID: "@putnami/cli", Kind: SubjectKindPackage, Status: StatusStable},
		{ID: "@putnami/go", Kind: SubjectKindPackage, Status: StatusStable},
		{ID: "@putnami/python", Kind: SubjectKindPackage, Status: StatusExperimental, Default: &falseValue, Parity: ParityUnsupported},
		{ID: "@putnami/typescript", Kind: SubjectKindPackage, Status: StatusStable},
		{ID: "@putnami/new-package", Kind: SubjectKindPackage, Status: StatusPreview},
	}}

	validateRootCatalogDecisions(t, t.TempDir(), catalog)
}

// TestReviewedClaimsDriftCoversBothDirectionsOnBothAxes pins the comparison
// itself, not just its verdict against the checked-in catalog. No stable subject
// claims `default` or `parity` today, so the real catalog exercises exactly one
// of these branches; a promise that starts or stops making a claim is precisely
// the change that must not slip through.
func TestReviewedClaimsDriftCoversBothDirectionsOnBothAxes(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name    string
		claims  reviewedClaims
		entry   Entry
		wantHit string
	}{
		{name: "no claim on either side", claims: reviewedClaims{}, entry: Entry{}},
		{name: "reviewed claims are repeated", claims: reviewedClaims{Default: &no, Parity: ParityUnsupported},
			entry: Entry{Default: &no, Parity: ParityUnsupported}},
		{name: "an unreviewed default appears", claims: reviewedClaims{}, entry: Entry{Default: &yes},
			wantHit: "unreviewed default claim (default: true)"},
		{name: "a reviewed default disappears", claims: reviewedClaims{Default: &no}, entry: Entry{},
			wantHit: "dropped its reviewed default claim (default: false)"},
		{name: "a reviewed default flips", claims: reviewedClaims{Default: &no}, entry: Entry{Default: &yes},
			wantHit: "changed its default claim to true, reviewed as false"},
		{name: "an unreviewed parity appears", claims: reviewedClaims{}, entry: Entry{Parity: ParityUnsupported},
			wantHit: `claims parity "unsupported", reviewed as ""`},
		{name: "a reviewed parity disappears", claims: reviewedClaims{Parity: ParityUnsupported}, entry: Entry{},
			wantHit: `claims parity "", reviewed as "unsupported"`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			drift := test.claims.drift(test.entry)
			if test.wantHit == "" {
				if drift != "" {
					t.Fatalf("drift = %q, want no drift", drift)
				}
				return
			}
			if !strings.Contains(drift, test.wantHit) {
				t.Fatalf("drift = %q, want it to contain %q", drift, test.wantHit)
			}
		})
	}
}

// requireProtocolModule resolves a protocol entry id to the module that owns
// the contract. The id is the Go module path, so the owning project is derived,
// never declared twice.
func requireProtocolModule(t *testing.T, workspaceRoot, id string) {
	t.Helper()
	const prefix = "go.putnami.dev/protocol/"
	name, ok := protocolModuleName(id)
	if !ok {
		t.Fatalf("protocol entry %q is not a %s<name> module path", id, prefix)
	}
	modulePath := filepath.Join(workspaceRoot, "protocols", name, "go.mod")
	data, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatalf("protocol entry %q names no module in protocols/: %v", id, err)
	}
	want := "module " + id
	for line := range strings.Lines(string(data)) {
		if strings.TrimSpace(line) == want {
			return
		}
	}
	t.Fatalf("%s does not declare %q", modulePath, want)
}

func protocolModuleName(id string) (string, bool) {
	const modulePrefix = "go.putnami.dev/protocol/"
	name, ok := strings.CutPrefix(id, modulePrefix)
	return name, ok && name != "" && name != "." && name != ".." && !path.IsAbs(name) && path.Clean(name) == name && !strings.Contains(name, "/")
}

func TestProtocolModuleNameRejectsPathAliases(t *testing.T) {
	for _, id := range []string{
		"go.putnami.dev/protocol/.",
		"go.putnami.dev/protocol/..",
		"go.putnami.dev/protocol/sitecontent/../support",
	} {
		if name, ok := protocolModuleName(id); ok {
			t.Errorf("protocolModuleName(%q) = %q, true; want rejection", id, name)
		}
	}
	if name, ok := protocolModuleName("go.putnami.dev/protocol/sitecontent"); !ok || name != "sitecontent" {
		t.Fatalf("valid protocol module = %q, %v; want sitecontent, true", name, ok)
	}
}

func forEachFixture(t *testing.T, pattern string, run func(*testing.T, string, []byte)) {
	t.Helper()
	paths, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no fixtures matched %s", pattern)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			run(t, path, data)
		})
	}
}
