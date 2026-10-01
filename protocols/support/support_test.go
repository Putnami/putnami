package support

import (
	"bytes"
	"reflect"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestStatusAndKindVocabulariesAreClosed(t *testing.T) {
	if ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1; changing it requires a migration story", ProtocolVersion)
	}
	wantStatuses := map[Status]bool{StatusStable: true, StatusPreview: true, StatusExperimental: true}
	if !reflect.DeepEqual(ValidStatuses, wantStatuses) {
		t.Fatalf("support statuses = %#v, want exactly %#v", ValidStatuses, wantStatuses)
	}
	wantKinds := map[SubjectKind]bool{SubjectKindProtocol: true, SubjectKindPackage: true, SubjectKindFeature: true}
	if !reflect.DeepEqual(ValidSubjectKinds, wantKinds) {
		t.Fatalf("subject kinds = %#v, want exactly %#v", ValidSubjectKinds, wantKinds)
	}
	for _, featureMaturity := range []Status{"modeled", "coded", "wired", "default-on", "live-verified", "design-partner-proven", "ga"} {
		if ValidStatuses[featureMaturity] {
			t.Errorf("feature maturity %q leaked into the support vocabulary", featureMaturity)
		}
	}
}

func TestStrictParserRejectsNonCanonicalJSON(t *testing.T) {
	tests := []struct {
		name string
		data string
		code string
	}{
		{"float version", `{"protocolVersion":1.0,"entries":[]}`, ErrorCodeInvalidProtocolVersion},
		{"missing version", `{"entries":[]}`, ErrorCodeInvalidProtocolVersion},
		{"unknown field", `{"protocolVersion":1,"entries":[],"other":true}`, ErrorCodeUnknownField},
		{"duplicate field", `{"protocolVersion":1,"entries":[],"entries":[]}`, ErrorCodeDuplicateField},
		{"nested duplicate field", `{"protocolVersion":1,"entries":[{"id":"x","id":"y","kind":"feature","status":"stable"}]}`, ErrorCodeDuplicateField},
		{"explicit null", `{"protocolVersion":1,"entries":null}`, ErrorCodeParseError},
		{"trailing value", `{"protocolVersion":1,"entries":[]} {}`, ErrorCodeParseError},
		{"not object", `[]`, ErrorCodeParseError},
		{"malformed", `{"protocolVersion":1`, ErrorCodeParseError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			catalog, diagnostics := ParseAndValidateCatalog([]byte(test.data))
			if catalog != nil {
				t.Fatalf("catalog = %#v, want nil", catalog)
			}
			if len(diagnostics) != 1 || diagnostics[0].Code != test.code {
				t.Fatalf("diagnostics = %#v, want one %s", diagnostics, test.code)
			}
		})
	}
}

func TestValidateCatalogReportsIndependentFindings(t *testing.T) {
	defaultOn := true
	catalog := &Catalog{ProtocolVersion: 2, Entries: []Entry{
		{ID: "Bad ID", Kind: "surface", Status: "beta", Default: &defaultOn, Parity: "partial"},
		{ID: "Bad ID", Kind: "surface", Status: "beta"},
	}}
	diagnostics := ValidateCatalog(catalog)
	wantCodes := map[string]bool{
		ErrorCodeInvalidProtocolVersion: true,
		ErrorCodeInvalidKind:            true,
		ErrorCodeInvalidID:              true,
		ErrorCodeInvalidStatus:          true,
		ErrorCodeInvalidParity:          true,
		ErrorCodeDuplicateEntry:         true,
	}
	for code := range wantCodes {
		found := false
		for _, finding := range diagnostics {
			if finding.Code == code {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing diagnostic %s in %#v", code, diagnostics)
		}
	}
	if !diag.HasErrors(diagnostics) {
		t.Fatal("invalid catalog did not report errors")
	}
}

func TestValidateCatalogRejectsNilAndEmpty(t *testing.T) {
	emptyCatalog := &Catalog{ProtocolVersion: ProtocolVersion}
	for _, test := range []struct {
		name    string
		catalog *Catalog
	}{
		{name: "nil"},
		{name: "empty", catalog: emptyCatalog},
	} {
		t.Run(test.name, func(t *testing.T) {
			if diagnostics := ValidateCatalog(test.catalog); !diag.HasErrors(diagnostics) {
				t.Fatalf("ValidateCatalog(%s) produced no errors", test.name)
			}
		})
	}
}

func TestValidateCatalogRejectsNonCanonicalSubjectIDs(t *testing.T) {
	for _, id := range []string{
		"go.putnami.dev/protocol/support/../cli",
		"go.putnami.dev/protocol/./cli",
		"go.putnami.dev/protocol//cli",
	} {
		t.Run(id, func(t *testing.T) {
			diagnostics := ValidateCatalog(&Catalog{ProtocolVersion: ProtocolVersion, Entries: []Entry{{
				ID: id, Kind: SubjectKindProtocol, Status: StatusStable,
			}}})
			found := false
			for _, diagnostic := range diagnostics {
				found = found || diagnostic.Code == ErrorCodeInvalidID
			}
			if !found {
				t.Fatalf("subject ID %q diagnostics = %#v, want %s", id, diagnostics, ErrorCodeInvalidID)
			}
		})
	}
}

func TestCanonicalCatalogSortsACopyAndRoundTrips(t *testing.T) {
	defaultOff := false
	input := &Catalog{Schema: CatalogSchemaURL, ProtocolVersion: ProtocolVersion, Entries: []Entry{
		{ID: "zeta", Kind: SubjectKindProtocol, Status: StatusStable},
		{ID: "@putnami/python", Kind: SubjectKindPackage, Status: StatusExperimental, Default: &defaultOff, Parity: ParityUnsupported},
		{ID: "alpha", Kind: SubjectKindFeature, Status: StatusPreview},
	}}
	original := append([]Entry(nil), input.Entries...)
	first, err := MarshalCatalog(input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(input.Entries, original) {
		t.Fatal("MarshalCatalog mutated caller entry order")
	}
	parsed, diagnostics := ParseAndValidateCatalog(first)
	if parsed == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("canonical bytes failed round trip: %v", diagnostics)
	}
	second, err := MarshalCatalog(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("canonical marshal is not idempotent\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if CanonicalCatalog(nil) != nil {
		t.Fatal("CanonicalCatalog(nil) must stay nil")
	}
}

func TestExperimentalCannotBeDefault(t *testing.T) {
	defaultOn := true
	diagnostics := ValidateCatalog(&Catalog{ProtocolVersion: 1, Entries: []Entry{{
		ID: "@putnami/python", Kind: SubjectKindPackage, Status: StatusExperimental, Default: &defaultOn,
	}}})
	if len(diagnostics) != 1 || diagnostics[0].Code != ErrorCodeInvalidDefault {
		t.Fatalf("diagnostics = %#v, want one %s", diagnostics, ErrorCodeInvalidDefault)
	}
}
