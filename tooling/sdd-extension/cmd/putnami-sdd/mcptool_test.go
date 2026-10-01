package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/cli"
	"go.putnami.dev/tooling/sdd/extension/internal/sdd"
)

// The MCP tool bridge, tested where the CLI's parity test cannot reach: the
// routing, the refusals, and the failure SHAPE.
//
// Payload parity with core is measured in tooling/cli/internal/cli against the
// live oracle — bytes against bytes, over a real subprocess. What is left here
// is what that comparison cannot state: that an unresolved call is refused
// instead of answered, and that a failure carries the report beside the message
// exactly when core's does.

// callTool runs one request through the bridge and returns the result.
func callTool(t *testing.T, request proto.ToolCallRequest) proto.ToolCallResult {
	t.Helper()
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var out bytes.Buffer
	if err := runMCPTool(context.Background(), bytes.NewReader(payload), &out); err != nil {
		t.Fatalf("runMCPTool: %v", err)
	}
	var result proto.ToolCallResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode result %q: %v", out.String(), err)
	}
	return result
}

// toolFixture writes the smallest workspace the five tools have something to
// say about, and returns the request members that describe it.
func toolFixture(t *testing.T) (root string, projects []proto.ToolProjectRef) {
	t.Helper()
	root = t.TempDir()
	write := func(rel, contents string) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("putnami.workspace.json", `{"name":"tools","includes":["billing","catalog"]}`)
	write("billing/putnami.json", `{"name":"@acme/billing","type":"application"}`)
	write("catalog/putnami.json", `{"name":"@acme/catalog","type":"library"}`)
	write("billing/putnami.features.json", `{
  "protocolVersion": 1,
  "namespace": "billing",
  "features": [{
    "id": "billing/invoice",
    "type": "feature",
    "name": "Invoice export",
    "outcome": "Customers export issued invoices",
    "owner": "billing",
    "target": "modeled"
  }]
}`)
	write("billing/specs/billing.invoice.json", `{
  "protocolVersion": 1,
  "feature": "billing/invoice",
  "outcomes": ["Customers export issued invoices"],
  "requirements": []
}`)
	write("catalog/putnami.architecture.json", `{
  "protocolVersion": 1,
  "domain": "catalog",
  "owner": "catalog-team",
  "projects": ["/catalog"],
  "owns": [{"id":"catalog.product-identity","kind":"model","description":"Canonical product identifiers"}],
  "exports": [{
    "id": "catalog.products.v1",
    "version": 1,
    "status": "active",
    "description": "Stable product package reference.",
    "facts": [{"name":"id","authority":"catalog"}],
    "modes": ["reference"],
    "compatibility": {"strategy":"additive","minimumConsumerVersion":1}
  }],
  "imports": []
}`)
	write("billing/putnami.architecture.json", `{
  "protocolVersion": 1,
  "domain": "billing",
  "owner": "billing-team",
  "projects": ["/billing"],
  "owns": [{"id":"billing.invoice","kind":"model","description":"Issued customer invoices"}],
  "exports": [{
    "id": "billing.invoices.v1",
    "version": 1,
    "status": "active",
    "description": "Issued invoice query contract.",
    "facts": [{"name":"number","authority":"billing","classification":"internal","personalData":"none"}],
    "modes": ["query"],
    "compatibility": {"strategy":"additive","minimumConsumerVersion":1}
  }],
  "imports": [{
    "id": "billing.catalog-reference.v1",
    "version": 1,
    "from": {"domain":"catalog","export":"catalog.products.v1"},
    "as": "billing.catalog-reference",
    "mode": "reference",
    "status": "active",
    "facts": ["id"],
    "justification": "Billing code imports the stable catalog contract.",
    "bindings": [{"kind":"project-dependency","consumerProject":"/billing","producerProject":"/catalog"}]
  }]
}`)
	return root, []proto.ToolProjectRef{
		{ID: "/billing", Name: "@acme/billing", Type: "application", Path: "billing", Dependencies: []string{"/catalog"}},
		{ID: "/catalog", Name: "@acme/catalog", Type: "library", Path: "catalog"},
	}
}

func toolRequest(t *testing.T, name string, arguments string) proto.ToolCallRequest {
	t.Helper()
	root, projects := toolFixture(t)
	request := proto.ToolCallRequest{
		Name:              name,
		WorkspaceRoot:     root,
		WorkspaceProjects: projects,
		Selection: &proto.ToolSelection{
			Mode: proto.ToolSelectionModeAll, ProjectIDs: []string{"/billing", "/catalog"},
		},
	}
	if arguments != "" {
		request.Arguments = json.RawMessage(arguments)
	}
	return request
}

