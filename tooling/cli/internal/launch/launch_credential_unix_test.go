//go:build !windows

package launch

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

const (
	credentialRoleEnv = "LAUNCH_CREDENTIAL_ROLE"
	credentialRun     = "-test.run=^TestRestartCredentialHelper$"
	credentialBearer  = "launch-run-credential-2b9e"
)

// TestRestartCredentialHelper is not a test: it is one role of the child
// processes TestRestartHandsTheRunCredentialOn starts, and it returns at once
// unless that test started it as one.
func TestRestartCredentialHelper(t *testing.T) {
	switch os.Getenv(credentialRoleEnv) {
	case "restart":
		if err := runcredential.Capture([]string{"upgrade", "--credential-fd", "3"}); err != nil {
			fmt.Println("launch: capture:", err)
			return
		}
		err := Restart(os.Args[0], []string{credentialRun, "--"}, credentialRoleEnv+"=next")
		fmt.Println("launch: restart returned:", err)
	case "next":
		err := runcredential.Capture(os.Args[1:])
		bearer, _ := runcredential.Current()
		sum := sha256.Sum256([]byte(bearer))
		fmt.Printf("launch: err=%v digest=%s args=%s\n", err, hex.EncodeToString(sum[:]), strings.Join(os.Args[1:], " "))
	}
}

func TestRestartHandsTheRunCredentialOn(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-custody", "credential-on-a-descriptor", "relaunch-keeps-the-descriptor")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if _, err := w.WriteString(credentialBearer + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	cmd := exec.Command(os.Args[0], credentialRun)
	cmd.Env = append(os.Environ(), credentialRoleEnv+"=restart")
	cmd.ExtraFiles = []*os.File{r}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run the helper: %v\n%s", err, out)
	}
	sum := sha256.Sum256([]byte(credentialBearer))
	want := "launch: err=<nil> digest=" + hex.EncodeToString(sum[:]) + " args=" + credentialRun + " -- " + runcredential.Flag + "="
	if !strings.Contains(string(out), want) {
		t.Errorf("the restarted image did not read the run credential; want %q in\n%s", want, out)
	}
	if strings.Contains(string(out), credentialBearer) {
		t.Errorf("the credential reached the output:\n%s", out)
	}
}
