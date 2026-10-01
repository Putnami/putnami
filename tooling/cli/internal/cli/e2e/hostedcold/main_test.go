// Package hostedcold runs a hosted `putnami install` as a real process, from
// a CLI binary that is not the one the workspace pins, against an empty store
// and a registry that serves only the credential the user scope's provider
// derives from the run credential. It builds two CLI binaries from this
// module, so it runs in its own test binary rather than in internal/cli.
//
// A copy of the test binary is also the native runtime of the user scope's
// credential provider: the CLI starts it with the provider variables set, and
// TestMain serves the credential-provider/v1 protocol instead of running the
// tests.
package hostedcold

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	registry "go.putnami.dev/protocol/registry"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

const (
	// providerRecordEnv names the file the provider appends its environment
	// and every request it reads to. A process started with it set is the
	// provider.
	providerRecordEnv = "HOSTEDCOLD_PROVIDER_RECORD"
	// providerHostsEnv is the comma-separated hosts the provider's credential
	// names.
	providerHostsEnv = "HOSTEDCOLD_PROVIDER_HOSTS"
)

// TestMain runs the tests, or, when a CLI under test starts this binary as the
// user scope's credential provider, that provider. As the provider
// extension's runtime, it first answers the CLI's runtime-info handshake.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "__putnami" && os.Args[2] == "runtime-info" {
		os.Exit(answerRuntimeInfo())
	}
	if record := os.Getenv(providerRecordEnv); record != "" {
		os.Exit(serveProvider(record, strings.Split(os.Getenv(providerHostsEnv), ",")))
	}
	os.Exit(clitest.Main(m))
}

// answerRuntimeInfo prints the runtime-info document of the extension whose
// runtime this binary is: the manifest two levels above the executable names
// it.
func answerRuntimeInfo() int {
	executable, err := os.Executable()
	if err != nil {
		return 2
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(executable)), "putnami.extension.json"))
	if err != nil {
		return 2
	}
	var manifest extensionproto.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return 2
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
		return 2
	}
	fmt.Println(string(info))
	return 0
}

// registryBearer is the read bearer the fake registry accepts for a run
// credential. Only the provider computes it, from the run credential it reads
// in initialize.
func registryBearer(runCredential string) string {
	return "reg_" + sha256Hex([]byte("hostedcold registry read "+runCredential))
}

// sha256Hex is the hex SHA-256 of data. A record holds a secret in this form.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// serveProvider serves credential-provider/v1 on the standard streams. It
// answers the read purpose with registryBearer of the run credential
// initialize carried, for hosts, and records its environment and each request
// in record, the run credential as its digest. Without a run credential it
// holds no credential. It returns after shutdown or at the end of its input.
func serveProvider(record string, hosts []string) int {
	file, err := os.OpenFile(record, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 3
	}
	defer func() { _ = file.Close() }()
	note := func(line string) { _, _ = file.WriteString(line + "\n") }
	note("start")
	for _, entry := range os.Environ() {
		note("env " + entry)
	}
	write := func(response registry.CredentialResponse) {
		line, _ := json.Marshal(response)
		_, _ = os.Stdout.Write(append(line, '\n'))
	}
	runCredential := ""
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 4<<10), registry.MaxCredentialLineBytes)
	for scanner.Scan() {
		request, err := registry.ParseCredentialRequest(scanner.Bytes())
		if err != nil {
			note("unparsable request")
			return 4
		}
		switch request.Op {
		case registry.CredentialOpInitialize:
			params, err := registry.ParseCredentialInitializeParams(request.Payload)
			if err != nil {
				note("unparsable initialize")
				return 4
			}
			runCredential = params.RunCredential
			note("initialize run-credential=" + sha256Hex([]byte(runCredential)))
			payload, _ := json.Marshal(registry.CredentialInitializeResult{
				ProtocolVersion: 1, ProviderName: "hostedcold-user-scope", Capabilities: []string{registry.CapabilityCredentialV1},
			})
			write(registry.CredentialResponse{ProtocolVersion: 1, ID: request.ID, OK: true, Payload: payload})
		case registry.CredentialOpCredential:
			params, _ := registry.ParseCredentialParams(request.Payload)
			note("credential " + params.Purpose)
			var result registry.CredentialResult
			if runCredential != "" && params.Purpose == registry.PurposeRead {
				result.Credential = &registry.Credential{
					Bearer:    registryBearer(runCredential),
					ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
					Hosts:     hosts,
				}
			}
			payload, _ := json.Marshal(result)
			write(registry.CredentialResponse{ProtocolVersion: 1, ID: request.ID, OK: true, Payload: payload})
		case registry.CredentialOpShutdown:
			note("shutdown")
			write(registry.CredentialResponse{ProtocolVersion: 1, ID: request.ID, OK: true})
			return 0
		}
	}
	note("eof")
	return 0
}
