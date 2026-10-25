package qualify

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	httproutes "go.putnami.dev/protocol/http-routes"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
)

// The verdict goldens run `putnami qualify --target local` through the shipped
// CLI binary and pin the verdict document byte for byte, after normalizing
// what differs between runs: the worktree's fingerprint and commit, the
// composition id, ports, timestamps and durations.
//
// PUTNAMI_UPDATE_QUALIFY_GOLDENS=1 rewrites the goldens.

const verdictGoldenDir = "testdata/local-verdict"

var updateVerdictGoldens = os.Getenv("PUTNAMI_UPDATE_QUALIFY_GOLDENS") == "1"

// Each pattern keeps the member name and its separator, so an indented and a
// compact document normalize alike.
var (
	verdictFingerprint = regexp.MustCompile(`("fingerprint":\s*)"[^"]+"`)
	verdictHeadSHA     = regexp.MustCompile(`("headSHA":\s*)"[0-9a-f]{40}"`)
	verdictTimestamp   = regexp.MustCompile(`("(?:startedAt|finishedAt)":\s*)"[^"]+"`)
	verdictDuration    = regexp.MustCompile(`("durationMs":\s*)[0-9]+`)
	verdictComposition = regexp.MustCompile(`\b[0-9a-f]{16}\b`)
	verdictLoopback    = regexp.MustCompile(`127\.0\.0\.1:[0-9]+`)
)

func normalizeVerdict(root, document string) string {
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		document = strings.ReplaceAll(document, resolved, "<root>")
	}
	document = strings.ReplaceAll(document, root, "<root>")
	document = verdictFingerprint.ReplaceAllString(document, `${1}"<fingerprint>"`)
	document = verdictHeadSHA.ReplaceAllString(document, `${1}"<sha>"`)
	document = verdictTimestamp.ReplaceAllString(document, `${1}"<time>"`)
	document = verdictDuration.ReplaceAllString(document, `${1}"<ms>"`)
	document = verdictLoopback.ReplaceAllString(document, "127.0.0.1:<port>")
	return verdictComposition.ReplaceAllString(document, "<composition>")
}

// qualifyThroughCLI runs the built CLI's qualify command in the fixture and
// returns its exit code and stdout.
func qualifyThroughCLI(t *testing.T, fixture *localFixture, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(fixtureproc.CLI(t), append([]string{"qualify"}, args...)...)
	cmd.Dir = fixture.root
	cmd.Env = fixtureproc.CLIEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, stdout.String()
	case errors.As(err, &exitErr):
		if exitErr.ExitCode() != 1 {
			t.Fatalf("putnami qualify exited %d\nstdout:\n%s\nstderr:\n%s", exitErr.ExitCode(), stdout.String(), stderr.String())
		}
		return exitErr.ExitCode(), stdout.String()
	}
	t.Fatalf("run putnami qualify: %v", err)
	return 0, ""
}

func assertVerdictGolden(t *testing.T, name string, code int, document string) {
	t.Helper()
	got := fmt.Sprintf("exit %d\n%s", code, document)
	if !strings.HasSuffix(got, "\n") {
		got += "\n"
	}
	path := filepath.Join(verdictGoldenDir, name+".golden")
	if updateVerdictGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with: PUTNAMI_UPDATE_QUALIFY_GOLDENS=1 ./putnamiw test --projects /tooling/cli --run VerdictDocument --no-cache --no-enforce-coverage)", path, err)
	}
	if string(want) != got {
		t.Errorf("%s drifted from its golden.\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

func goldenRoutes() []httproutes.Route {
	provenance := httproutes.Provenance{Project: "app", SourceKind: httproutes.SourceManual}
	return []httproutes.Route{
		{Match: httproutes.MatchExact, Path: "/items", Methods: []string{"GET"}, PublicEdge: true, Provenance: provenance},
	}
}

func TestQualifyLocal_TheVerdictDocumentKeepsItsBytes(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "each-member-is-an-ordinary-serve-run",
		"the-start-exit-and-verdict-documents-keep-their-bytes")
	t.Run("passed", func(t *testing.T) {
		fixture := newLocalFixture(t,
			localMember{name: "provider", mode: fixtureServe},
			localMember{name: "app", mode: fixtureServe, runsWith: []string{"provider"}, routes: goldenRoutes()},
		)
		code, stdout := qualifyThroughCLI(t, fixture, "app", "--target", "local", "--output=json", "--no-color")
		assertVerdictGolden(t, "passed", code, normalizeVerdict(fixture.root, stdout))
		if got := fixture.requests("app"); got != "GET /items" {
			t.Errorf("requests reaching the target = %q", got)
		}
		fixture.assertStopped(t, "provider", "app")
	})
	t.Run("composition-failed", func(t *testing.T) {
		fixture := newLocalFixture(t,
			localMember{name: "provider", mode: fixtureNeverReady},
			localMember{name: "app", mode: fixtureServe, runsWith: []string{"provider"}, routes: goldenRoutes()},
		)
		code, stdout := qualifyThroughCLI(t, fixture, "app", "--target", "local", "--ready-timeout", "2s", "--output=json", "--no-color")
		assertVerdictGolden(t, "composition-failed", code, normalizeVerdict(fixture.root, stdout))
		fixture.assertStopped(t, "provider")
	})
}
