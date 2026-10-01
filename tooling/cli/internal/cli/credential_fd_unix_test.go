//go:build !windows

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/runnerprovider"
)

const (
	appCredentialRoleEnv = "CLI_CREDENTIAL_HELPER_ARGS"
	appCredentialRun     = "-test.run=^TestAppRunCredentialHelper$"
	appCredentialBearer  = "app-run-credential-c41d"
)

// TestAppRunCredentialHelper is not a test: it runs App.Run with the
// arguments of CLI_CREDENTIAL_HELPER_ARGS, the way cmd/putnami does, in a
// child of this test binary started by the tests below, and reports what the
// process holds afterwards. It returns at once unless a test started it.
func TestAppRunCredentialHelper(t *testing.T) {
	t.Parallel()
	raw := os.Getenv(appCredentialRoleEnv)
	if raw == "" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	code := (&App{}).Run(context.Background(), args)
	bearer, held := runcredential.Current()
	sum := sha256.Sum256([]byte(bearer))
	_, token := os.LookupEnv(runcredential.CacheTokenEnv)
	_, err := unix.FcntlInt(3, unix.F_GETFD, 0)
	fmt.Printf("app-credential: exit=%d held=%v digest=%s cache-token=%v descriptor-closed=%v\n",
		code, held, hex.EncodeToString(sum[:]), token, errors.Is(err, unix.EBADF))
}

func runAppWithCredential(t *testing.T, args []string, content string, env ...string) (stdout, stderr string) {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	go func() {
		_, _ = w.WriteString(content)
		_ = w.Close()
	}()
	cmd := exec.Command(os.Args[0], appCredentialRun)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, runcredential.CacheTokenEnv+"=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, appCredentialRoleEnv+"="+string(encoded))
	cmd.Env = append(cmd.Env, env...)
	cmd.ExtraFiles = []*os.File{r}
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("run the helper: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errOut.String())
	}
	if strings.Contains(out.String(), appCredentialBearer) || strings.Contains(errOut.String(), appCredentialBearer) {
		t.Errorf("the credential reached the output\nstdout:\n%s\nstderr:\n%s", out.String(), errOut.String())
	}
	return out.String(), errOut.String()
}

func TestAppRunCapturesTheRunCredential(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-custody", "credential-on-a-descriptor", "descriptor-read-once-and-closed")
	const token = "cache-token-value-a07b"
	stdout, stderr := runAppWithCredential(t, []string{"--version", "--credential-fd", "3"}, appCredentialBearer+"\n",
		runcredential.CacheTokenEnv+"="+token)
	sum := sha256.Sum256([]byte(appCredentialBearer))
	want := "app-credential: exit=0 held=true digest=" + hex.EncodeToString(sum[:]) + " cache-token=false descriptor-closed=true"
	if !strings.Contains(stdout, "putnami "+Version) || !strings.Contains(stdout, want) {
		t.Errorf("stdout = %q, want the version and %q", stdout, want)
	}
	if !strings.Contains(stderr, "removed PUTNAMI_CACHE_TOKEN from the environment") || strings.Contains(stderr, token) {
		t.Errorf("stderr = %q, want the cache-token notice without the token", stderr)
	}
}

func TestAppRunRefusesAnUnusableCredential(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-custody", "credential-on-a-descriptor", "unusable-descriptor-is-a-usage-error")
	for name, tc := range map[string]struct {
		args    []string
		content string
		want    string
	}{
		"whitespace": {[]string{"--version", "--credential-fd=3"}, appCredentialBearer + " x", "putnami: --credential-fd 3: the credential is empty, holds whitespace, or is not UTF-8\n"},
		"twice":      {[]string{"--version", "--credential-fd=3", "--credential-fd", "3"}, appCredentialBearer, "putnami: --credential-fd is given 2 times: pass the run credential on one descriptor\n"},
		"stdout":     {[]string{"--version", "--credential-fd", "1"}, appCredentialBearer, "putnami: invalid --credential-fd value 1: descriptors 0, 1 and 2 are the standard streams\n"},
	} {
		stdout, stderr := runAppWithCredential(t, tc.args, tc.content)
		if !strings.Contains(stdout, "app-credential: exit=2 held=false") || strings.Contains(stdout, "putnami "+Version) {
			t.Errorf("%s: stdout = %q, want a usage exit before the version", name, stdout)
		}
		if stderr != tc.want {
			t.Errorf("%s: stderr = %q, want %q", name, stderr, tc.want)
		}
	}
}

// A runner provider launches the executing engine of a bound request with the
// run credential's descriptor as its one argument. The adapter accepts it and
// refuses any other.
func TestBoundRequestAcceptsOnlyTheCredentialDescriptor(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-custody", "credential-on-a-descriptor", "bound-request-accepts-only-the-descriptor")
	request := runnerprovider.BoundRequestEnv + "=" + t.TempDir() + "/request.json"
	stdout, stderr := runAppWithCredential(t, []string{"--credential-fd", "3"}, appCredentialBearer+"\n", request)
	if !strings.Contains(stdout, "held=true") || strings.Contains(stderr, "not accepted") ||
		!strings.Contains(stderr, "bound execution request needs a workspace") {
		t.Errorf("descriptor alone: stdout = %q, stderr = %q, want the workspace refusal past the argument check", stdout, stderr)
	}
	stdout, stderr = runAppWithCredential(t, []string{"build", "--credential-fd", "3"}, appCredentialBearer+"\n", request)
	if !strings.Contains(stdout, "app-credential: exit=2") || !strings.Contains(stderr, "arguments are not accepted beside it, except --credential-fd") {
		t.Errorf("an argument beside the descriptor: stdout = %q, stderr = %q, want a usage refusal", stdout, stderr)
	}
}
