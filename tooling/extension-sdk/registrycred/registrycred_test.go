package registrycred

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	registryproto "go.putnami.dev/protocol/registry"
)

func TestResolveTokenWithCLI_BootstrapsWithoutChangingTheParentOrConsultingPATH(t *testing.T) {
	spectest.Proves(t, "registry/credential-seam-contract", "the-seam-is-pinned", "the-extension-sdk-passes-only-the-host")
	args := filepath.Join(t.TempDir(), "argv")
	executable := fakeCLIExecutable(t, fakeCLI{
		RequireEnv: map[string]string{"PUTNAMI_NO_RELAUNCH": "1"},
		LinesFile:  args,
		Stdout:     "pkt_bootstrap",
	})
	t.Setenv("PATH", t.TempDir())
	t.Setenv(registryproto.CLIExecutableEnv, "/must-not-select-the-advertised-cli")
	t.Setenv("PUTNAMI_NO_RELAUNCH", "0")
	token, hint := ResolveTokenWithCLI(context.Background(), "put.example.test", executable)
	if token != "pkt_bootstrap" || hint != "" {
		t.Fatalf("token = %q, hint = %q", token, hint)
	}
	recorded, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(recorded), "cloud\nregistry-token\n--host\nput.example.test\n"; got != want {
		t.Fatalf("credential command = %q, want host-only %q", got, want)
	}
	if os.Getenv("PUTNAMI_NO_RELAUNCH") != "0" {
		t.Fatal("obtaining a credential disabled the parent's workspace pin")
	}
}

