//go:build unix

package main

import (
	"os"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

// credentialDescriptor returns a file holding line, open for reading, the way
// the engine hands workspace-fetch its credential: the child inherits it as
// descriptor 3.
func credentialDescriptor(t *testing.T, line string) *os.File {
	t.Helper()
	path := jobtest.WriteFile(t, t.TempDir(), "credential", line)
	file, err := os.Open(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

// workspace-fetch is registered on the entry point the engine starts, and it
// reads the credential on the descriptor the variable names. The workspace
// declares no Go, so the job ends before any go command, and the bearer
// appears in no event and no line of standard error either way.
func TestWorkspaceFetchWire_ReadsTheCredentialDescriptor(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "hosted-run-dependencies",
		"workspace-fetch-reads-the-credential-descriptor")
	cases := []struct {
		name   string
		line   string
		status runtimeproto.ResultStatus
		code   int
	}{
		{
			name:   "a credential",
			line:   `{"credential":{"bearer":"` + fetchWireBearer + `","expiresAt":"2099-01-01T00:00:00Z","hosts":["modules.example.test"]}}` + "\n",
			status: runtimeproto.ResultOK,
		},
		{name: "no credential", line: "{}\n", status: runtimeproto.ResultOK},
		{
			name:   "an invalid document",
			line:   `{"credential":{"bearer":"` + fetchWireBearer + `"},"x":1}` + "\n",
			status: runtimeproto.ResultFailed,
			code:   1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := jobtest.RealTempDir(t)
			context := writeLifecycleContext(t, ws, "workspace-fetch", `{}`)
			env := fetchWireEnv(t, extensionproto.JobCredentialFDEnv+"=3")
			descriptor := credentialDescriptor(t, tc.line)

			events, stderr, code := runPutnamiGoWithFiles(t, env, []*os.File{descriptor},
				"workspace-fetch", "--putnamiContext", context)
			if code != tc.code {
				t.Fatalf("exit code = %d, want %d\nstderr:\n%s\n%s", code, tc.code, stderr, eventLines(events))
			}
			requireJobStream(t, events, "workspace-fetch", runtimeproto.ProtocolVersion2, tc.status)
			if strings.Contains(eventLines(events), fetchWireBearer) || strings.Contains(stderr, fetchWireBearer) {
				t.Errorf("the bearer reached the job's output:\n%s\n%s", eventLines(events), stderr)
			}
		})
	}
}
