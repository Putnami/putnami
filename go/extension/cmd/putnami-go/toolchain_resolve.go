package main

import (
	"fmt"
	"io"
	"os"

	"go.putnami.dev/go/extension/internal/workspacejob"
	wsproto "go.putnami.dev/protocol/workspace"
)

// handleToolchainResolve answers core's reserved toolchain-resolve invocation
// for the lock identities the manifest lists in runtime.resolves. It is called
// before ordinary subcommand dispatch, exactly like the workspace probe, and
// reports handled=false for every other argv.
//
// Core spawns it with the workspace root as its working directory. getenv
// locates the Putnami home that holds the machine's Go pin records. The
// answer is attributed to this extension, so a request routed to another
// extension's name is rejected rather than answered.
func handleToolchainResolve(args []string, stdin io.Reader, stdout io.Writer, getenv func(string) string) (bool, error) {
	return wsproto.ServeToolchainResolve(args, stdin, stdout, func(request wsproto.ToolchainResolveRequest) (wsproto.ToolchainResolveResult, error) {
		root, err := os.Getwd()
		if err != nil {
			return wsproto.ToolchainResolveResult{}, fmt.Errorf("resolve workspace root: %w", err)
		}
		result, err := workspacejob.AnswerToolchainResolve(root, request, getenv)
		if err != nil {
			return wsproto.ToolchainResolveResult{}, err
		}
		result.Extension = goExtensionName
		return result, nil
	})
}
