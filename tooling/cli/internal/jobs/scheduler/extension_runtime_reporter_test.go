package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
)

func TestReporterRuntimePreparationHonorsContextWhileWaitingForArtifact(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeRuntimeFixture(t, root, "@putnami/test", "1.2.3")
	ext := runtimeTestExtension(root)
	digest, err := cachekey.ExtensionRuntimeDigest(ext)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := artifactstore.New(t.TempDir())
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := artifacts.Admit(digest, func(string) error { close(locked); <-release; return nil })
		done <- err
	}()
	<-locked
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := prepareOrLoadExtensionRuntime(ctx, artifacts, ext); result <- err }()
	select {
	case err := <-result:
		var runtimeErr *extensionRuntimeError
		if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &runtimeErr) || runtimeErr.code != extensionproto.FailureRuntimePrepareFailed {
			t.Fatalf("runtime ignored canceled lock wait: %v", err)
		}
	case <-time.After(answerWaitBudget):
		t.Fatal("runtime preparation ignored its 50ms context while the artifact lock was held")
	}
}
