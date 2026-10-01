// Command putnami-scaffold is the binary for the @putnami/scaffold extension. It
// packages content projects into distributable archives: project templates
// (putnami.template.json) that `putnami new` consumes, and content-only
// extensions (a putnami.extension.json that declares agentContent) whose agent
// content `putnami install` materializes into a workspace. Both are
// cross-language concerns, so they live in their own extension rather than in
// any single language ecosystem.
//
// Usage: putnami-scaffold <subcommand> [flags]
//
// Subcommands:
//
//	package-content     Package a template or a content-only extension into a distributable archive
package main

import (
	"go.putnami.dev/sdk/extension/cli"

	"go.putnami.dev/tooling/scaffold/extension/internal/packaging"
)

func main() {
	cli.RunSubcommand(map[string]cli.JobFunc{
		"package-content": packaging.Content,
	}, cli.WithSkipEmptyProject(false))
}
