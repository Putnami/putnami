package compose

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestEnginePrepare_ReturnsAnEngineRefusalWithoutPrintingIt holds the
// preparation run to returning an engine refusal: the error becomes the
// composition's failure, which the caller reports, so a copy on stderr would
// be a second report.
func TestEnginePrepare_ReturnsAnEngineRefusalWithoutPrintingIt(t *testing.T) {
	t.Setenv(git.SourceRevisionEnv, "not-a-commit")
	opts := Options{WorkspaceRoot: t.TempDir(), Target: &workspace.Project{ID: "/app"}}

	original := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = writer
	captured := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(reader)
		captured <- string(data)
	}()
	_, prepareErr := enginePrepare(context.Background(), opts, []string{"/app"}, []string{"serve"})
	os.Stderr = original
	_ = writer.Close()
	stderr := <-captured
	_ = reader.Close()

	if prepareErr == nil || !strings.Contains(prepareErr.Error(), git.SourceRevisionEnv) {
		t.Fatalf("enginePrepare error = %v, want the refusal of the source revision", prepareErr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want the refusal returned and not printed", stderr)
	}
}
