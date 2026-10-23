package workspace

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
)

// inputResolveHelperEnv makes this test binary answer one input-resolve
// request in the mode it names (TestInputResolveHelperProcess).
const inputResolveHelperEnv = "PUTNAMI_INPUT_RESOLVE_PROVIDER_HELPER_MODE"

// TestInputResolveHelperProcess is the child. It is a real test so `go test`
// builds it, and it exits before doing anything when the mode variable is
// unset.
func TestInputResolveHelperProcess(t *testing.T) {
	mode := os.Getenv(inputResolveHelperEnv)
	if mode == "" {
		t.Skip("helper process: not invoked as a child")
	}
	switch mode {
	case "answer":
		_, err := wsproto.ServeInputResolve(wsproto.InputResolveControlArgs(), os.Stdin, os.Stdout,
			func(request wsproto.InputResolveRequest) (wsproto.InputResolveResult, error) {
				result := wsproto.InputResolveResult{}
				for _, input := range request.Inputs {
					answer := wsproto.InputResolveAnswer{Project: input.Project, Selector: input.Selector}
					if input.Project == "broken" {
						answer.Error = "embed pattern missing.txt: no matching files"
					} else {
						answer.Files = []string{"assets/a.txt"}
						answer.Basis = wsproto.InputBasis{Content: [][]string{{"assets/*.txt"}}}
					}
					result.Inputs = append(result.Inputs, answer)
				}
				return result, nil
			})
		if err != nil {
			os.Stderr.WriteString(err.Error()) //nolint:errcheck // child process diagnostics
			os.Exit(2)
		}
	case "reordered":
		// A well-formed result that answers the asked inputs in another order,
		// written without the serving side's own check.
		data, err := wsproto.DecodeInputResolveDocument(os.Stdin)
		if err != nil {
			os.Exit(2)
		}
		var request wsproto.InputResolveRequest
		if err := json.Unmarshal(data, &request); err != nil {
			os.Exit(2)
		}
		result := wsproto.InputResolveResult{Version: wsproto.InputResolveProtocolVersion, Extension: request.Extension}
		for _, input := range slices.Backward(request.Inputs) {
			result.Inputs = append(result.Inputs, wsproto.InputResolveAnswer{Project: input.Project, Selector: input.Selector})
		}
		encoded, _ := json.Marshal(result)
		os.Stdout.Write(append(encoded, '\n')) //nolint:errcheck // child process output
	case "hang":
		time.Sleep(30 * time.Second)
	case "fail":
		os.Stderr.WriteString("go.mod: unknown directive") //nolint:errcheck // child process diagnostics
		os.Exit(3)
	}
	os.Exit(0)
}

func inputResolveHelper(t *testing.T, mode string, timeout time.Duration) *ExecProbeProvider {
	t.Helper()
	return &ExecProbeProvider{
		Extension:  "@test/selectors",
		Executable: os.Args[0],
		Args:       []string{"-test.run=^TestInputResolveHelperProcess$"},
		Env:        append(os.Environ(), inputResolveHelperEnv+"="+mode),
		Dir:        t.TempDir(),
		Timeout:    timeout,
	}
}

func inputResolveRequest(extension string) wsproto.InputResolveRequest {
	return wsproto.InputResolveRequest{Extension: extension, Inputs: []wsproto.InputResolveQuery{
		{Project: "app", Selector: "sel:build", Patterns: []string{"src/**"}},
		{Project: "broken", Selector: "sel:build"},
	}}
}

// The provider returns the answer of every asked input in request order, an
// error answer included: the caller decides what an error answer fails.
func TestExecProbeProvider_ResolveInputsRoundTrip(t *testing.T) {
	provider := inputResolveHelper(t, "answer", 30*time.Second)
	result, err := provider.ResolveInputs(inputResolveRequest(provider.Extension))
	if err != nil {
		t.Fatalf("ResolveInputs: %v", err)
	}
	if result.Extension != provider.Extension || len(result.Inputs) != 2 {
		t.Fatalf("result = %+v, want two answers attributed to the asked extension", result)
	}
	if app := result.Inputs[0]; app.Project != "app" || !slices.Equal(app.Files, []string{"assets/a.txt"}) || len(app.Basis.Content) != 1 {
		t.Fatalf("first answer = %+v, want the listed file and its basis", app)
	}
	if broken := result.Inputs[1]; broken.Project != "broken" || !strings.Contains(broken.Error, "no matching files") {
		t.Fatalf("second answer = %+v, want the error answer", broken)
	}
}

// An answer whose inputs are out of request order passes the result's own
// validation and fails the check against the request.
func TestExecProbeProvider_ResolveInputsRejectsAnAnswerToAnotherQuestion(t *testing.T) {
	provider := inputResolveHelper(t, "reordered", 30*time.Second)
	_, err := provider.ResolveInputs(inputResolveRequest(provider.Extension))
	var failure *wsproto.ProbeFailure
	if !errors.As(err, &failure) || failure.Kind != wsproto.ProbeFailureInvalidResult {
		t.Fatalf("error = %v, want an invalid-result failure", err)
	}
	if len(failure.Diagnostics) == 0 || !strings.Contains(failure.Diagnostics[0].Message, `the request asks "app"`) {
		t.Fatalf("diagnostics = %+v, want the misplaced project named", failure.Diagnostics)
	}
}

func TestExecProbeProvider_ResolveInputsFailureCarriesProviderStderr(t *testing.T) {
	provider := inputResolveHelper(t, "fail", 30*time.Second)
	_, err := provider.ResolveInputs(inputResolveRequest(provider.Extension))
	var failure *wsproto.ProbeFailure
	if !errors.As(err, &failure) || failure.Kind != wsproto.ProbeFailureTransport {
		t.Fatalf("error = %v, want a transport failure", err)
	}
	if !strings.Contains(failure.Message, "input-resolve exited with an error") ||
		!strings.Contains(failure.Message, "go.mod: unknown directive") {
		t.Fatalf("message = %q, want the exchange and the provider's stderr", failure.Message)
	}
}

func TestExecProbeProvider_ResolveInputsTimesOut(t *testing.T) {
	provider := inputResolveHelper(t, "hang", 2*time.Second)
	_, err := provider.ResolveInputs(inputResolveRequest(provider.Extension))
	var failure *wsproto.ProbeFailure
	if !errors.As(err, &failure) || failure.Kind != wsproto.ProbeFailureTimeout {
		t.Fatalf("error = %v, want a timeout failure", err)
	}
}

// A request the contract rejects is reported before anything is spawned.
func TestExecProbeProvider_ResolveInputsRefusesAnInvalidRequest(t *testing.T) {
	provider := inputResolveHelper(t, "answer", 30*time.Second)
	request := inputResolveRequest(provider.Extension)
	request.Inputs = nil
	_, err := provider.ResolveInputs(request)
	var failure *wsproto.ProbeFailure
	if !errors.As(err, &failure) || failure.Kind != wsproto.ProbeFailureTransport ||
		!strings.Contains(failure.Message, "at least one input") {
		t.Fatalf("error = %v, want the request refused for asking no input", err)
	}
}
