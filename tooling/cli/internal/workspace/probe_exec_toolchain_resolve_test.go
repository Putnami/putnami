package workspace

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// toolchainResolveHelperEnv makes this test binary answer one toolchain-resolve
// request in the mode it names (TestToolchainResolveHelperProcess).
const toolchainResolveHelperEnv = "PUTNAMI_TOOLCHAIN_RESOLVE_HELPER_MODE"

// helperGoPin is the pin the helper answers for "go" on the request's platform.
func helperGoPin(platform string) *wsproto.LockToolchainEntry {
	return &wsproto.LockToolchainEntry{
		Version:     "1.26.1",
		Integrities: map[string]string{platform: strings.Repeat("a", 64)},
		Archives: map[string]wsproto.LockToolchainArchive{platform: {
			URL: "https://example.test/go1.26.1.tar.gz", Format: wsproto.LockArchiveFormatTarGz, Executable: "go/bin/go",
		}},
	}
}

// TestToolchainResolveHelperProcess is the child. It is a real test so
// `go test` builds it, and it exits before doing anything when the mode
// variable is unset.
func TestToolchainResolveHelperProcess(t *testing.T) {
	mode := os.Getenv(toolchainResolveHelperEnv)
	if mode == "" {
		t.Skip("helper process: not invoked as a child")
	}
	switch mode {
	case "answer":
		_, err := wsproto.ServeToolchainResolve(wsproto.ToolchainResolveControlArgs(), os.Stdin, os.Stdout,
			func(request wsproto.ToolchainResolveRequest) (wsproto.ToolchainResolveResult, error) {
				return wsproto.ToolchainResolveResult{Toolchains: map[string]wsproto.ToolchainResolution{
					"go": {Pin: helperGoPin(request.Platform)},
				}}, nil
			})
		if err != nil {
			os.Stderr.WriteString(err.Error()) //nolint:errcheck // child process diagnostics
			os.Exit(2)
		}
	case "unasked":
		// A well-formed result that resolves an identity the request does not
		// ask for, written without the serving side's own check.
		data, err := wsproto.DecodeToolchainResolveDocument(os.Stdin)
		if err != nil {
			os.Exit(2)
		}
		var request wsproto.ToolchainResolveRequest
		if err := json.Unmarshal(data, &request); err != nil {
			os.Exit(2)
		}
		encoded, _ := json.Marshal(wsproto.ToolchainResolveResult{
			Version:   wsproto.ToolchainResolveProtocolVersion,
			Extension: request.Extension,
			Toolchains: map[string]wsproto.ToolchainResolution{
				"bun": {Pin: helperGoPin(request.Platform)},
			},
		})
		os.Stdout.Write(append(encoded, '\n')) //nolint:errcheck // child process output
	case "fail":
		os.Stderr.WriteString("go.work: unknown directive") //nolint:errcheck // child process diagnostics
		os.Exit(3)
	}
	os.Exit(0)
}

func toolchainResolveHelper(t *testing.T, mode string) *ExecProbeProvider {
	t.Helper()
	return &ExecProbeProvider{
		Extension:  "@putnami/go",
		Executable: os.Args[0],
		Args:       []string{"-test.run=^TestToolchainResolveHelperProcess$"},
		Env:        append(os.Environ(), toolchainResolveHelperEnv+"="+mode),
		Dir:        t.TempDir(),
		Timeout:    30 * time.Second,
	}
}

func toolchainResolveRequest(extension string) wsproto.ToolchainResolveRequest {
	return wsproto.ToolchainResolveRequest{Extension: extension, Identities: []string{"go"}, Platform: "linux/amd64"}
}