func TestResolveTokenWithCLI_RefusesRelativeExecutableAndHonorsCancellation(t *testing.T) {
	if token, hint := ResolveTokenWithCLI(context.Background(), "put.example.test", "putnami"); token != "" || hint == "" {
		t.Fatalf("relative executable must be refused: token=%q hint=%q", token, hint)
	}
	executable := fakeCLIExecutable(t, fakeCLI{Sleep: 30 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if token, _ := ResolveTokenWithCLI(ctx, "put.example.test", executable); token != "" {
		t.Fatal("canceled credential request returned a token")
	}
	if ctx.Err() == nil || time.Since(started) > 5*time.Second {
		t.Fatal("credential resolution did not honor the caller's cancellation")
	}
}

// A credential helper the cloud CLI starts and leaves behind holds the
// child's output pipes; the bearer request still ends at its deadline plus
// credentialWaitDelay, with no token.
func TestResolveTokenWithCLI_AStuckHelperCannotHoldTheCallPastItsDeadline(t *testing.T) {
	executable := fakeCLIExecutable(t, stuckHelperCLI(t))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	token, _ := ResolveTokenWithCLI(ctx, "put.example.test", executable)
	elapsed := time.Since(started)
	if token != "" {
		t.Fatalf("token = %q, want none from a timed-out request", token)
	}
	if elapsed > 5*time.Second {
		t.Errorf("ResolveTokenWithCLI returned after %v, want the deadline plus %v", elapsed, credentialWaitDelay)
	}
}

func TestResolveTokenCLI_FallbackRetainsLauncherPolicy(t *testing.T) {
	t.Setenv("PUTNAMI_NO_RELAUNCH", "")
	fakeCloudCLI(t, fakeCLI{RefuseEnv: map[string]string{"PUTNAMI_NO_RELAUNCH": "1"}, Stdout: "pkt_live_token\n"})
	tok, hint := resolveTokenCLI("oci.putnami.dev")
	if tok != "pkt_live_token" {
		t.Errorf("token = %q, want pkt_live_token", tok)
	}
	if hint != "" {
		t.Errorf("hint = %q, want empty on success", hint)
	}
}

// TestResolveTokenCLI_PassesTheHostAndNothingElse pins the host-only seam. The
// publisher and the installer make the SAME call; a package, an action, or an
// owning workspace on this command line is a cross-repo break.
func TestResolveTokenCLI_PassesTheHostAndNothingElse(t *testing.T) {
	spectest.Proves(t, "registry/credential-seam-contract", "the-seam-is-pinned", "the-extension-sdk-passes-only-the-host")
	recorded := filepath.Join(t.TempDir(), "argv")
	fakeCloudCLI(t, fakeCLI{WordsFile: recorded, Stdout: "pkt_host_only\n"})

	if tok, hint := resolveTokenCLI("go.putnami.dev"); tok != "pkt_host_only" || hint != "" {
		t.Fatalf("token = %q, hint = %q", tok, hint)
	}
	argv, err := os.ReadFile(recorded)
	if err != nil {
		t.Fatal(err)
	}
	want := registryproto.SeamParentCommand + " " + registryproto.SeamSubcommand +
		" --" + registryproto.SeamHostFlag + " go.putnami.dev"
	if string(argv) != want {
		t.Errorf("argv = %q, want %q", argv, want)
	}
}

func TestResolveTokenCLI_FailureReturnsHint(t *testing.T) {
	spectest.Proves(t, "registry/credential-seam-contract", "a-seam-token-is-a-bare-bearer", "the-extension-sdk-relays-the-stderr-of-a-failure")
	recordedCloudCLI(t, recordedExchange(t, "not-authenticated"))
	tok, hint := resolveTokenCLI("oci.putnami.dev")
	if tok != "" {
		t.Errorf("token = %q, want empty on failure", tok)
	}
	if hint != "not authenticated; run putnami cloud login" {
		t.Errorf("hint = %q, want the cloud's stderr relayed", hint)
	}
}

// The settled answer is one production-sized bearer on stdout: a 1011-byte
// RS256 JWT, returned without its trailing newline.
func TestResolveTokenCLI_SettledAnswerIsTheWholeBearer(t *testing.T) {
	spectest.Proves(t, "registry/credential-seam-contract", "a-seam-token-is-a-bare-bearer", "the-extension-sdk-returns-the-whole-bearer")
	settled := recordedExchange(t, "settled")
	recordedCloudCLI(t, settled)
	tok, hint := resolveTokenCLI("put.putnami.dev")
	if hint != "" || tok != strings.TrimSpace(string(settled.Stdout())) || len(tok) != 1011 {
		t.Fatalf("token of %d bytes, hint = %q; want the recorded 1011-byte bearer", len(tok), hint)
	}
}

func TestResolveTokenCLI_MissingBinaryIsSilent(t *testing.T) {
	spectest.Proves(t, "registry/credential-seam-contract", "a-seam-token-is-a-bare-bearer", "the-extension-sdk-is-silent-without-an-answer")
	t.Setenv(registryproto.CLIExecutableEnv, "")
	prev := cloudCLIName
	t.Cleanup(func() { cloudCLIName = prev })
	cloudCLIName = filepath.Join(t.TempDir(), "does-not-exist")
	tok, hint := resolveTokenCLI("oci.putnami.dev")
	if tok != "" || hint != "" {
		t.Errorf("a missing cloud binary must be silent: token=%q hint=%q", tok, hint)
	}
}

func TestResolveTokenCLI_UsesAdvertisedCLIExecutableWithoutWorkspacePinRelaunch(t *testing.T) {
	spectest.Proves(t, "registry/credential-seam-contract", "the-seam-is-pinned", "the-extension-sdk-uses-the-advertised-cli")
	// Without PUTNAMI_NO_RELAUNCH=1 the advertised CLI would relaunch through
	// the workspace pin.
	cliPath := fakeCLIExecutable(t, fakeCLI{
		RequireEnv: map[string]string{"PUTNAMI_NO_RELAUNCH": "1"},
		Stdout:     "pkt_local_cli\n",
	})
	t.Setenv("PATH", t.TempDir())
	t.Setenv("PUTNAMI_NO_RELAUNCH", "")
	t.Setenv(registryproto.CLIExecutableEnv, cliPath)
	previous := cloudCLIName
	cloudCLIName = "putnami"
	t.Cleanup(func() { cloudCLIName = previous })

	token, hint := resolveTokenCLI("npm.putnami.dev")
	if token != "pkt_local_cli" || hint != "" {
		t.Fatalf("token = %q, hint = %q; want advertised CLI executable used without PATH", token, hint)
	}
}

func TestResolveTokenCLI_EmptyHost(t *testing.T) {
	spectest.Proves(t, "registry/credential-seam-contract", "the-seam-is-pinned", "the-extension-sdk-answers-nothing-for-an-empty-host")
	// No host → no resolution attempt at all (the binary is never invoked).
	t.Setenv(registryproto.CLIExecutableEnv, "")
	prev := cloudCLIName
	t.Cleanup(func() { cloudCLIName = prev })
	cloudCLIName = filepath.Join(t.TempDir(), "should-not-run")
	if tok, hint := resolveTokenCLI("  "); tok != "" || hint != "" {
		t.Errorf("empty host should resolve to nothing: token=%q hint=%q", tok, hint)
	}
}

func TestResolveTokenCLI_WhitespaceRejected(t *testing.T) {
	spectest.Proves(t, "registry/credential-seam-contract", "a-seam-token-is-a-bare-bearer", "the-extension-sdk-reads-a-status-line-as-no-token")
	// Human status lines ahead of the bearer (not a bare bearer) must not become
	// a token. The recording is the stdout the incident captured.
	recordedCloudCLI(t, recordedExchange(t, "lock-changed"))
	tok, hint := resolveTokenCLI("oci.putnami.dev")
	if tok != "" {
		t.Errorf("a value with internal whitespace must be rejected, got %q", tok)
	}
	if hint == "" {
		t.Error("expected a diagnostic hint for a non-bearer value")
	}
}

func TestResolveTokenCLI_EmptyStdoutIsSilent(t *testing.T) {
	spectest.Proves(t, "registry/credential-seam-contract", "a-seam-token-is-a-bare-bearer", "the-extension-sdk-is-silent-without-an-answer")
	fakeCloudCLI(t, fakeCLI{})
	tok, hint := resolveTokenCLI("oci.putnami.dev")
	if tok != "" || hint != "" {
		t.Errorf("an empty success must be silent: token=%q hint=%q", tok, hint)
	}
}