// TestEveryDeclaredToolAnswers is the routing check: each of the five names
// reaches a handler and produces a payload the fixture's documents explain.
func TestEveryDeclaredToolAnswers(t *testing.T) {
	for _, tool := range []struct {
		name      string
		arguments string
		contains  string
	}{
		{name: "sdd.list_features", contains: `"billing/invoice"`},
		{name: "sdd.feature_context", arguments: `{"feature":"billing/invoice"}`, contains: `"billing/invoice"`},
		{name: "sdd.list_specs", contains: `"billing/specs/billing.invoice.json"`},
		{name: "sdd.spec_context", arguments: `{"feature":"billing/invoice"}`, contains: `"Customers export issued invoices"`},
		{name: "sdd.architecture_context", arguments: `{"domain":"billing"}`, contains: `"billing.catalog-reference.v1"`},
	} {
		t.Run(tool.name, func(t *testing.T) {
			result := callTool(t, toolRequest(t, tool.name, tool.arguments))
			if result.IsError {
				t.Fatalf("%s failed: %s", tool.name, result.Content[0].Text)
			}
			if len(result.Content) != 1 {
				t.Fatalf("%s returned %d content blocks, want one", tool.name, len(result.Content))
			}
			if !strings.Contains(result.Content[0].Text, tool.contains) {
				t.Errorf("%s did not report %s:\n%s", tool.name, tool.contains, result.Content[0].Text)
			}
		})
	}
}

// TestArchitectureContextReturnsTheCompleteDeclaredBoundary proves the agent
// gets the protocol's typed domain and edge projection, not a second summary
// assembled by the MCP adapter. In particular, the consumer can tell that its
// reviewed reference contract authorizes importing only the producer's `id`.
func TestArchitectureContextReturnsTheCompleteDeclaredBoundary(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "architecture-agent-context", "architecture-context-returns-the-complete-domain")
	result := callTool(t, toolRequest(t, toolArchitectureContext, `{"domain":"billing"}`))
	if result.IsError || len(result.Content) != 1 {
		t.Fatalf("architecture context = %#v, want one successful report", result)
	}
	var report sdd.ArchitectureInspectionReport
	if err := json.Unmarshal([]byte(result.Content[0].Text), &report); err != nil {
		t.Fatalf("decode architecture report: %v\n%s", err, result.Content[0].Text)
	}
	if report.Requested != "billing" || report.Domain == nil || report.Domain.ID != "billing" {
		t.Fatalf("domain = %+v, requested = %q", report.Domain, report.Requested)
	}
	if len(report.Domain.Owns) != 1 || len(report.Domain.Exports) != 1 || len(report.Domain.Imports) != 1 {
		t.Fatalf("complete declaration = %+v", report.Domain)
	}
	if got := report.Domain.Imports[0].Facts; len(got) != 1 || got[0] != "id" {
		t.Fatalf("consumer import facts = %v, want exactly [id]", got)
	}
	if len(report.Inbound) != 1 || report.Inbound[0].ProducerDomain != "catalog" || report.Inbound[0].ConsumerDomain != "billing" {
		t.Fatalf("inbound edges = %+v", report.Inbound)
	}
	if len(report.Outbound) != 0 || len(report.Observed) != 1 || len(report.Findings) != 0 {
		t.Fatalf("relationships = outbound:%+v observed:%+v findings:%+v", report.Outbound, report.Observed, report.Findings)
	}
	if report.Coverage == nil || report.Coverage.ProjectDependencies != "enforced-for-mapped-projects" {
		t.Fatalf("coverage = %+v", report.Coverage)
	}
	if report.Baseline.Compared || report.Baseline.Requested != "" {
		t.Fatalf("baseline = %+v, want worktree-only", report.Baseline)
	}
}

// TestToolHandlersCoverTheManifest keeps the dispatch table and the manifest
// together, the same way main_test.go does for jobs and subcommands: a tool the
// manifest declares and this binary does not handle fails at run time in a
// user's agent session with "unknown extension tool".
func TestToolHandlersCoverTheManifest(t *testing.T) {
	m := committedManifest(t)
	handlers := mcpToolHandlers()
	for name := range m.Tools {
		if _, handled := handlers[name]; !handled {
			t.Errorf("manifest declares tool %q, which this binary does not handle", name)
		}
	}
	for name := range handlers {
		if _, declared := m.Tools[name]; !declared {
			t.Errorf("tool %q has a handler but no manifest entry runs it", name)
		}
	}
	if len(handlers) == 0 {
		t.Fatal("the tool table is empty; this assertion would pass vacuously")
	}
	names := make([]string, 0, len(handlers))
	for name := range handlers {
		names = append(names, name)
	}
	sort.Strings(names)
	want := []string{"sdd.architecture_context", "sdd.feature_context", "sdd.list_features", "sdd.list_specs", "sdd.spec_context"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("tools = %v, want %v", names, want)
	}
}

