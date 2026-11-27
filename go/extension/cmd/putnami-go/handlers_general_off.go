//go:build putnami_runtime_codegen

package main

import "go.putnami.dev/sdk/extension/cli"

// generalHandlers is empty in the codegen entry: every other job runs the main
// executable (handlers_general.go).
func generalHandlers() map[string]cli.JobFunc {
	return map[string]cli.JobFunc{}
}
