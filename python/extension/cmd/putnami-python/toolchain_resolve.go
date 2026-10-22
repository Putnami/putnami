package main

import (
	"fmt"
	"io"
	"os"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/python/extension/internal/toolchain"
)

// handleToolchainResolve answers core's reserved toolchain-resolve invocation
// for the lock identities the manifest lists in runtime.resolves. It is called
// before ordinary subcommand dispatch, like the workspace probe, and reports
// handled=false for every other argv. Core runs it with the workspace root as
// its working directory.
func handleToolchainResolve(args []string, stdin io.Reader, stdout io.Writer) (bool, error) {
	return wsproto.ServeToolchainResolve(args, stdin, stdout, func(request wsproto.ToolchainResolveRequest) (wsproto.ToolchainResolveResult, error) {
		root, err := os.Getwd()
		if err != nil {
			return wsproto.ToolchainResolveResult{}, fmt.Errorf("resolve workspace root: %w", err)
		}
		return answerToolchainResolve(root, request)
	})
}

// answerToolchainResolve answers one request against the workspace at root.
// The extension pins uv (toolchain.ResolveUVPin). Any other asked identity is
// answered as a problem, never omitted: an omitted identity tells core the
// lock keeps no pin of it.
func answerToolchainResolve(root string, request wsproto.ToolchainResolveRequest) (wsproto.ToolchainResolveResult, error) {
	result := wsproto.ToolchainResolveResult{
		Version:    wsproto.ToolchainResolveProtocolVersion,
		Extension:  pythonExtensionName,
		Toolchains: make(map[string]wsproto.ToolchainResolution, len(request.Identities)),
	}
	for _, identity := range request.Identities {
		if identity != toolchain.UVLockIdentity {
			result.Toolchains[identity] = wsproto.ToolchainResolution{Problem: &wsproto.ToolchainProblem{
				Message: fmt.Sprintf("%s pins the %s toolchain only, not %q", pythonExtensionName, toolchain.UVLockIdentity, identity),
			}}
			continue
		}
		resolution, err := toolchain.ResolveUVPin(root, request)
		if err != nil {
			return wsproto.ToolchainResolveResult{}, err
		}
		result.Toolchains[identity] = resolution
	}
	return result, nil
}
