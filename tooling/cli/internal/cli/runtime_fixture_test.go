package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

// fixtureRuntimeExecutable is where a fixture extension of this package keeps
// its runtime, a copy of this test binary.
const fixtureRuntimeExecutable = "compiled/provider"

// answerRuntimeHandshake answers the CLI's runtime-info handshake when this
// test binary runs as the runtime of a fixture extension: the arguments are
// `__putnami runtime-info`, and the manifest two levels above the executable
// names the extension. It reports whether it answered; TestMain then exits.
func answerRuntimeHandshake() bool {
	if len(os.Args) != 3 || os.Args[1] != "__putnami" || os.Args[2] != "runtime-info" {
		return false
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(executable)), "putnami.extension.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var manifest extensionproto.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	info, err := json.Marshal(runtimeproto.Info{
		Extension:       manifest.Name,
		Version:         manifest.Version,
		Platform:        runtime.GOOS + "/" + runtime.GOARCH,
		CLIContract:     protocolcli.CurrentContract,
		RuntimeProtocol: runtimeproto.MaxKnownProtocolVersion,
		RuntimeABI:      runtimeproto.RuntimeABIVersion,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Println(string(info))
	return true
}