// TestAnUnresolvedCallIsRefusedRatherThanAnswered is the fail-closed property,
// and the reason it is stated as its own test.
//
// Every one of the five would answer a selection-less request without
// complaining — with an empty catalog or a whole-workspace one, depending on
// what the caller asked. That answer is well-formed, exit-zero, and wrong, and
// an agent has no way to tell it from a right one. Measured before the wire
// carried a selection: `sdd.list_features` returned `features: []` over a
// workspace with two authored features.
func TestAnUnresolvedCallIsRefusedRatherThanAnswered(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "refuse-rather-than-widen", "an-unresolved-tool-call-is-refused")
	for _, name := range []string{
		"sdd.architecture_context", "sdd.list_features", "sdd.feature_context", "sdd.list_specs", "sdd.spec_context",
	} {
		request := toolRequest(t, name, `{"feature":"billing/invoice"}`)
		if name == "sdd.architecture_context" {
			request.Arguments = json.RawMessage(`{"domain":"billing"}`)
		}
		if name == "sdd.list_features" || name == "sdd.list_specs" {
			request.Arguments = nil
		}
		request.Selection = nil

		result := callTool(t, request)
		if !result.IsError {
			t.Errorf("%s answered a call the orchestrator resolved nothing for:\n%s",
				name, result.Content[0].Text)
			continue
		}
		if !strings.Contains(result.Content[0].Text, "no resolved workspace selection") {
			t.Errorf("%s refused with %q, want a message naming the missing member", name, result.Content[0].Text)
		}
	}
}

// TestArgumentFailuresCarryNoReport pins the ONE-block half of the failure
// contract: a call rejected before anything ran has no report to attach, and
// inventing an empty one would tell an agent the tool ran.
func TestArgumentFailuresCarryNoReport(t *testing.T) {
	for _, tool := range []struct {
		name      string
		arguments string
		message   string
	}{
		{"sdd.feature_context", `{}`, "feature is required"},
		{"sdd.spec_context", `{}`, "feature is required"},
		{"sdd.architecture_context", `{}`, "domain is required"},
		{"sdd.architecture_context", `{"domain":"billing","nope":1}`, "invalid arguments"},
		{"sdd.list_features", `{"nope":1}`, "invalid arguments"},
		{"sdd.list_specs", `{"nope":1}`, "invalid arguments"},
	} {
		result := callTool(t, toolRequest(t, tool.name, tool.arguments))
		if !result.IsError {
			t.Errorf("%s accepted %s", tool.name, tool.arguments)
			continue
		}
		if len(result.Content) != 1 {
			t.Errorf("%s returned %d content blocks for an argument failure, want one",
				tool.name, len(result.Content))
			continue
		}
		if !strings.Contains(result.Content[0].Text, tool.message) {
			t.Errorf("%s message = %q, want one containing %q", tool.name, result.Content[0].Text, tool.message)
		}
	}
}

// TestAFailedEvaluationCarriesItsReport pins the TWO-block half: a tool that
// RAN and found something reports the value first and the verdict second,
// exactly as core's own tools do. The comparison against core is in the CLI's
// parity test; this states the shape so a regression names itself.
func TestAFailedEvaluationCarriesItsReport(t *testing.T) {
	result := callTool(t, toolRequest(t, "sdd.spec_context", `{"feature":"billing/nope"}`))
	if !result.IsError {
		t.Fatal("an unknown feature was reported as a success")
	}
	if len(result.Content) != 2 {
		t.Fatalf("content blocks = %d, want the report and the message: %#v", len(result.Content), result.Content)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(result.Content[0].Text), &report); err != nil {
		t.Fatalf("the first block is not the report: %v\n%s", err, result.Content[0].Text)
	}
	if report["feature"] != "billing/nope" {
		t.Errorf("the attached report is about %v, want the feature that was asked for", report["feature"])
	}
	if !strings.Contains(result.Content[1].Text, "billing/nope") {
		t.Errorf("the second block = %q, want the verdict", result.Content[1].Text)
	}
}

