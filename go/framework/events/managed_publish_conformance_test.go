package events

import (
	"bytes"
	"io/fs"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	protoevents "go.putnami.dev/protocol/events"
)

// TestManagedPublishConformance_SharedCorpus is the Go framework counterpart
// to @putnami/events' protocol-conformance test. Both consume the canonical
// protocol corpus instead of maintaining language-owned fixture copies.
func TestManagedPublishConformance_SharedCorpus(t *testing.T) {
	fixtures := protoevents.ManagedPublishV1Fixtures()

	for _, test := range []struct {
		kind       string
		wantErrors bool
		minimum    int
	}{
		{kind: "valid", minimum: 4},
		{kind: "invalid", wantErrors: true, minimum: 18},
	} {
		t.Run(test.kind, func(t *testing.T) {
			entries, err := fs.ReadDir(fixtures, test.kind)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) < test.minimum {
				t.Fatalf("managed publish %s fixtures = %d, want at least %d", test.kind, len(entries), test.minimum)
			}

			for _, entry := range entries {
				if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
					continue
				}
				t.Run(entry.Name(), func(t *testing.T) {
					data, err := fs.ReadFile(fixtures, test.kind+"/"+entry.Name())
					if err != nil {
						t.Fatal(err)
					}
					_, diagnostics := protoevents.ParseAndValidateManagedPublishFrame(data)
					if gotErrors := diag.HasErrors(diagnostics); gotErrors != test.wantErrors {
						t.Fatalf("managed publish fixture errors = %t, want %t: %v", gotErrors, test.wantErrors, diagnostics)
					}
				})
			}
		})
	}
}

func TestManagedPublishConformance_ByteStableRetry(t *testing.T) {
	first, err := protoevents.ReadManagedPublishV1Fixture("valid/retry-attempt-1.json")
	if err != nil {
		t.Fatal(err)
	}
	retry, err := protoevents.ReadManagedPublishV1Fixture("valid/retry-attempt-2.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, retry) {
		t.Fatal("managed publish retry fixtures are not byte-for-byte identical")
	}
	if diagnostics := protoevents.ValidateManagedPublishRetry(first, retry); diag.HasErrors(diagnostics) {
		t.Fatalf("managed publish retry fixtures produced errors: %v", diagnostics)
	}
}

func TestManagedPublishConformance_OutcomeClasses(t *testing.T) {
	fixtures := protoevents.ManagedPublishV1Fixtures()
	entries, err := fs.ReadDir(fixtures, "outcomes")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 8 {
		t.Fatalf("managed publish outcome fixtures = %d, want at least 8", len(entries))
	}

	classes := map[protoevents.ManagedPublishOutcomeClass]bool{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			data, err := fs.ReadFile(fixtures, "outcomes/"+entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			fixture, diagnostics := protoevents.ParseAndValidateManagedPublishOutcomeFixture(data)
			if diag.HasErrors(diagnostics) {
				t.Fatalf("managed publish outcome fixture produced errors: %v", diagnostics)
			}
			classes[fixture.Expected.Class] = true
		})
	}

	for _, class := range []protoevents.ManagedPublishOutcomeClass{
		protoevents.ManagedPublishAccepted,
		protoevents.ManagedPublishPermanent,
		protoevents.ManagedPublishRetryable,
		protoevents.ManagedPublishAmbiguous,
	} {
		if !classes[class] {
			t.Errorf("managed publish corpus has no %q outcome", class)
		}
	}
}
