package client

// GeneratedClientOperation is one exact operation embedded by a client
// generator, and the unit producer attribution is keyed by.
//
// OperationID is the canonical operation identity taken from the API contract.
// It is deliberately independent from the generated language symbol: a symbol
// normalizer may rewrite punctuation the canonical id keeps (canonical
// "getV1_Operator_Cli-usage" against a Go/TypeScript method symbol
// "getV1_Operator_Cli_usage"), and lineage must survive that rewrite.
//
// Path is the contract template, not a deployment URL.
//
// ProducerProject and ProducerFeature identify the feature that produced THIS
// operation. They are set together or not at all: an operation whose endpoint
// owner declares no feature stays unattributed rather than inheriting the
// lineage of a neighboring operation that happens to share the client.
type GeneratedClientOperation struct {
	OperationID     string `json:"operationId"`
	Method          string `json:"method"`
	Path            string `json:"path"`
	ProducerProject string `json:"producerProject,omitempty"`
	ProducerFeature string `json:"producerFeature,omitempty"`
}

// GeneratedClientDesign identifies the immutable API contract from which a
// generated client was built. One generated client routinely spans several
// producer features, so producer identity lives on each operation instead of on
// the client as a whole.
type GeneratedClientDesign struct {
	Language   string                     `json:"language"`
	Client     string                     `json:"client"`
	SpecHash   string                     `json:"specHash,omitempty"`
	Operations []GeneratedClientOperation `json:"operations"`
}

// FeatureTrace is the exact generated-client → operation → producer-feature
// identity attached to an outgoing request. Every field is resolved from the
// invoked operation, never from a deployment URL or a package name.
//
// ProducerProject and ProducerFeature are empty for an unattributed operation;
// that is a first-class state, not a missing value to be guessed at.
type FeatureTrace struct {
	GeneratedClient string `json:"generatedClient"`
	OperationID     string `json:"operationId"`
	Method          string `json:"method"`
	Path            string `json:"path"`
	SpecHash        string `json:"specHash,omitempty"`
	ProducerProject string `json:"producerProject,omitempty"`
	ProducerFeature string `json:"producerFeature,omitempty"`
}

// Trace resolves one operation from the generated descriptor by its canonical
// operation id — the same key the design graph's generatedFrom edges use. It
// returns nil for an unknown operation instead of guessing from an URL or
// package name, and a trace with empty producer fields for a known but
// unattributed operation.
func (design GeneratedClientDesign) Trace(operationID string) *FeatureTrace {
	for _, operation := range design.Operations {
		if operation.OperationID != operationID {
			continue
		}
		return &FeatureTrace{
			GeneratedClient: design.Client,
			OperationID:     operation.OperationID,
			Method:          operation.Method,
			Path:            operation.Path,
			SpecHash:        design.SpecHash,
			ProducerProject: operation.ProducerProject,
			ProducerFeature: operation.ProducerFeature,
		}
	}
	return nil
}
