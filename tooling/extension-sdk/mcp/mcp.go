// Package mcp implements the narrow stdin/stdout protocol used by
// manifest-declared extension MCP tools. It is intentionally not an MCP server:
// the Putnami CLI owns JSON-RPC and the stdio connection to the agent, while an
// extension receives one tool call and writes one tool result.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	proto "go.putnami.dev/protocol/extension"
)

// Handler handles one extension MCP tool request. Returning a value serializes
// it as a formatted JSON text block, exactly like Putnami's core tools. Return
// an error for a user-facing tool error; Serve encodes it as isError instead of
// forcing callers to reimplement the protocol envelope.
//
// A handler MAY return a value AND an error. That is not a contradiction: it is
// the failure that still produced a report — a validation that ran and found the
// document invalid — and Serve then writes both blocks, the report first and the
// message second, which is exactly what the CLI's own tools do (handleToolsCall
// in tooling/cli/internal/mcp/tools.go). A handler that failed before it had
// anything to say returns a nil value and gets the message alone.
type Handler func(context.Context, proto.ToolCallRequest) (any, error)

// Serve reads one ToolCallRequest from in, routes it to handlers, and writes
// exactly one ToolCallResult to out. It is suitable for a dedicated extension
// subcommand, for example `putnami-go mcp-tool`; callers retain ownership of
// process flags, authentication, and the surrounding command dispatcher.
func Serve(ctx context.Context, in io.Reader, out io.Writer, handlers map[string]Handler) error {
	var request proto.ToolCallRequest
	decoder := json.NewDecoder(in)
	if err := decoder.Decode(&request); err != nil {
		return fmt.Errorf("decode extension MCP tool request: %w", err)
	}
	if err := requireSingleJSONValue(decoder); err != nil {
		return fmt.Errorf("decode extension MCP tool request: %w", err)
	}
	if request.Name == "" {
		return fmt.Errorf("extension MCP tool request is missing name")
	}
	handler, ok := handlers[request.Name]
	if !ok {
		return encodeResult(out, proto.ToolCallResult{
			Content: []proto.ToolContent{{Type: "text", Text: "unknown extension tool: " + request.Name}},
			IsError: true,
		})
	}
	data, err := handler(ctx, request)
	if err != nil {
		// The attached value, when there is one, leads — same order and same
		// encoding as the CLI's core tools, so an agent reads a failed extension
		// tool exactly as it reads a failed built-in one. A value that will not
		// encode is dropped rather than reported twice: the message is the answer
		// the caller needs, and an encoder complaint would bury it.
		if data != nil {
			if attached, encodeErr := json.MarshalIndent(data, "", "  "); encodeErr == nil {
				return encodeResult(out, proto.ToolCallResult{
					Content: []proto.ToolContent{
						{Type: "text", Text: string(attached)},
						{Type: "text", Text: err.Error()},
					},
					IsError: true,
				})
			}
		}
		return encodeResult(out, proto.ToolCallResult{
			Content: []proto.ToolContent{{Type: "text", Text: err.Error()}},
			IsError: true,
		})
	}
	text, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return encodeResult(out, proto.ToolCallResult{
			Content: []proto.ToolContent{{Type: "text", Text: "encode result: " + err.Error()}},
			IsError: true,
		})
	}
	return encodeResult(out, proto.ToolCallResult{
		Content: []proto.ToolContent{{Type: "text", Text: string(text)}},
	})
}

func encodeResult(out io.Writer, result proto.ToolCallResult) error {
	encoder := json.NewEncoder(out)
	if err := encoder.Encode(result); err != nil {
		return fmt.Errorf("encode extension MCP tool result: %w", err)
	}
	return nil
}

func requireSingleJSONValue(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("multiple JSON values")
	}
	return err
}
