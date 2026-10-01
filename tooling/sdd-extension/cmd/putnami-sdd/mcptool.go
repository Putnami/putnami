package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	proto "go.putnami.dev/protocol/extension"
	sdkmcp "go.putnami.dev/sdk/extension/mcp"
	"go.putnami.dev/tooling/sdd/extension/internal/sdd"
	"go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// The five SDD MCP tools — the agent half of @putnami/sdd, with architecture
// context added later.
//
// # One request in, one result out
//
// This is not an MCP server. The CLI owns JSON-RPC and the agent's stdio
// connection; a manifest-declared extension tool is a process that reads ONE
// proto.ToolCallRequest from stdin and writes ONE proto.ToolCallResult to
// stdout. The SDK's mcp.Serve is that bridge, so this file is a dispatch table
// and five adapters over the same engine builders the commands call.
//
// # Names
//
// Extension tool names must be dot-namespaced and core names always win
// (tooling/cli/internal/mcp/extension_tools.go), so the four are `sdd.*` and
// D4 deletes core's undotted names with no alias.
//
// # What "payload parity" means here, and why it is NOT the interactive rule
//
// The interactive path re-encodes its payload through map[string]any because
// App.runStructuredCommand captures a handler's stdout and decodes it before
// re-encoding — so a built-in command's `data` object comes out with ALPHABETICAL
// keys. The MCP path does the opposite: core's handleToolsCall hands the
// handler's return value straight to json.MarshalIndent, so a report struct
// encodes in DECLARATION order. Applying the interactive adaptation here would
// therefore BREAK parity rather than achieve it, which is why nothing in this
// file round-trips a payload.
//
// The error shape is the second half of the same contract. A core tool that
// fails WITH a value attached reports two content blocks — the value, then the
// message — and one that fails during argument validation reports only the
// message. Each adapter below reproduces its counterpart's split exactly: the
// argument guards return a nil payload, and the engine calls return the report
// they built beside the error.
const (
	toolListFeatures        = "sdd.list_features"
	toolFeatureContext      = "sdd.feature_context"
	toolListSpecs           = "sdd.list_specs"
	toolSpecContext         = "sdd.spec_context"
	toolArchitectureContext = "sdd.architecture_context"
)

// mcpToolArg is the manifest argument that routes this binary to the tool
// bridge. It is a leading positional like every other dispatch here, so one
// executable still answers jobs, subcommands and tools.
const mcpToolArg = "mcp-tool"

// mcpToolHandlers is the dispatch table the bridge routes on. Like
// commandHandlers it is a function rather than a package-level map, so one
// test's mutation cannot reach another's.
func mcpToolHandlers() map[string]sdkmcp.Handler {
	return map[string]sdkmcp.Handler{
		toolListFeatures:        handleListFeatures,
		toolFeatureContext:      handleFeatureContext,
		toolListSpecs:           handleListSpecs,
		toolSpecContext:         handleSpecContext,
		toolArchitectureContext: handleArchitectureContext,
	}
}

// runMCPTool answers one tool call. Errors from Serve are protocol failures
// (a request that would not decode, a result that would not encode) rather
// than tool failures, which Serve already reports as isError content.
func runMCPTool(ctx context.Context, in io.Reader, out io.Writer) error {
	return sdkmcp.Serve(ctx, in, out, mcpToolHandlers())
}

// featureCatalogArgs, featureContextArgs, specCatalogArgs and specContextArgs
// are the tools' argument objects, member for member and tag for tag with the
// core tools they replace (tooling/cli/internal/mcp/adapter.go). They are
// decoded strictly, so an argument the schema does not declare is the same
// "invalid arguments" error on both sides instead of a silently ignored key.
type featureCatalogArgs struct {
	Query    string   `json:"query"`
	Projects []string `json:"projects"`
	Impacted bool     `json:"impacted"`
	Baseline string   `json:"baseline"`
}

type featureContextArgs struct {
	Feature string `json:"feature"`
}

type specCatalogArgs struct {
	Projects []string `json:"projects"`
	Impacted bool     `json:"impacted"`
	Baseline string   `json:"baseline"`
}

type specContextArgs struct {
	Feature string `json:"feature"`
}

type architectureContextArgs struct {
	Domain string `json:"domain"`
}

func handleListFeatures(_ context.Context, request proto.ToolCallRequest) (any, error) {
	var args featureCatalogArgs
	if err := decodeToolArgs(request.Arguments, &args); err != nil {
		return nil, err
	}
	ws, selection, err := toolWorkspace(request)
	if err != nil {
		return nil, err
	}
	return sdd.BuildFeatureCatalogResult(ws, args.Query, selection)
}

func handleFeatureContext(_ context.Context, request proto.ToolCallRequest) (any, error) {
	var args featureContextArgs
	if err := decodeToolArgs(request.Arguments, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Feature) == "" {
		return nil, errors.New("feature is required (exact feature id or unambiguous final segment)")
	}
	ws, _, err := toolWorkspace(request)
	if err != nil {
		return nil, err
	}
	return sdd.BuildAgentFeatureContextResult(ws, args.Feature)
}

func handleListSpecs(_ context.Context, request proto.ToolCallRequest) (any, error) {
	var args specCatalogArgs
	if err := decodeToolArgs(request.Arguments, &args); err != nil {
		return nil, err
	}
	ws, selection, err := toolWorkspace(request)
	if err != nil {
		return nil, err
	}
	return sdd.BuildSpecCatalogResult(ws, selection)
}

func handleSpecContext(_ context.Context, request proto.ToolCallRequest) (any, error) {
	var args specContextArgs
	if err := decodeToolArgs(request.Arguments, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Feature) == "" {
		return nil, errors.New("feature is required (exact authored feature id)")
	}
	ws, _, err := toolWorkspace(request)
	if err != nil {
		return nil, err
	}
	return sdd.BuildSpecContextResult(ws, args.Feature)
}

func handleArchitectureContext(_ context.Context, request proto.ToolCallRequest) (any, error) {
	var args architectureContextArgs
	if err := decodeToolArgs(request.Arguments, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Domain) == "" {
		return nil, errors.New("domain is required (exact authored architecture domain id)")
	}
	ws, _, err := toolWorkspace(request)
	if err != nil {
		return nil, err
	}
	return sdd.BuildArchitectureWorktreeInspectionResult(ws, args.Domain)
}

// toolWorkspace builds the view and the projection one tool call is entitled
// to, from the request and nothing else.
//
// It refuses a request that carries no resolved selection instead of falling
// back to the whole workspace, and the refusal is the load-bearing part. Every
// one of these five tools would answer an unresolved call perfectly well — with
// a well-formed, confidently WRONG payload: a catalog covering every project for
// a caller who asked about one, or an empty one for a caller whose CLI sent no
// membership. A wrong answer an agent cannot tell from a right one is worse than
// no answer, so this fails closed and names the cause.
func toolWorkspace(request proto.ToolCallRequest) (*wsview.Workspace, sdd.Selection, error) {
	selection, resolved := wsview.ToolSelection(request)
	if !resolved {
		return nil, sdd.Selection{}, errors.New(
			"this tool call carries no resolved workspace selection, so @putnami/sdd cannot answer it: " +
				"the CLI serving it does not publish `selection` on a tool request")
	}
	return wsview.FromToolRequest(request), selection, nil
}

// decodeToolArgs is core's decodeArgs, verbatim in behavior: an absent argument
// object is the zero value, and an argument the schema does not declare is a
// rejection rather than a silently dropped key.
func decodeToolArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}
