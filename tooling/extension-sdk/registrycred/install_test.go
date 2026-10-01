package registrycred

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	registryproto "go.putnami.dev/protocol/registry"
)

// fakeMaterializeCLI points the seam at a controlled shim and clears the
// per-process memo, so each case starts from an unasked state.
func fakeMaterializeCLI(t *testing.T, fake fakeCLI) {
	t.Helper()
	fakeCloudCLI(t, fake)
	resetMaterializedForTest()
	t.Cleanup(resetMaterializedForTest)
}

func TestEnsureNativeCredential_MaterializesThroughTheHostOnlyForm(t *testing.T) {
	recorded := filepath.Join(t.TempDir(), "argv")
	fakeMaterializeCLI(t, fakeCLI{WordsFile: recorded})

	outcome := EnsureNativeCredential(context.Background(), t.TempDir(), "npm.putnami.dev")
	if outcome.Kind != KindMaterialized {
		t.Fatalf("Kind = %v, want KindMaterialized", outcome.Kind)
	}
	if outcome.Level != "debug" {
		t.Errorf("Level = %q, want debug — a successful refresh is not news", outcome.Level)
	}
	argv, err := os.ReadFile(recorded)
	if err != nil {
		t.Fatal(err)
	}
	want := registryproto.SeamParentCommand + " " + registryproto.SeamSubcommand +
		" --" + registryproto.SeamHostFlag + " npm.putnami.dev --" + registryproto.SeamMaterializeFlag
	if string(argv) != want {
		t.Errorf("argv = %q, want %q", argv, want)
	}
}

func TestEnsureNativeCredential_NeverPrintsTheToken(t *testing.T) {
	// A cloud that ignores --materialize and prints a bearer must not leak it.
	fakeMaterializeCLI(t, fakeCLI{Stdout: "pkt_secret_value\n"})
	outcome := EnsureNativeCredential(context.Background(), "", "npm.putnami.dev")
	if strings.Contains(outcome.Message, "pkt_secret_value") {
		t.Fatalf("the outcome message carries the token: %q", outcome.Message)
	}
}

func TestEnsureNativeCredential_EmptyHostAsksNothing(t *testing.T) {
	fakeMaterializeCLI(t, fakeCLI{Exit: 9})
	outcome := EnsureNativeCredential(context.Background(), "", "   ")
	if outcome.Kind != KindSkipped || outcome.Message != "" {
		t.Fatalf("outcome = %+v, want a silent skip", outcome)
	}
}

func TestEnsureNativeCredential_MissingCLIIsADebugSkip(t *testing.T) {
	t.Setenv(registryproto.CLIExecutableEnv, "")
	previous := cloudCLIName
	t.Cleanup(func() { cloudCLIName = previous })
	cloudCLIName = filepath.Join(t.TempDir(), "does-not-exist")
	resetMaterializedForTest()
	t.Cleanup(resetMaterializedForTest)

	outcome := EnsureNativeCredential(context.Background(), "", "npm.putnami.dev")
	if outcome.Kind != KindCloudAbsent {
		t.Fatalf("Kind = %v, want KindCloudAbsent", outcome.Kind)
	}
	if outcome.Level != "debug" {
		t.Errorf("Level = %q, want debug — a core-only install is supported", outcome.Level)
	}
}

// A real core-only putnami never says "Unknown command": it runs `cloud` as a
// task pipeline, reads `registry-token` as a project, and exits 2 with an
// undeclared-flag notice first. Classified on that first line alone, every
// core-only install warned that the cloud "cannot write the credential yet".
func TestEnsureNativeCredential_RecordedCoreOnlyCLIIsADebugSkip(t *testing.T) {
	recordedCloudCLI(t, recordedExchange(t, "core-only-cli-materialize"))
	resetMaterializedForTest()
	t.Cleanup(resetMaterializedForTest)
	outcome := EnsureNativeCredential(context.Background(), "", "npm.putnami.dev")
	if outcome.Kind != KindCloudAbsent || outcome.Level != "debug" {
		t.Fatalf("outcome = %+v, want a KindCloudAbsent debug line", outcome)
	}
}

// A cloud that names the subcommand in its flag notice is a cloud, and a flag
// it does not declare is still a usage error worth a warning.
func TestEnsureNativeCredential_SubcommandFlagNoticeIsNotACoreOnlyCLI(t *testing.T) {
	fakeMaterializeCLI(t, fakeCLI{
		Stderr: "putnami: flag --materialize is not declared by `cloud registry-token`; passing undeclared flags is deprecated\n",
		Exit:   2,
	})
	outcome := EnsureNativeCredential(context.Background(), "", "npm.putnami.dev")
	if outcome.Kind != KindUnsupported || outcome.Level != "warn" {
		t.Fatalf("outcome = %+v, want a KindUnsupported warning", outcome)
	}
}

func TestEnsureNativeCredential_AuthExitWarnsWithTheExactHint(t *testing.T) {
	recordedCloudCLI(t, recordedExchange(t, "not-authenticated-materialize"))
	resetMaterializedForTest()
	t.Cleanup(resetMaterializedForTest)
	outcome := EnsureNativeCredential(context.Background(), "", "go.putnami.dev")
	if outcome.Kind != KindNotSignedIn || outcome.Level != "warn" {
		t.Fatalf("outcome = %+v, want a KindNotSignedIn warning", outcome)
	}
	if !strings.Contains(outcome.Message, "run putnami cloud login") {
		t.Errorf("Message = %q, want the exact sign-in hint", outcome.Message)
	}
}