func TestExecProbeProvider_ResolveToolchainsRoundTrip(t *testing.T) {
	provider := toolchainResolveHelper(t, "answer")
	result, err := provider.ResolveToolchains(toolchainResolveRequest(provider.Extension))
	if err != nil {
		t.Fatalf("ResolveToolchains: %v", err)
	}
	pin := result.Toolchains["go"].Pin
	if result.Extension != provider.Extension || pin == nil || pin.Version != "1.26.1" ||
		pin.Archives["linux/amd64"].Executable != "go/bin/go" {
		t.Fatalf("result = %+v, want the go pin the helper answers for linux/amd64", result)
	}
}

// An answer that resolves an identity the request did not ask for passes the
// result's own validation and fails the check against the request.
func TestExecProbeProvider_ResolveToolchainsRejectsAnAnswerToAnotherQuestion(t *testing.T) {
	provider := toolchainResolveHelper(t, "unasked")
	_, err := provider.ResolveToolchains(toolchainResolveRequest(provider.Extension))
	var failure *wsproto.ProbeFailure
	if !errors.As(err, &failure) || failure.Kind != wsproto.ProbeFailureInvalidResult {
		t.Fatalf("error = %v, want an invalid-result failure", err)
	}
	if len(failure.Diagnostics) == 0 || !strings.Contains(failure.Diagnostics[0].Message, `"bun"`) {
		t.Fatalf("diagnostics = %+v, want the unasked identity named", failure.Diagnostics)
	}
}

func TestExecProbeProvider_ResolveToolchainsFailureCarriesProviderStderr(t *testing.T) {
	provider := toolchainResolveHelper(t, "fail")
	_, err := provider.ResolveToolchains(toolchainResolveRequest(provider.Extension))
	var failure *wsproto.ProbeFailure
	if !errors.As(err, &failure) || failure.Kind != wsproto.ProbeFailureTransport {
		t.Fatalf("error = %v, want a transport failure", err)
	}
	if !strings.Contains(failure.Message, "toolchain-resolve exited with an error") ||
		!strings.Contains(failure.Message, "go.work: unknown directive") {
		t.Fatalf("message = %q, want the exchange and the provider's stderr", failure.Message)
	}
}

// A request the contract rejects is reported before anything is spawned.
func TestExecProbeProvider_ResolveToolchainsRefusesAnInvalidRequest(t *testing.T) {
	provider := toolchainResolveHelper(t, "answer")
	request := toolchainResolveRequest(provider.Extension)
	request.Identities = nil
	_, err := provider.ResolveToolchains(request)
	var failure *wsproto.ProbeFailure
	if !errors.As(err, &failure) || failure.Kind != wsproto.ProbeFailureTransport ||
		!strings.Contains(failure.Message, "at least one identity") {
		t.Fatalf("error = %v, want the request refused for asking no identity", err)
	}
}

// The toolchain-resolve call starts the extension's executable under the
// probe's custody rule: unless the runtime was prepared before the run, it is
// repository code, recorded before the spawn.
func TestExecProbeProvider_ResolveToolchainsRecordsRepositoryCodeOutsideTheStore(t *testing.T) {
	for name, c := range map[string]struct {
		preparedBeforeRun bool
		records           bool
	}{
		"repository code":         {records: true},
		"prepared before the run": {preparedBeforeRun: true},
	} {
		restore := runcredential.SetForTest("run-bearer")
		provider := &ExecProbeProvider{
			Extension:         "@acme/pins",
			Executable:        filepath.Join(t.TempDir(), "does-not-exist"),
			PreparedBeforeRun: c.preparedBeforeRun,
		}
		_, err := provider.ResolveToolchains(toolchainResolveRequest(provider.Extension))
		var failure *wsproto.ProbeFailure
		if !errors.As(err, &failure) || failure.Kind != wsproto.ProbeFailureUnavailable {
			t.Errorf("%s: error = %v, want the missing executable unavailable", name, err)
		}
		err = runcredential.RequireCustody("the cache provider of @acme/cache")
		restore()
		if recorded := err != nil; recorded != c.records {
			t.Errorf("%s: repository code recorded = %v (%v), want %v", name, recorded, err, c.records)
		}
	}
}
