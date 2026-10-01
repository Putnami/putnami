package client

import "testing"

// multiFeatureDesign is the regression fixture the per-operation contract exists
// for: one generated client whose operations belong to two different producer
// features plus one unattributed operation, and a canonical operation id whose
// punctuation ("Cli-usage") no language symbol normalizer can keep.
func multiFeatureDesign() GeneratedClientDesign {
	return GeneratedClientDesign{
		Language: "go",
		Client:   "PlatformClient",
		SpecHash: "spec-123",
		Operations: []GeneratedClientOperation{
			{
				OperationID: "getV1_Operator_Cli-usage", Method: "GET", Path: "/v1/operator/cli-usage",
				ProducerProject: "acme-platform", ProducerFeature: "platform/operator-cli-usage",
			},
			{
				OperationID: "getV1_Billing_Invoices", Method: "GET", Path: "/v1/billing/invoices",
				ProducerProject: "acme-platform", ProducerFeature: "platform/billing",
			},
			{OperationID: "getV1_Health", Method: "GET", Path: "/v1/health"},
		},
	}
}

func TestGeneratedClientDesignTraceIsExact(t *testing.T) {
	design := GeneratedClientDesign{
		Language: "go",
		Client:   "ItemsClient",
		SpecHash: "spec-123",
		Operations: []GeneratedClientOperation{
			{
				OperationID: "getItems", Method: "GET", Path: "/items",
				ProducerProject: "example/provider", ProducerFeature: "items/manage",
			},
		},
	}
	trace := design.Trace("getItems")
	if trace == nil {
		t.Fatal("trace = nil")
	}
	want := FeatureTrace{
		GeneratedClient: "ItemsClient",
		OperationID:     "getItems",
		Method:          "GET",
		Path:            "/items",
		SpecHash:        "spec-123",
		ProducerProject: "example/provider",
		ProducerFeature: "items/manage",
	}
	if *trace != want {
		t.Fatalf("trace = %+v, want %+v", *trace, want)
	}
	if unknown := design.Trace("deleteItems"); unknown != nil {
		t.Fatalf("unknown operation was guessed: %+v", unknown)
	}
}

// A client that spans two features must attribute each operation to its own
// producer — the whole point of moving attribution off the client.
func TestGeneratedClientDesignTraceKeepsPerOperationProducers(t *testing.T) {
	design := multiFeatureDesign()

	cliUsage := design.Trace("getV1_Operator_Cli-usage")
	if cliUsage == nil || cliUsage.ProducerFeature != "platform/operator-cli-usage" {
		t.Fatalf("cli-usage trace = %+v", cliUsage)
	}
	if cliUsage.Method != "GET" || cliUsage.Path != "/v1/operator/cli-usage" || cliUsage.SpecHash != "spec-123" {
		t.Fatalf("cli-usage trace lost operation identity: %+v", cliUsage)
	}

	billing := design.Trace("getV1_Billing_Invoices")
	if billing == nil || billing.ProducerFeature != "platform/billing" {
		t.Fatalf("billing trace = %+v", billing)
	}
}

// An operation whose endpoint owner declares no feature stays unattributed: it
// resolves (so method/path/spec hash are still exact) but never borrows the
// lineage of a sibling operation in the same client.
func TestGeneratedClientDesignTraceLeavesUnrelatedOperationsUnattributed(t *testing.T) {
	trace := multiFeatureDesign().Trace("getV1_Health")
	if trace == nil {
		t.Fatal("known operation did not resolve")
	}
	if trace.ProducerProject != "" || trace.ProducerFeature != "" {
		t.Fatalf("unattributed operation carries false lineage: %+v", trace)
	}
	if trace.OperationID != "getV1_Health" || trace.Path != "/v1/health" {
		t.Fatalf("unattributed trace lost operation identity: %+v", trace)
	}
}

// The canonical operation id is the trace key, so a descriptor keeps punctuation
// a Go/TypeScript symbol normalizer would have rewritten to "_".
func TestGeneratedClientDesignTraceKeepsCanonicalOperationID(t *testing.T) {
	design := multiFeatureDesign()
	if trace := design.Trace("getV1_Operator_Cli_usage"); trace != nil {
		t.Fatalf("normalized language symbol resolved as a canonical operation: %+v", trace)
	}
	trace := design.Trace("getV1_Operator_Cli-usage")
	if trace == nil || trace.OperationID != "getV1_Operator_Cli-usage" {
		t.Fatalf("canonical operation id was not preserved: %+v", trace)
	}
}
