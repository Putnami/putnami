//go:build putnami_runtime_main

package main

import "go.putnami.dev/sdk/extension/cli"

// codegenHandlers is empty in the main executable: build-generate and
// build-describe run the codegen entry (handlers_codegen.go).
func codegenHandlers() map[string]cli.JobFunc {
	return nil
}
