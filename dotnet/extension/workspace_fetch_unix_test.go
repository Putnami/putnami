//go:build unix

package dotnet

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	registry "go.putnami.dev/protocol/registry"
)

// The workspace-fetch process of TestAHandedCredentialReachesNoProcessOfTheFetch
// restores the projects of the workspace fetchHelperRootEnv names and writes
// its status to the file fetchHelperResultEnv names.
const (
	fetchHelperRootEnv   = "PUTNAMI_DOTNET_FETCH_HELPER_ROOT"
	fetchHelperResultEnv = "PUTNAMI_DOTNET_FETCH_HELPER_RESULT"
)

// TestWorkspaceFetchDescriptorHelper is the workspace-fetch process that
// TestAHandedCredentialReachesNoProcessOfTheFetch starts, with the production
// credential read. It does nothing in any other run.
func TestWorkspaceFetchDescriptorHelper(t *testing.T) {
	root := os.Getenv(fetchHelperRootEnv)
	if root == "" {
		return
	}
	status, err := runWorkspaceJob(t, root, WorkspaceFetchJob(stack(t)), nil)
	result := status
	if err != nil {
		result += " " + err.Error()
	}
	if err := os.WriteFile(os.Getenv(fetchHelperResultEnv), []byte(result), 0o600); err != nil {
		t.Fatal(err)
	}
}

// credentialDescriptor returns an open file that holds the credential
// document line, and no path names it any more.
func credentialDescriptor(t *testing.T, line []byte) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "credential")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if _, err := file.Write(line); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file.Name()); err != nil {
		t.Fatal(err)
	}
	return file
}

// The engine hands workspace-fetch its credential on an inherited descriptor,
// which carries no close-on-exec. The job reads it before the SDK check, so no
// dotnet it starts inherits the descriptor or finds the variable or the bearer
// in its environment.
func TestAHandedCredentialReachesNoProcessOfTheFetch(t *testing.T) {
	spectest.Proves(t, feature, "fetch-reads-the-credential-first", "workspace-fetch-reads-the-credential-before-any-process")
	root := fixture(t)
	log := stubDotnet(t, "8.0.404", trxPass)
	document, err := json.Marshal(registry.CredentialResult{Credential: &registry.Credential{
		Bearer: fetchBearer, ExpiresAt: "2099-01-01T00:00:00Z", Hosts: []string{"registry.example.test"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(t.TempDir(), "result")
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkspaceFetchDescriptorHelper$", "-test.count=1") //nolint:gosec // this test binary
	cmd.Env = append(os.Environ(),
		fetchHelperRootEnv+"="+root,
		fetchHelperResultEnv+"="+resultPath,
		extensionproto.JobCredentialFDEnv+"="+strconv.Itoa(credentialFD),
	)
	cmd.ExtraFiles = make([]*os.File, credentialFD-2)
	cmd.ExtraFiles[credentialFD-3] = credentialDescriptor(t, append(document, '\n'))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("workspace-fetch process: %v\n%s", err, out)
	}
	if result, err := os.ReadFile(resultPath); err != nil || string(result) != "OK" {
		t.Fatalf("workspace-fetch = %q (%v)\n%s", result, err, out)
	}
	if strings.Contains(string(out), fetchBearer) {
		t.Errorf("the workspace-fetch output holds the bearer:\n%s", out)
	}
	if got := readLines(t, log); len(got) != 3 || !strings.Contains(got[0], ": dotnet restore ") {
		t.Errorf("ran %q", got)
	}
	record := started(t, log)
	if !strings.Contains(record, "dotnet --version") || !strings.Contains(record, "dotnet restore") {
		t.Fatalf("the stub recorded no SDK check and no restore:\n%s", record)
	}
	for _, leak := range []string{inheritedDescriptor, extensionproto.JobCredentialFDEnv + "=", fetchBearer} {
		if strings.Contains(record, leak) {
			t.Errorf("a process of the fetch saw %q:\n%s", leak, record)
		}
	}
}