func TestEnsureNativeCredential_UnclassifiedAuthTextStillWarnsWithTheHint(t *testing.T) {
	// A cloud that exits 1 for everything still says so in words.
	fakeMaterializeCLI(t, fakeCLI{Stderr: "not signed in to Putnami Cloud\n", Exit: 1})
	outcome := EnsureNativeCredential(context.Background(), "", "go.putnami.dev")
	if outcome.Kind != KindNotSignedIn {
		t.Fatalf("Kind = %v, want KindNotSignedIn", outcome.Kind)
	}
	if !strings.Contains(outcome.Message, SignInHint) {
		t.Errorf("Message = %q, want the exact sign-in hint", outcome.Message)
	}
}

func TestEnsureNativeCredential_UsageErrorWarnsAndKeepsGoing(t *testing.T) {
	// The state until the cloud side lands: the flag is rejected.
	fakeMaterializeCLI(t, fakeCLI{Stderr: "flag provided but not defined: -materialize\n", Exit: 2})
	outcome := EnsureNativeCredential(context.Background(), "", "npm.putnami.dev")
	if outcome.Kind != KindUnsupported || outcome.Level != "warn" {
		t.Fatalf("outcome = %+v, want a KindUnsupported warning", outcome)
	}
	if !strings.Contains(outcome.Message, "continuing") {
		t.Errorf("Message = %q, want it to say the install continues", outcome.Message)
	}
}

// Session wording on a later stderr line — a help listing that names
// `putnami cloud login` — does not turn a usage error into "not signed in".
func TestEnsureNativeCredential_UsageErrorWithALaterLoginLineStaysAUsageError(t *testing.T) {
	fakeMaterializeCLI(t, fakeCLI{
		Stderr: "flag provided but not defined: -materialize\nCommands:\n  putnami cloud login   Sign in\n",
		Exit:   2,
	})
	outcome := EnsureNativeCredential(context.Background(), "", "npm.putnami.dev")
	if outcome.Kind != KindUnsupported {
		t.Fatalf("outcome = %+v, want KindUnsupported", outcome)
	}
}

func TestEnsureNativeCredential_OtherFailureWarnsOnce(t *testing.T) {
	fakeMaterializeCLI(t, fakeCLI{Stderr: "registry unreachable\n", Exit: 4})
	outcome := EnsureNativeCredential(context.Background(), "", "npm.putnami.dev")
	if outcome.Kind != KindFailed || outcome.Level != "warn" {
		t.Fatalf("outcome = %+v, want a KindFailed warning", outcome)
	}
	if !strings.Contains(outcome.Message, "registry unreachable") {
		t.Errorf("Message = %q, want the cloud's own diagnostic relayed", outcome.Message)
	}
}

func TestEnsureNativeCredential_DedupesPerHostPerProcess(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "calls")
	fakeMaterializeCLI(t, fakeCLI{CountFile: counter, Stderr: "not signed in\n", Exit: 3})

	first := EnsureNativeCredential(context.Background(), "", "npm.putnami.dev")
	second := EnsureNativeCredential(context.Background(), "", "NPM.putnami.dev")
	other := EnsureNativeCredential(context.Background(), "", "go.putnami.dev")

	if first.Message == "" {
		t.Error("the first call must carry the warning")
	}
	if second.Message != "" {
		t.Errorf("the second call for the same host must be silent, got %q", second.Message)
	}
	if second.Kind != first.Kind {
		t.Errorf("second.Kind = %v, want the memoized %v", second.Kind, first.Kind)
	}
	if other.Message == "" {
		t.Error("a different host must be asked, and reported, on its own")
	}

	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "x"); got != 2 {
		t.Errorf("the seam ran %d times, want 2 (one per distinct host)", got)
	}
}

func TestEnsureNativeCredential_RunsInTheWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	recorded := filepath.Join(t.TempDir(), "cwd")
	fakeMaterializeCLI(t, fakeCLI{CwdFile: recorded})

	if outcome := EnsureNativeCredential(context.Background(), root, "npm.putnami.dev"); outcome.Kind != KindMaterialized {
		t.Fatalf("Kind = %v, want KindMaterialized", outcome.Kind)
	}
	data, err := os.ReadFile(recorded)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != resolved {
		t.Errorf("cwd = %q, want the workspace root %q", strings.TrimSpace(string(data)), resolved)
	}
}

func TestEnsureNativeCredential_CancelledContextIsAWarning(t *testing.T) {
	fakeMaterializeCLI(t, fakeCLI{Sleep: 5 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outcome := EnsureNativeCredential(ctx, "", "npm.putnami.dev")
	if outcome.Kind != KindFailed {
		t.Fatalf("Kind = %v, want KindFailed", outcome.Kind)
	}
	if outcome.Message == "" {
		t.Error("a timed-out refresh must say so")
	}
}

func TestFirstLine_BoundsTheRelayedDiagnostic(t *testing.T) {
	if got := firstLine("\n\n  hello  \nworld\n"); got != "hello" {
		t.Errorf("firstLine = %q, want hello", got)
	}
	if got := firstLine(strings.Repeat("a", 500)); len(got) != 200 {
		t.Errorf("firstLine length = %d, want 200", len(got))
	}
	if got := firstLine("   \n \n"); got != "" {
		t.Errorf("firstLine = %q, want empty", got)
	}
}
