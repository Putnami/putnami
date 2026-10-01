// Package sdk is the umbrella package for the Putnami extension SDK.
//
// Actual code lives in subpackages: cli (extension entrypoint helpers),
// codegen (code generation utilities), context (Putnami extension run
// context), exec (process exec wrapper), and jsonl (jsonl I/O helpers
// used by the extension protocol). This file exists so the root
// package is a valid Go package — the workspace's cross-compile job
// expects every Go-tagged project to have at least one root .go file
// so it can build for foreign targets without "no Go files" errors.
package sdk
