package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	proto "go.putnami.dev/protocol/extension"
	sdkmcp "go.putnami.dev/sdk/extension/mcp"
	"go.putnami.dev/typescript/extension/internal/dependencydocs"
)

const (
	mcpToolArg         = "mcp-tool"
	toolTypeScriptDocs = "putnami.typescript_docs"
	// maxDocsResolveTime leaves headroom under the manifest's 10s host timeout,
	// so an unexpectedly deep workspace returns a bounded answer rather than
	// being killed. It matches the Go documentation tool's budget.
	maxDocsResolveTime = 8 * time.Second
)

type docsArgs struct {
	Reference string `json:"reference"`
	Project   string `json:"project"`
}

func mcpToolHandlers() map[string]sdkmcp.Handler {
	return map[string]sdkmcp.Handler{toolTypeScriptDocs: handleTypeScriptDocs}
}

func runMCPTool(ctx context.Context, in io.Reader, out io.Writer) error {
	return sdkmcp.Serve(ctx, in, out, mcpToolHandlers())
}

func handleTypeScriptDocs(ctx context.Context, request proto.ToolCallRequest) (any, error) {
	var args docsArgs
	if err := decodeToolArgs(request.Arguments, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Reference) == "" {
		return nil, errors.New("reference is required (exact npm package name)")
	}
	resolveCtx, cancel := context.WithTimeout(ctx, maxDocsResolveTime)
	defer cancel()
	return dependencydocs.Resolve(resolveCtx, request.WorkspaceRoot, args.Reference, args.Project), nil
}

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
