// Package treeverify drives `putnami tree verify` through the real CLI: the
// global flag parser, the catalog and the dispatcher, with both process
// streams captured apart. finalize-pr.sh and the execute skill parse the
// verdict document on stdout, so these tests pin that nothing else reaches
// either stream. They swap the process streams, change the working directory
// and set the environment, so they run one after another in their own test
// binary. The verifier's own checks are tested in internal/commands/treecmd.
package treeverify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/launch"
)

func TestMain(m *testing.M) {
	os.Exit(clitest.Main(m))
}

// runTreeVerify runs `putnami <args>` in dir through App.Run and returns the
// exit code, stdout and stderr.
func runTreeVerify(t *testing.T, dir string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	stdout, stderr = clitest.CaptureStreams(t, func() {
		app, err := cli.NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(dir)
		code = app.Run(context.Background(), args)
	})
	return code, stdout, stderr
}

// TestTreeVerifyBareCallIsAVerdictOnStdoutAlone pins how finalize-pr.sh tells
// whether the CLI has the verifier: a bare `putnami tree verify` prints one
// not-verified verdict on stdout, exits 1 and writes nothing on stderr, in
// every output mode. No parser, catalog or dispatcher step answers first.
func TestTreeVerifyBareCallIsAVerdictOnStdoutAlone(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "local-evidence-verification",
		"a-bare-call-is-a-not-verified-verdict-on-stdout-alone")
	hometest.Temp(t)
	t.Setenv(launch.NoRelaunchEnv, "1")
	dir := t.TempDir()
	want := `{"verdict":"not-verified","reason":"choose exactly one of --record, --snapshot, --ref and --gate"}` + "\n"
	for _, args := range [][]string{
		{"tree", "verify"},
		{"tree", "verify", "--output=json"},
		{"tree", "verify", "--output", "jsonl"},
		{"tree", "verify", "--json"},
	} {
		code, stdout, stderr := runTreeVerify(t, dir, args...)
		if code != cli.ExitError || stdout != want || stderr != "" {
			t.Errorf("putnami %s: exit %d, stdout %q, stderr %q; want exit %d, stdout %q, empty stderr",
				strings.Join(args, " "), code, stdout, stderr, cli.ExitError, want)
		}
	}
}

// TestTreeVerifyForwardsItsDocumentUntouched runs a successful verification in
// every output mode and flag spelling: stdout is the verifier's own document,
// never a result envelope, and paths resolve against the Git top level from a
// sub directory.
func TestTreeVerifyForwardsItsDocumentUntouched(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "local-evidence-verification",
		"a-verdict-reaches-stdout-untouched-in-every-output-mode")
	hometest.Temp(t)
	t.Setenv(launch.NoRelaunchEnv, "1")
	root := t.TempDir()
	clitest.WriteFile(t, filepath.Join(root, "README.md"), "# Test\n")
	clitest.InitGitRepo(t, root)
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// 47dd7b50... is the SHA-256 of README.md, "# Test\n".
	want := "{\n  \"path\": \"README.md\",\n" +
		"  \"sha256\": \"47dd7b50af765df240fe2514f029fc697c907fc37a3267e22060f2f9f611975c\"\n}\n"
	for _, args := range [][]string{
		{"tree", "verify", "--ref", "README.md"},
		{"tree", "verify", "--ref=README.md"},
		{"tree", "verify", "--ref", "README.md", "--output=json"},
		{"tree", "verify", "--output=jsonl", "--ref", "README.md"},
	} {
		code, stdout, stderr := runTreeVerify(t, sub, args...)
		if code != cli.ExitSuccess || stdout != want || stderr != "" {
			t.Errorf("putnami %s: exit %d, stdout %q, stderr %q; want exit 0, stdout %q, empty stderr",
				strings.Join(args, " "), code, stdout, stderr, want)
		}
	}
}
