package migration

import (
	"encoding/json"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

const (
	testUpHash   = "1111111111111111111111111111111111111111111111111111111111111111"
	testDownHash = "2222222222222222222222222222222222222222222222222222222222222222"
	testUpHash2  = "3333333333333333333333333333333333333333333333333333333333333333"
)

func validBundle() Bundle {
	return Bundle{
		Protocol: BundleProtocol,
		AppName:  "wealth",
		Source:   "go/samples/wealth",
		Version:  "1.4.0",
		Operations: []BundleOperation{
			{
				Kind:      KindSQL,
				Target:    "default",
				Namespace: "iam",
				Name:      "iam/20260604120000_add_users",
				Up:        PayloadRef{Path: "payload/sql/default/iam/20260604120000_add_users.up.sql", Hash: testUpHash},
				Down:      &PayloadRef{Path: "payload/sql/default/iam/20260604120000_add_users.down.sql", Hash: testDownHash},
				Safety:    SafetySafeOnline,
			},
		},
	}
}

func TestValidateBundle_Valid(t *testing.T) {
	b := validBundle()
	diags := ValidateBundle(&b)
	if diag.HasErrors(diags) {
		t.Fatalf("expected valid bundle, got diagnostics: %v", diags)
	}
}

func TestNormalizeBundle_Defaults(t *testing.T) {
	b := Bundle{
		AppName: "svc",
		Operations: []BundleOperation{
			{Kind: KindSQL, Name: "default/0001_init", Up: PayloadRef{Path: "payload/sql/default/default/0001_init.up.sql", Hash: testUpHash}},
		},
	}
	n := NormalizeBundle(b)

	if n.Protocol != BundleProtocol {
		t.Errorf("protocol default: got %q, want %q", n.Protocol, BundleProtocol)
	}
	op := n.Operations[0]
	if op.Target != DefaultDatasource {
		t.Errorf("target default: got %q, want %q", op.Target, DefaultDatasource)
	}
	if op.OrderKey != op.Name {
		t.Errorf("orderKey default: got %q, want %q", op.OrderKey, op.Name)
	}
	if op.Safety != SafetySafeOnline {
		t.Errorf("safety default: got %q, want %q", op.Safety, SafetySafeOnline)
	}
	if op.Capabilities.Reversible {
		t.Error("operation without down payload must not be reversible")
	}
}

func TestNormalizeOperation_DownImpliesReversible(t *testing.T) {
	op := NormalizeOperation(BundleOperation{
		Kind: KindSQL,
		Name: "x",
		Up:   PayloadRef{Path: "a.up.sql", Hash: testUpHash},
		Down: &PayloadRef{Path: "a.down.sql", Hash: testDownHash},
	})
	if !op.Capabilities.Reversible {
		t.Error("operation with down payload must be marked reversible")
	}
}

// TestComputeBundleDigest_CompatibleChangesDigest pins that the compatibility
// marker is migration-meaningful, not provenance: it travels in the operation,
// so two bundles that disagree about whether the previous image can still run
// address differently. The control is the absent field — `omitempty` keeps it
// out of the canonical form, which is what lets every bundle published before
// the marker existed keep its digest (TestCrossLanguage_BundleDigest).
func TestComputeBundleDigest_CompatibleChangesDigest(t *testing.T) {
	absent := validBundle()
	compatible := validBundle()
	compatible.Operations[0].Capabilities.Compatible = true

	if ComputeBundleDigest(absent) == ComputeBundleDigest(compatible) {
		t.Fatal("digest must change when an operation declares itself compatible")
	}

	explicitFalse := validBundle()
	explicitFalse.Operations[0].Capabilities.Compatible = false
	if ComputeBundleDigest(absent) != ComputeBundleDigest(explicitFalse) {
		t.Fatal("an absent marker and an explicit false must digest identically")
	}
}

// TestRollbackAllowed pins the workload rollback rule (D34): migrations are
// forward-only, so an environment may move back across an operation only when a
// Down payload exists or the operation was written expand/contract. One
// operation that says neither refuses the whole set, because a
// partially-rolled-back schema is the state nobody can reason about.
func TestRollbackAllowed(t *testing.T) {
	reversible := Capabilities{Reversible: true}
	compatible := Capabilities{Compatible: true}
	both := Capabilities{Reversible: true, Compatible: true}
	neither := Capabilities{Transactional: true}

	for name, tc := range map[string]struct {
		caps []Capabilities
		want bool
	}{
		"empty set":              {nil, true},
		"all reversible":         {[]Capabilities{reversible, reversible}, true},
		"all compatible":         {[]Capabilities{compatible, compatible}, true},
		"mixed":                  {[]Capabilities{reversible, compatible, both}, true},
		"one says neither":       {[]Capabilities{reversible, neither, compatible}, false},
		"only one, says neither": {[]Capabilities{neither}, false},
	} {
		t.Run(name, func(t *testing.T) {
			ops := make([]BundleOperation, len(tc.caps))
			for i, caps := range tc.caps {
				ops[i] = BundleOperation{Kind: KindSQL, Name: "op", Capabilities: caps}
			}
			if got := RollbackAllowed(ops); got != tc.want {
				t.Fatalf("RollbackAllowed = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestValidateBundle_CompatibleAndReversibleCoexist pins that the two markers
// are independent claims about the same operation: an expand/contract migration
// that also ships a Down payload is the safest kind there is, and validation
// must not treat saying both as a contradiction.
func TestValidateBundle_CompatibleAndReversibleCoexist(t *testing.T) {
	b := validBundle()
	b.Operations[0].Capabilities.Compatible = true
	b.Operations[0].Capabilities.Reversible = true
	if diags := ValidateBundle(&b); diag.HasErrors(diags) {
		t.Fatalf("an operation that is both reversible and compatible was rejected: %v", diags)
	}

	// The pre-existing rule still holds around the new one: a reversible claim
	// with no down payload stays an error, compatible or not.
	b.Operations[0].Down = nil
	if diags := ValidateBundle(&b); !diag.HasErrors(diags) {
		t.Fatal("compatible must not excuse a reversible operation from carrying a down payload")
	}
}

// TestNormalizeLeavesCompatibleUntouched pins that neither normalization path
// infers the marker. Down implies Reversible because a rollback payload is
// evidence of itself; nothing on the wire is evidence that the previous image
// can read the new schema, so only the author may claim it.
func TestNormalizeLeavesCompatibleUntouched(t *testing.T) {
	op := NormalizeOperation(BundleOperation{
		Kind: KindSQL,
		Name: "x",
		Up:   PayloadRef{Path: "a.up.sql", Hash: testUpHash},
		Down: &PayloadRef{Path: "a.down.sql", Hash: testDownHash},
	})
	if op.Capabilities.Compatible {
		t.Error("normalization inferred a compatibility claim from a down payload")
	}

	claimed := NormalizeOperation(BundleOperation{
		Kind:         KindSQL,
		Name:         "x",
		Up:           PayloadRef{Path: "a.up.sql", Hash: testUpHash},
		Capabilities: Capabilities{Compatible: true},
	})
	if !claimed.Capabilities.Compatible {
		t.Error("normalization dropped an author's compatibility claim")
	}

	defs := NormalizeDefinitions([]Definition{
		{Name: "a", Hash: testUpHash, Compatible: true},
		{Name: "b", Hash: testUpHash2},
	})
	if !defs[0].Compatible || defs[1].Compatible {
		t.Errorf("NormalizeDefinitions moved Compatible: %v", defs)
	}
}

func TestComputeBundleDigest_StableAcrossReleaseProvenance(t *testing.T) {
	a := validBundle()
	a.Version = "1.4.0"
	a.Git = &GitMetadata{Revision: "abc", Branch: "main"}
	a.ImageDigest = "sha256:deadbeef"
	a.GeneratedAt = "2026-06-04T12:00:00Z"

	b := validBundle()
	b.Version = "1.5.0"
	b.Git = &GitMetadata{Revision: "different-revision"}
	b.GeneratedAt = "2026-07-01T09:30:00Z"

	if ComputeBundleDigest(a) != ComputeBundleDigest(b) {
		t.Fatal("digest must ignore release provenance (version, git, image digest, generated timestamp)")
	}
}

func TestComputeBundleDigest_StableAcrossOperationOrder(t *testing.T) {
	a := validBundle()
	a.Operations = append(a.Operations, BundleOperation{
		Kind: KindSQL, Target: "default", Namespace: "billing",
		Name: "billing/20260604130000_add_invoices",
		Up:   PayloadRef{Path: "payload/sql/default/billing/20260604130000_add_invoices.up.sql", Hash: testUpHash2},
	})

	b := validBundle()
	// Same operations, reversed input order.
	b.Operations = []BundleOperation{
		{Kind: KindSQL, Target: "default", Namespace: "billing",
			Name: "billing/20260604130000_add_invoices",
			Up:   PayloadRef{Path: "payload/sql/default/billing/20260604130000_add_invoices.up.sql", Hash: testUpHash2}},
		a.Operations[0],
	}

	if ComputeBundleDigest(a) != ComputeBundleDigest(b) {
		t.Fatal("digest must be independent of operation input order")
	}
}

func TestComputeBundleDigest_ChangesWithContent(t *testing.T) {
	a := validBundle()
	b := validBundle()
	b.Operations[0].Up.Hash = testUpHash2

	if ComputeBundleDigest(a) == ComputeBundleDigest(b) {
		t.Fatal("digest must change when payload content hash changes")
	}
}

func TestValidateBundle_DigestRoundTrip(t *testing.T) {
	b := validBundle()
	b.Digest = ComputeBundleDigest(b)

	diags := ValidateBundle(&b)
	if diag.HasErrors(diags) {
		t.Fatalf("bundle with correct digest must validate, got: %v", diags)
	}
}

func TestValidateBundle_DigestMismatch(t *testing.T) {
	b := validBundle()
	b.Digest = "0000000000000000000000000000000000000000000000000000000000000000"

	diags := ValidateBundle(&b)
	if !hasCode(diags, ErrorCodeDigestMismatch) {
		t.Fatalf("expected digest mismatch error, got: %v", diags)
	}
}

func TestValidateBundle_Errors(t *testing.T) {
	tests := []struct {
		name string
		want string
		mut  func(*Bundle)
	}{
		{"unknown protocol", ErrorCodeInvalidBundle, func(b *Bundle) { b.Protocol = "migration-bundle.v2" }},
		{"missing appName", ErrorCodeInvalidBundle, func(b *Bundle) { b.AppName = "" }},
		{"no operations", ErrorCodeInvalidBundle, func(b *Bundle) { b.Operations = nil }},
		{"unknown kind", ErrorCodeInvalidBundle, func(b *Bundle) { b.Operations[0].Kind = "graph" }},
		{"missing name", ErrorCodeInvalidBundle, func(b *Bundle) { b.Operations[0].Name = "" }},
		{"unknown safety", ErrorCodeInvalidBundle, func(b *Bundle) { b.Operations[0].Safety = "whenever" }},
		{"missing up hash", ErrorCodeInvalidPayload, func(b *Bundle) { b.Operations[0].Up.Hash = "" }},
		{"bad up hash", ErrorCodeInvalidPayload, func(b *Bundle) { b.Operations[0].Up.Hash = "NOTHEX" }},
		{"missing up path", ErrorCodeInvalidPayload, func(b *Bundle) { b.Operations[0].Up.Path = "" }},
		{"escaping path", ErrorCodeInvalidPayload, func(b *Bundle) { b.Operations[0].Up.Path = "../escape.sql" }},
		{"absolute path", ErrorCodeInvalidPayload, func(b *Bundle) { b.Operations[0].Up.Path = "/etc/passwd" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := validBundle()
			tt.mut(&b)
			diags := ValidateBundle(&b)
			if !hasCode(diags, tt.want) {
				t.Fatalf("expected error code %q, got: %v", tt.want, diags)
			}
		})
	}
}

func TestValidateBundle_DuplicateOperation(t *testing.T) {
	b := validBundle()
	dup := b.Operations[0]
	dup.Up = PayloadRef{Path: "payload/sql/default/iam/dup.up.sql", Hash: testUpHash2}
	dup.Down = nil
	b.Operations = append(b.Operations, dup)

	diags := ValidateBundle(&b)
	if !hasCode(diags, ErrorCodeDuplicateOperation) {
		t.Fatalf("expected duplicate operation error, got: %v", diags)
	}
}

func TestValidateBundle_SameNameDifferentTargetNotDuplicate(t *testing.T) {
	b := validBundle()
	other := b.Operations[0]
	other.Target = "analytics"
	other.Down = nil
	b.Operations = append(b.Operations, other)

	diags := ValidateBundle(&b)
	if hasCode(diags, ErrorCodeDuplicateOperation) {
		t.Fatal("same name in different targets must not be a duplicate")
	}
}

func TestParseBundle_RejectsUnknownFields(t *testing.T) {
	data := []byte(`{"protocol":"migration-bundle.v1","appName":"x","operations":[],"bogus":true}`)
	_, diags := ParseBundle(data)
	if !diag.HasErrors(diags) {
		t.Fatal("strict parse must reject unknown fields")
	}
}

func TestParseAndValidateBundle_RoundTrip(t *testing.T) {
	src := validBundle()
	src.Digest = ComputeBundleDigest(src)
	data, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}

	got, diags := ParseAndValidateBundle(data)
	if diag.HasErrors(diags) {
		t.Fatalf("round-trip produced errors: %v", diags)
	}
	if got == nil {
		t.Fatal("expected non-nil bundle")
	}
	if got.AppName != src.AppName {
		t.Errorf("appName: got %q, want %q", got.AppName, src.AppName)
	}
	// Returned bundle is normalized: operations carry filled defaults.
	if got.Operations[0].OrderKey == "" {
		t.Error("normalized operation must have an order key")
	}
}

func TestComputePayloadHash(t *testing.T) {
	h := ComputePayloadHash([]byte("CREATE TABLE users (id uuid primary key);\n"))
	if !sha256HexPattern.MatchString(h) {
		t.Fatalf("payload hash %q is not canonical sha256 hex", h)
	}
	// Deterministic for identical bytes.
	if h != ComputePayloadHash([]byte("CREATE TABLE users (id uuid primary key);\n")) {
		t.Fatal("payload hash must be deterministic")
	}
}

func hasCode(diags []diag.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}
