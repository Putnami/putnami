package analytics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestConformance_ProtocolVersion pins the current protocol version so any bump
// is intentional and surfaces in code review. A bump is a breaking change for
// every deployed browser tracker, which keeps sending the old number.
func TestConformance_ProtocolVersion(t *testing.T) {
	if ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1 — bumping requires a migration story", ProtocolVersion)
	}
}

// TestConformance_ErrorCodes validates that every protocol error code uses the
// analytics.* prefix and that the canonical set is complete. The TypeScript
// sanitizer counts drop reasons by these strings minus the prefix, so a rename
// or an addition that lands here alone stops a deployed consumer from counting.
func TestConformance_ErrorCodes(t *testing.T) {
	canonical := []string{
		"analytics.parse_error",
		"analytics.invalid_version",
		"analytics.batch_too_large",
		"analytics.invalid_timestamp",
		"analytics.invalid_event_id",
		"analytics.unknown_event",
		"analytics.unknown_attribute",
		"analytics.missing_attribute",
		"analytics.attribute_kind",
		"analytics.invalid_value",
		"analytics.invalid_path",
		"analytics.invalid_route",
		"analytics.invalid_referrer",
		"analytics.invalid_utm",
		"analytics.props_too_many",
		"analytics.invalid_prop_key",
		"analytics.invalid_prop_value",
		"analytics.invalid_action",
	}
	for _, code := range canonical {
		if !ValidErrorCodes[code] {
			t.Errorf("canonical error code %q missing from ValidErrorCodes", code)
		}
		if !strings.HasPrefix(code, "analytics.") {
			t.Errorf("error code %q must use analytics.* prefix", code)
		}
	}
	if len(ValidErrorCodes) != len(canonical) {
		t.Errorf("ValidErrorCodes has %d entries, want %d canonical codes", len(ValidErrorCodes), len(canonical))
	}
}

// TestConformance_ClosedEnums pins the closed vocabularies and their predicates
// together. Each enum is a database column vocabulary and a counter dimension on
// the receiving side, so adding a member is a protocol change; each predicate is
// the only way a consumer is meant to test membership.
func TestConformance_ClosedEnums(t *testing.T) {
	enums := []struct {
		name      string
		members   []string
		predicate func(string) bool
	}{
		{"ViewportClasses", ViewportClasses, IsViewportClass},
		{"ReferrerTypes", ReferrerTypes, IsReferrerType},
		{"DeviceTypes", DeviceTypes, IsDeviceType},
		{"Browsers", Browsers, IsBrowser},
		{"OperatingSystems", OperatingSystems, IsOS},
		{"Sources", Sources, IsSource},
		{"VisitorKinds", VisitorKinds, IsVisitorKind},
		{"Outcomes", Outcomes, IsOutcome},
		{"UTMKeys", UTMKeys, IsUTMKey},
		{"Dimensions", Dimensions, IsDimension},
	}
	want := map[string]int{
		"ViewportClasses": 5, "ReferrerTypes": 5, "DeviceTypes": 4, "Browsers": 7,
		"OperatingSystems": 7, "Sources": 2, "VisitorKinds": 2, "Outcomes": 3,
		"UTMKeys": 5, "Dimensions": 15,
	}
	for _, enum := range enums {
		t.Run(enum.name, func(t *testing.T) {
			if len(enum.members) != want[enum.name] {
				t.Errorf("%s has %d members, want %d", enum.name, len(enum.members), want[enum.name])
			}
			seen := make(map[string]bool, len(enum.members))
			for _, member := range enum.members {
				if seen[member] {
					t.Errorf("%s repeats %q", enum.name, member)
				}
				seen[member] = true
				if !enum.predicate(member) {
					t.Errorf("%s predicate rejects its own member %q", enum.name, member)
				}
			}
			if enum.predicate("definitely-not-a-member") {
				t.Errorf("%s predicate accepts a value outside the enum", enum.name)
			}
			if enum.predicate("") {
				t.Errorf("%s predicate accepts the empty string", enum.name)
			}
		})
	}
}

// TestConformance_EventNames pins the wire vocabulary against the server-only
// event. form_submit is exported so consumers share one name for it, and is
// rejected on the wire so a visitor cannot forge a server-recorded outcome.
func TestConformance_EventNames(t *testing.T) {
	if !IsEventName(EventPageView) || !IsEventName(EventAction) {
		t.Error("IsEventName must accept the two wire event names")
	}
	if IsEventName(EventFormSubmit) {
		t.Errorf("%s is recorded server-side and must never be accepted on the wire", EventFormSubmit)
	}
}

// TestConformance_Bounds pins the numeric contract the TypeScript sanitizer
// mirrors. A silent widening here would let a batch through that the other
// runtime rejects, which is the exact asymmetry the shared corpus exists to
// prevent.
func TestConformance_Bounds(t *testing.T) {
	bounds := map[string][2]int{
		"MaxEvents":        {MaxEvents, 50},
		"MaxBodyBytes":     {MaxBodyBytes, 65536},
		"MaxSeq":           {MaxSeq, 1_000_000},
		"MaxEngagementMs":  {MaxEngagementMs, 86_400_000},
		"MaxPathLen":       {MaxPathLen, 512},
		"MaxRouteLen":      {MaxRouteLen, 256},
		"MaxReferrerLen":   {MaxReferrerLen, 512},
		"MaxUTMLen":        {MaxUTMLen, 128},
		"MaxActionNameLen": {MaxActionNameLen, 64},
		"MaxPropKeys":      {MaxPropKeys, 20},
		"MaxPropKeyLen":    {MaxPropKeyLen, 32},
		"MaxPropStringLen": {MaxPropStringLen, 256},
	}
	for name, pair := range bounds {
		if pair[0] != pair[1] {
			t.Errorf("%s = %d, want %d", name, pair[0], pair[1])
		}
	}
}

// TestConformance_Fixtures runs every batch fixture: those in valid/ must
// produce no errors, those in invalid/ must produce at least one. The
// TypeScript sanitizer runs the same corpus, which is what makes it a parity
// proof rather than two independent test suites.
func TestConformance_Fixtures(t *testing.T) {
	runFixtureDir(t, "valid", false)
	runFixtureDir(t, "invalid", true)
}

func runFixtureDir(t *testing.T, kind string, wantErrors bool) {
	t.Helper()
	for _, path := range fixturePaths(t, kind) {
		t.Run(kind+"/"+filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, diags := ParseAndValidateBatch(data)
			hasErr := diag.HasErrors(diags)
			if wantErrors && !hasErr {
				t.Errorf("invalid fixture %s should produce errors but none found", path)
			}
			if !wantErrors && hasErr {
				t.Errorf("valid fixture %s produced errors: %v", path, diags)
			}
		})
	}
}

// fixturePaths returns the batch fixtures of one kind, failing when the glob
// matches nothing: an empty corpus would make every conformance test vacuous.
func fixturePaths(t *testing.T, kind string) []string {
	t.Helper()
	glob := filepath.Join("fixtures", "batch", kind, "*.json")
	paths, err := filepath.Glob(glob)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no fixtures matched %s", glob)
	}
	return paths
}
