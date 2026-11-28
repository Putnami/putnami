package features

import (
	"reflect"
	"sync"
	"testing"

	featureproto "go.putnami.dev/protocol/features"
)

// TestTheDocumentCacheAnswersTheParse pins that a shared cache answers what
// the strict parse answers, for a valid and an invalid document, and that
// concurrent evaluations reading it agree.
func TestTheDocumentCacheAnswersTheParse(t *testing.T) {
	valid := []byte(`{"protocolVersion":1,"namespace":"billing","features":[{"id":"billing/invoicing","type":"feature","name":"Invoicing","outcome":"Invoices","owner":"billing","target":"modeled"}]}`)
	invalid := []byte(`{"protocolVersion":1,"namespace":"billing","features":[],"unknown":true}`)
	evidence := []byte(`{"protocolVersion":1,"evidence":[]}`)
	cache := NewDocumentCache()
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for _, data := range [][]byte{valid, invalid} {
				gotManifest, gotFindings := cache.manifest(data)
				wantManifest, wantFindings := featureproto.ParseManifest(data)
				if !reflect.DeepEqual(gotManifest, wantManifest) || !reflect.DeepEqual(gotFindings, wantFindings) {
					t.Errorf("cached manifest parse = %+v %v, want %+v %v", gotManifest, gotFindings, wantManifest, wantFindings)
				}
				gotDocument, gotDocumentFindings := cache.featureDocument(data)
				wantDocument, wantDocumentFindings := featureproto.ParseFeatureDocument(data)
				if !reflect.DeepEqual(gotDocument, wantDocument) || !reflect.DeepEqual(gotDocumentFindings, wantDocumentFindings) {
					t.Errorf("cached feature document parse differs")
				}
			}
			gotEvidence, gotEvidenceFindings := cache.evidenceDocument(evidence)
			wantEvidence, wantEvidenceFindings := featureproto.ParseEvidenceDocument(evidence)
			if !reflect.DeepEqual(gotEvidence, wantEvidence) || !reflect.DeepEqual(gotEvidenceFindings, wantEvidenceFindings) {
				t.Errorf("cached evidence parse differs")
			}
		}()
	}
	wait.Wait()
	if first, _ := cache.manifest(valid); first == nil {
		t.Fatal("a valid manifest parsed to nothing")
	} else if second, _ := cache.manifest(valid); first != second {
		t.Fatal("the cache parsed the same bytes twice")
	}
	var none *DocumentCache
	if manifest, _ := none.manifest(valid); manifest == nil {
		t.Fatal("a nil cache did not parse")
	}
}