// TestArchitectureContextFailsClosed pins both post-evaluation failures. They
// carry the typed report beside the error because the evaluator ran; missing
// and unknown arguments are covered above and carry no invented report.
func TestArchitectureContextFailsClosed(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "architecture-agent-context", "architecture-context-fails-closed")

	t.Run("relevant architecture finding", func(t *testing.T) {
		request := toolRequest(t, toolArchitectureContext, `{"domain":"billing"}`)
		path := filepath.Join(request.WorkspaceRoot, "billing", "putnami.architecture.json")
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read architecture manifest: %v", err)
		}
		withoutBinding := strings.Replace(string(contents),
			`    "justification": "Billing code imports the stable catalog contract.",
    "bindings": [{"kind":"project-dependency","consumerProject":"/billing","producerProject":"/catalog"}]`,
			`    "justification": "Billing code imports the stable catalog contract."`, 1)
		if withoutBinding == string(contents) {
			t.Fatal("fixture binding was not removed")
		}
		if err := os.WriteFile(path, []byte(withoutBinding), 0o644); err != nil {
			t.Fatalf("write unbound architecture manifest: %v", err)
		}

		result := callTool(t, request)
		if !result.IsError || len(result.Content) != 2 {
			t.Fatalf("finding result = %#v, want report plus error", result)
		}
		var report sdd.ArchitectureInspectionReport
		if err := json.Unmarshal([]byte(result.Content[0].Text), &report); err != nil {
			t.Fatalf("decode report: %v", err)
		}
		if report.Domain == nil || report.Domain.ID != "billing" || len(report.Findings) != 1 {
			t.Fatalf("finding report = %+v", report)
		}
	})

	t.Run("unknown domain", func(t *testing.T) {
		result := callTool(t, toolRequest(t, toolArchitectureContext, `{"domain":"missing"}`))
		if !result.IsError || len(result.Content) != 2 {
			t.Fatalf("unknown domain result = %#v, want report plus error", result)
		}
		var report sdd.ArchitectureInspectionReport
		if err := json.Unmarshal([]byte(result.Content[0].Text), &report); err != nil {
			t.Fatalf("decode report: %v", err)
		}
		if report.Requested != "missing" || report.Domain != nil || len(report.Diagnostics) == 0 {
			t.Fatalf("unknown-domain report = %+v", report)
		}
		if !strings.Contains(result.Content[1].Text, "missing") {
			t.Fatalf("verdict = %q, want exact unknown domain", result.Content[1].Text)
		}
	})

	t.Run("structurally invalid repository", func(t *testing.T) {
		request := toolRequest(t, toolArchitectureContext, `{"domain":"billing"}`)
		path := filepath.Join(request.WorkspaceRoot, "billing", "putnami.architecture.json")
		if err := os.WriteFile(path, []byte(`{"protocolVersion":1,"domain":`), 0o644); err != nil {
			t.Fatalf("break architecture manifest: %v", err)
		}
		result := callTool(t, request)
		if !result.IsError || len(result.Content) != 2 {
			t.Fatalf("invalid repository result = %#v, want report plus error", result)
		}
		var report sdd.ArchitectureInspectionReport
		if err := json.Unmarshal([]byte(result.Content[0].Text), &report); err != nil {
			t.Fatalf("decode report: %v", err)
		}
		if report.Domain != nil || len(report.Diagnostics) == 0 {
			t.Fatalf("structural failure report = %+v", report)
		}
		if !strings.Contains(result.Content[1].Text, "architecture context failed") {
			t.Fatalf("verdict = %q, want architecture evaluation failure", result.Content[1].Text)
		}
	})
}

// TestUnknownToolNamesAreReportedNotRouted keeps the bridge from answering for
// a name it does not own — the failure an agent sees when a manifest and this
// binary disagree.
func TestUnknownToolNamesAreReportedNotRouted(t *testing.T) {
	result := callTool(t, toolRequest(t, "sdd.nope", ""))
	if !result.IsError || !strings.Contains(result.Content[0].Text, "unknown extension tool: sdd.nope") {
		t.Errorf("result = %#v", result)
	}
}

// TestTheMCPArgumentRoutesToTheBridge is the entrypoint half: `mcp-tool` must
// reach the bridge, and never the job dispatcher — a tool call whose stdout
// carried cli.Run's JSONL beside the result would be rejected by the CLI as
// trailing data.
func TestTheMCPArgumentRoutesToTheBridge(t *testing.T) {
	request := toolRequest(t, "sdd.list_features", "")
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var stdout bytes.Buffer
	dispatched := false
	code, err := runEntrypoint([]string{mcpToolArg}, bytes.NewReader(payload), &stdout, io.Discard,
		func(map[string]cli.JobFunc) { dispatched = true })
	if err != nil {
		t.Fatalf("runEntrypoint: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if dispatched {
		t.Fatal("the job dispatcher ran for a tool call; its JSONL would corrupt the result")
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	var result proto.ToolCallResult
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("stdout is not one ToolCallResult: %v\n%s", err, stdout.String())
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("stdout carries a second document (%v); the CLI reads exactly one", trailing)
	}
	if result.IsError {
		t.Errorf("the routed call failed: %s", result.Content[0].Text)
	}
}
