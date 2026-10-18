package language

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	wire "go.putnami.dev/protocol/extension/hostrpc"
)

// TestTasksReportsACallThatNeverReachedAProvider covers the one seam this
// package changes from the core's copy: a call that fails before any
// provider answers is an error, and the scan exits 2.
func TestTasksReportsACallThatNeverReachedAProvider(t *testing.T) {
	t.Parallel()
	var out, errors bytes.Buffer
	runner := LanguageRunner{Root: t.TempDir(), Out: &out, Err: &errors, Call: func(context.Context, string, string, json.RawMessage) (collab.Envelope, error) {
		return collab.Envelope{}, os.ErrNotExist
	}}
	if status := runner.RunLanguage(context.Background(), "tasks", nil); status != 2 || out.Len() != 0 ||
		errors.String() != "change language: file does not exist\n" {
		t.Fatalf("status=%d output=%q error=%q", status, out.String(), errors.String())
	}
}

func TestTasksReportsAProviderFailureWithTheCoresWording(t *testing.T) {
	t.Parallel()
	var errors bytes.Buffer
	runner := LanguageRunner{Root: t.TempDir(), Out: &bytes.Buffer{}, Err: &errors, Call: func(context.Context, string, string, json.RawMessage) (collab.Envelope, error) {
		return collab.Envelope{Outcome: collab.OutcomeUnsupported, Error: &collab.Error{Message: "no provider", Reason: collab.ReasonBindingMissing}}, nil
	}}
	if status := runner.RunLanguage(context.Background(), "tasks", nil); status != 2 ||
		!strings.HasPrefix(errors.String(), "change language: tasks find answered unsupported: ") {
		t.Fatalf("status=%d error=%q", status, errors.String())
	}
}

func TestRepositoryEnvironmentDropsWhatRedirectsTheRepositoryReaderAndTheHostChannel(t *testing.T) {
	t.Parallel()
	got := repositoryEnvironment([]string{"PATH=/bin", "GIT_DIR=/elsewhere", "GIT_CONFIG_COUNT=1", "GIT_WORK_TREE=/w", "HOME=/h",
		wire.SocketEnv + "=/tmp/host.sock", wire.TokenEnv + "=secret"})
	if strings.Join(got, " ") != "PATH=/bin HOME=/h" {
		t.Fatalf("kept %v", got)
	}
}
