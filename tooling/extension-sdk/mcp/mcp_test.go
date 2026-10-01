package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	proto "go.putnami.dev/protocol/extension"
)

func TestServeRoutesAndEncodesResult(t *testing.T) {
	var out bytes.Buffer
	err := Serve(context.Background(), bytes.NewBufferString(`{"name":"putnami.search","arguments":{"query":"MCP"},"workspaceRoot":"/workspace","extensionRoot":"/extension"}`), &out, map[string]Handler{
		"putnami.search": func(_ context.Context, request proto.ToolCallRequest) (any, error) {
			if request.WorkspaceRoot != "/workspace" || request.ExtensionRoot != "/extension" {
				t.Fatalf("unexpected request roots: %#v", request)
			}
			return map[string]any{"query": "MCP"}, nil
		},
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var result proto.ToolCallResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.IsError || len(result.Content) != 1 || result.Content[0].Type != "text" || result.Content[0].Text != "{\n  \"query\": \"MCP\"\n}" {
		t.Errorf("result = %#v", result)
	}
}

func TestServeReturnsToolErrorForUnknownHandler(t *testing.T) {
	var out bytes.Buffer
	err := Serve(context.Background(), bytes.NewBufferString(`{"name":"putnami.missing"}`), &out, nil)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var result proto.ToolCallResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.IsError || result.Content[0].Text != "unknown extension tool: putnami.missing" {
		t.Errorf("result = %#v", result)
	}
}

// TestServeReportsTheValueAttachedToAToolError pins the two-block failure shape
// against the CLI's own.
//
// A tool that RAN and found something — an invalid document, a feature with no
// spec — has both a report and a verdict, and the CLI's core tools write both:
// the value first, the message second, isError true. An SDK that dropped the
// value would make the same failure read differently depending on whether it
// came from a built-in tool or an extension one, which is precisely what an
// extraction must not introduce.
func TestServeReportsTheValueAttachedToAToolError(t *testing.T) {
	var out bytes.Buffer
	err := Serve(context.Background(), bytes.NewBufferString(`{"name":"putnami.check"}`), &out, map[string]Handler{
		"putnami.check": func(context.Context, proto.ToolCallRequest) (any, error) {
			return map[string]any{"valid": false}, errors.New("one document is invalid")
		},
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var result proto.ToolCallResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.IsError {
		t.Error("a failed tool call must be isError")
	}
	if len(result.Content) != 2 {
		t.Fatalf("content blocks = %d, want the report and the message: %#v", len(result.Content), result.Content)
	}
	if result.Content[0].Text != "{\n  \"valid\": false\n}" {
		t.Errorf("first block = %q, want the attached report", result.Content[0].Text)
	}
	if result.Content[1].Text != "one document is invalid" {
		t.Errorf("second block = %q, want the message", result.Content[1].Text)
	}
}

// TestServeReportsOnlyTheMessageWithoutAnAttachedValue is the other half: a
// handler that failed BEFORE producing anything — a rejected argument — has no
// report to show, and inventing an empty one would tell an agent the tool ran.
func TestServeReportsOnlyTheMessageWithoutAnAttachedValue(t *testing.T) {
	var out bytes.Buffer
	err := Serve(context.Background(), bytes.NewBufferString(`{"name":"putnami.check"}`), &out, map[string]Handler{
		"putnami.check": func(context.Context, proto.ToolCallRequest) (any, error) {
			return nil, errors.New("feature is required")
		},
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var result proto.ToolCallResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.IsError || len(result.Content) != 1 || result.Content[0].Text != "feature is required" {
		t.Errorf("result = %#v", result)
	}
}
