package runcredential

import (
	"errors"
	"fmt"
)

// HostedFetchEnv marks the environment of a workspace-fetch job that holds the
// job credential of a hosted run. The engine sets it to "1" when it hands a
// fetch job the credential (the jobs package's attachJobCredential), and a
// Putnami CLI that starts with it set refuses to run (RefuseInHostedFetch).
//
// A fetch built on an older SDK starts `putnami cloud registry-token` with its
// own environment, so the child inherits the marker and the descriptor that
// carries the job credential. The child is the running CLI: the engine names
// it in registry.CLIExecutableEnv, and a hosted run hands the credential to no
// fetch whose environment names another. Its refusal comes before extension
// discovery, so a path or workspace extension that serves the command never
// starts beside the credential.
const HostedFetchEnv = "PUTNAMI_HOSTED_FETCH"

// ErrInHostedFetch is the refusal of a CLI started inside the workspace-fetch
// of a hosted run.
var ErrInHostedFetch = errors.New("a Putnami command cannot run inside the workspace-fetch of a hosted run")

// RefuseInHostedFetch returns an error that wraps ErrInHostedFetch when
// HostedFetchEnv is set to any non-empty value, and nil otherwise. getenv reads
// the process environment. Call it before the process discovers an extension
// or starts anything.
func RefuseInHostedFetch(getenv func(string) string) error {
	if getenv(HostedFetchEnv) == "" {
		return nil
	}
	return fmt.Errorf("%w: %s is set, so this process runs beside the job credential of the fetch that started it, "+
		"and it loads no extension and starts nothing; a current extension's fetch starts no Putnami command, so "+
		"move the extension's pin to a current release with `putnami upgrade`",
		ErrInHostedFetch, HostedFetchEnv)
}
