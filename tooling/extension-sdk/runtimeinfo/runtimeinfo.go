// Package runtimeinfo implements the extension executable handshake.
package runtimeinfo

import (
	"encoding/json"
	"io"
	"runtime"

	protocolcli "go.putnami.dev/protocol/cli"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

const (
	// Verb is the reserved executable-control verb.
	Verb = "__putnami"
	// Command requests the runtime descriptor.
	Command = "runtime-info"
)

// Write writes the canonical runtime descriptor for an extension executable.
func Write(w io.Writer, extension, version string) error {
	return json.NewEncoder(w).Encode(runtimeproto.Info{
		Extension:       extension,
		Version:         version,
		Platform:        runtime.GOOS + "/" + runtime.GOARCH,
		CLIContract:     protocolcli.CurrentContract,
		RuntimeProtocol: runtimeproto.MaxKnownProtocolVersion,
		RuntimeABI:      runtimeproto.RuntimeABIVersion,
	})
}

// Handle writes the descriptor and reports whether args named the reserved
// handshake. Call it before normal subcommand dispatch.
func Handle(args []string, w io.Writer, extension, version string) (bool, error) {
	if len(args) != 2 || args[0] != Verb || args[1] != Command {
		return false, nil
	}
	return true, Write(w, extension, version)
}
