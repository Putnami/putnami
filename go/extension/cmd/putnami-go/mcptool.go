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

	"go.putnami.dev/go/extension/internal/dependencydocs"
	proto "go.putnami.dev/protocol/extension"
	sdkmcp "go.putnami.dev/sdk/extension/mcp"
)

const (
	mcpToolArg         = "mcp-tool"
	toolGoDocs         = "putnami.go_docs"
	maxDocsResolveTime = 8 * time.Second
)

type docsArgs struct {
	Reference string `json:"reference"`
	Project   string `json:"project"`
}

func mcpToolHandlers() map[string]sdkmcp.Handler {
	return map[string]sdkmcp.Handler{toolGoDocs: handleGoDocs}
}

func runMCPTool(ctx context.Context, in io.Reader, out io.Writer) error {
	return sdkmcp.Serve(ctx, in, out, mcpToolHandlers())
}

func handleGoDocs(ctx context.Context, request proto.ToolCallRequest) (any, error) {
	var args docsArgs
	if err := decodeToolArgs(request.Arguments, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Reference) == "" {
		return nil, errors.New("reference is required (exact Go module path)")
	}
	resolveCtx, cancel := context.WithTimeout(ctx, maxDocsResolveTime)
	defer cancel()
	return dependencydocs.Resolve(resolveCtx, request.WorkspaceRoot, request.ExtensionRoot, args.Reference, args.Project), nil
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
