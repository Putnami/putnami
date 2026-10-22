package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/typescript/extension/internal/toolchain"
)

// handleToolchainResolve answers the reserved toolchain-resolve control call.
// Core spawns it with the workspace root as the working directory, one request
// on stdin naming the identities the manifest lists in runtime.resolves, and
// reads the lock's next pin of each one from stdout. Every other invocation is
// declined with handled=false.
func handleToolchainResolve(args []string) (bool, error) {
	return wsproto.ServeToolchainResolve(args, os.Stdin, os.Stdout, func(request wsproto.ToolchainResolveRequest) (wsproto.ToolchainResolveResult, error) {
		root, err := os.Getwd()
		if err != nil {
			return wsproto.ToolchainResolveResult{}, fmt.Errorf("resolve workspace root: %w", err)
		}
		result := toolchain.ResolveToolchains(context.Background(), root, strings.TrimSpace(os.Getenv(cliUserAgentEnv)), request)
		result.Extension = tsExtensionName
		return result, nil
	})
}
