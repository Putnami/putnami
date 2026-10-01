package main

import (
	"encoding/json"

	pctx "go.putnami.dev/sdk/extension/context"
)

// This extension's own metadata block, as read back from the job context.
//
// The block is produced by this extension's workspace probe (probeMetadataFor)
// and delivered under `project.metadata["@putnami/typescript"]`. The round trip
// is closed HERE: one place writes the shape, one place reads it, and the two
// are pinned together by the round-trip test.
//
// Reading only this extension's own namespace is a contract, not a courtesy. The
// complete metadata map travels on the wire — a Go project that also carries a
// package.json has both providers' blocks — so a consumer that scanned every
// namespace would start depending on facts another extension owns and is free to
// change.

// workspaceMetadata is the subset of this extension's probe metadata that job
// code consumes.
type workspaceMetadata struct {
	// Main is package.json's `main`, when declared.
	Main string `json:"main"`
	// Bin is package.json's `bin`, kept raw because it is a string or an object.
	Bin json.RawMessage `json:"bin"`
	// Exports is package.json's `exports` map, kept raw because its values are
	// strings or condition objects.
	Exports json.RawMessage `json:"exports"`
}

// workspaceMetadataOf decodes this extension's namespaced block from a job
// context. Every absence is an ordinary answer: a project with no metadata, a
// context from a run whose probe reported nothing for it, and a malformed block
// all yield the zero value rather than an error, because none of them is a
// reason to fail a build that has other ways to find its entrypoint.
func workspaceMetadataOf(ctx *pctx.Context) workspaceMetadata {
	if ctx == nil || len(ctx.Project.Metadata) == 0 {
		return workspaceMetadata{}
	}
	raw, ok := ctx.Project.Metadata[tsExtensionName]
	if !ok || len(raw) == 0 {
		return workspaceMetadata{}
	}
	var metadata workspaceMetadata
	if json.Unmarshal(raw, &metadata) != nil {
		return workspaceMetadata{}
	}
	return metadata
}

// BinString returns the `bin` value when it is a single string. An object form
// (`{"cli": "./dist/cli.js"}`) names several executables and cannot stand in for
// one entrypoint, so it yields "".
func (m workspaceMetadata) BinString() string {
	if len(m.Bin) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(m.Bin, &s) != nil {
		return ""
	}
	return s
}
