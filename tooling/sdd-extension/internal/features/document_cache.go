package features

import (
	"sync"

	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
)

// DocumentCache answers each strict parse of an authored document once per
// distinct bytes, for the evaluations that share it. A parse is a function of
// the bytes alone, so the shared answer is the one each evaluation would
// compute, and its findings name positions inside the document, which the
// caller prefixes with the document's path. The evaluations read the parsed
// documents and findings and never change them. It is safe for concurrent
// use, and a nil cache parses every time.
type DocumentCache struct {
	mu        sync.Mutex
	manifests map[string]parsedManifest
	documents map[string]parsedFeatureDocument
	evidence  map[string]parsedEvidence
}

type parsedManifest struct {
	manifest *featureproto.Manifest
	findings []diag.Diagnostic
}

type parsedFeatureDocument struct {
	document *featureproto.FeatureDocument
	findings []diag.Diagnostic
}

type parsedEvidence struct {
	document *featureproto.EvidenceDocument
	findings []diag.Diagnostic
}

// NewDocumentCache returns an empty cache.
func NewDocumentCache() *DocumentCache {
	return &DocumentCache{
		manifests: map[string]parsedManifest{},
		documents: map[string]parsedFeatureDocument{},
		evidence:  map[string]parsedEvidence{},
	}
}

// manifest is featureproto.ParseManifest(data).
func (cache *DocumentCache) manifest(data []byte) (*featureproto.Manifest, []diag.Diagnostic) {
	if cache == nil {
		return featureproto.ParseManifest(data)
	}
	cache.mu.Lock()
	answer, ok := cache.manifests[string(data)]
	cache.mu.Unlock()
	if !ok {
		answer.manifest, answer.findings = featureproto.ParseManifest(data)
		cache.mu.Lock()
		cache.manifests[string(data)] = answer
		cache.mu.Unlock()
	}
	return answer.manifest, answer.findings
}

// featureDocument is featureproto.ParseFeatureDocument(data).
func (cache *DocumentCache) featureDocument(data []byte) (*featureproto.FeatureDocument, []diag.Diagnostic) {
	if cache == nil {
		return featureproto.ParseFeatureDocument(data)
	}
	cache.mu.Lock()
	answer, ok := cache.documents[string(data)]
	cache.mu.Unlock()
	if !ok {
		answer.document, answer.findings = featureproto.ParseFeatureDocument(data)
		cache.mu.Lock()
		cache.documents[string(data)] = answer
		cache.mu.Unlock()
	}
	return answer.document, answer.findings
}

// evidenceDocument is featureproto.ParseEvidenceDocument(data).
func (cache *DocumentCache) evidenceDocument(data []byte) (*featureproto.EvidenceDocument, []diag.Diagnostic) {
	if cache == nil {
		return featureproto.ParseEvidenceDocument(data)
	}
	cache.mu.Lock()
	answer, ok := cache.evidence[string(data)]
	cache.mu.Unlock()
	if !ok {
		answer.document, answer.findings = featureproto.ParseEvidenceDocument(data)
		cache.mu.Lock()
		cache.evidence[string(data)] = answer
		cache.mu.Unlock()
	}
	return answer.document, answer.findings
}
