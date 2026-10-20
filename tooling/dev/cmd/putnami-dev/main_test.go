package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path"
	"runtime"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

// TestRunAnswersTheRuntimeHandshake pins the descriptor a host compares with
// the manifest before it runs anything from this executable, once the manifest
// declares a runtime.
func TestRunAnswersTheRuntimeHandshake(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "handshake-only-runtime", "the-runtime-answers-the-handshake")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"__putnami", "runtime-info"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	decoder := json.NewDecoder(&stdout)
	decoder.DisallowUnknownFields()
	var info runtimeproto.Info
	if err := decoder.Decode(&info); err != nil {
		t.Fatalf("runtime-info is not one descriptor: %v", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		t.Fatalf("runtime-info carries trailing data: %v", err)
	}
	want := runtimeproto.Info{
		Extension:       readManifest(t).Name,
		Version:         runtimeVersion,
		Platform:        runtime.GOOS + "/" + runtime.GOARCH,
		CLIContract:     protocolcli.CurrentContract,
		RuntimeProtocol: runtimeproto.MaxKnownProtocolVersion,
		RuntimeABI:      runtimeproto.RuntimeABIVersion,
	}
	if info != want {
		t.Errorf("runtime-info %+v, want %+v", info, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("runtime-info wrote to stderr: %q", stderr.String())
	}
}

// TestRunRefusesEveryOtherInvocation pins that the runtime does nothing
// beyond the handshake and the version commands: any other argument list, a
// verb of the vocabulary included, exits 2 and writes nothing to stdout.
func TestRunRefusesEveryOtherInvocation(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "handshake-only-runtime", "every-other-invocation-is-refused")
	for _, args := range [][]string{
		nil,
		{"build"},
		{"publish"},
		{"__putnami"},
		{"__putnami", "runtime-info", "extra"},
		{"version"},
		{"version", "set"},
		{"get", "version"},
	} {
		t.Run(path.Join(append([]string{"args"}, args...)...), func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 2 {
				t.Errorf("exit %d, want 2", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout %q, want nothing", stdout.String())
			}
			if !strings.Contains(stderr.String(), "unknown invocation") {
				t.Errorf("stderr %q does not name the refusal", stderr.String())
			}
		})
	}
}

// TestRunReportsAHandshakeItCannotWrite pins that a runtime-info descriptor
// the runtime cannot write is a failure the host sees: exit 1 and the write
// error on stderr, never a silent success.
func TestRunReportsAHandshakeItCannotWrite(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "handshake-only-runtime", "a-handshake-it-cannot-write-fails")
	var stderr bytes.Buffer
	if code := run([]string{"__putnami", "runtime-info"}, failingWriter{}, &stderr); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), errClosedStdout.Error()) {
		t.Errorf("stderr %q does not name the write error", stderr.String())
	}
}

var errClosedStdout = errors.New("stdout is closed")

// failingWriter refuses every write, as a closed stdout does.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errClosedStdout }
