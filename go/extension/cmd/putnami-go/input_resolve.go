package main

import (
	"fmt"
	"io"
	"os"

	"go.putnami.dev/go/extension/internal/inputresolve"
	wsproto "go.putnami.dev/protocol/workspace"
)

// handleInputResolve answers core's reserved input-resolve invocation for the
// input selectors the manifest lists in runtime.inputSelectors: go-embed:build
// and go-embed:test. It is called before ordinary subcommand dispatch, like
// the workspace probe, and reports handled=false for every other argv.
//
// Core spawns it with the workspace root as its working directory; each asked
// project is relative to that root. The answer is attributed to this
// extension, so a request routed to another extension's name is rejected
// rather than answered.
func handleInputResolve(args []string, stdin io.Reader, stdout io.Writer) (bool, error) {
	return wsproto.ServeInputResolve(args, stdin, stdout, func(request wsproto.InputResolveRequest) (wsproto.InputResolveResult, error) {
		root, err := os.Getwd()
		if err != nil {
			return wsproto.InputResolveResult{}, fmt.Errorf("resolve workspace root: %w", err)
		}
		result := inputresolve.Resolve(root, request)
		result.Extension = goExtensionName
		return result, nil
	})
}
