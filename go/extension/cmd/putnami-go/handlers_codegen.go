//go:build !putnami_runtime_main

package main

import (
	"go.putnami.dev/go/extension/internal/codegen"
	"go.putnami.dev/sdk/extension/cli"

	// Blank-imported visitors register themselves with the codegen package.
	// Add a new generator by introducing a sibling subpackage and listing it
	// here — no other plumbing is required.
	_ "go.putnami.dev/go/extension/internal/codegen/openapi"
)

// codegenHandlers are the jobs of the codegen entry: the runtime's
// compiled/putnami-go-codegen executable, which build-generate and
// build-describe name as their runtimeEntry. The main executable, built with
// the putnami_runtime_main tag, leaves them out (handlers_codegen_off.go).
func codegenHandlers() map[string]cli.JobFunc {
	return map[string]cli.JobFunc{
		"build-generate": codegen.Run,
		"build-describe": codegen.RunDescribe,
	}
}
