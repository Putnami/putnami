package pkg

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/oci"
)

// captureEvents captures JSONL events emitted to stdout during fn.
func captureEvents(t *testing.T, fn func(emit *jsonl.Emitter)) []map[string]any {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStdout := os.Stdout
	os.Stdout = w

	fn(jsonl.New())

	w.Close()
	os.Stdout = origStdout

	var buf bytes.Buffer
	io.Copy(&buf, r)
	r.Close()

	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) == nil {
			events = append(events, event)
		}
	}
	return events
}

// validDiagnosticSeverities is the runtime protocol's DiagnosticSeverity set.
var validDiagnosticSeverities = map[string]bool{
	"error": true, "warning": true, "info": true, "hint": true,
}

// writeDockerManifest emits a diagnostic when the manifest can't be written.
// That diagnostic must use the canonical "warning" severity, not "warn".
func TestWriteDockerManifest_WriteFailureEmitsWarningSeverity(t *testing.T) {
	// Put a regular file where the output directory's parent should be, so
	// MkdirAll and the subsequent WriteFile both fail with ENOTDIR.
	tmp := t.TempDir()
	blocker := filepath.Join(tmp, "blocker")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	dockerOutputDir := filepath.Join(blocker, "docker")

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		writeDockerManifest(emit, dockerOutputDir, map[string]any{"image": "x"})
	})

	// Two diagnostics: the advisory manifest write, then the channel record.
	// Their severities are deliberately different — a manifest this task can
	// rewrite next run is a warning, while a channel that goes unrecorded would
	// silently skip publication, so it fails the step.
	if len(events) != 2 {
		t.Fatalf("expected 2 diagnostic events, got %d: %+v", len(events), events)
	}
	sev, _ := events[0]["severity"].(string)
	if sev != "warning" {
		t.Errorf("manifest severity = %q, want %q", sev, "warning")
	}
	recordSev, _ := events[1]["severity"].(string)
	if recordSev != "error" {
		t.Errorf("channel record severity = %q, want %q", recordSev, "error")
	}
	for _, severity := range []string{sev, recordSev} {
		if !validDiagnosticSeverities[severity] {
			t.Errorf("severity %q is not a valid DiagnosticSeverity", severity)
		}
	}
}

// --- dockerSpec ---

func TestDockerSpec_ContentPureAndPinned(t *testing.T) {
	spec := dockerSpec(dockerBaseImagePinned, "linux/amd64", "/ws/.putnami/out/app/bin/app", "3000")

	if !strings.Contains(spec.BaseRef, "@sha256:") {
		t.Errorf("base ref must be digest-pinned for reproducible digests, got %q", spec.BaseRef)
	}
	if len(spec.Layers) != 1 || len(spec.Layers[0].Files) != 1 {
		t.Fatalf("expected a single binary layer, got %+v", spec.Layers)
	}
	binary := spec.Layers[0].Files[0]
	if binary.Path != "/app" || binary.Mode != 0o755 {
		t.Errorf("binary file = %+v, want /app mode 0755", binary)
	}
	if len(spec.Entrypoint) != 1 || spec.Entrypoint[0] != "/app" {
		t.Errorf("entrypoint = %v", spec.Entrypoint)
	}
	// No git-derived bytes: nothing version-shaped may reach the spec.
	canonical := spec.CanonicalString()
	for _, forbidden := range []string{"0.0.0", "revision", "branch"} {
		if strings.Contains(canonical, forbidden) {
			t.Errorf("spec must not carry %q, got: %s", forbidden, canonical)
		}
	}
}

func TestDockerSpec_HashSensitivity(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "app")
	if err := os.WriteFile(binaryPath, []byte("binary-v1"), 0o755); err != nil {
		t.Fatal(err)
	}

	specA := dockerSpec(dockerBaseImagePinned, "linux/amd64", binaryPath, "3000")
	hashA, err := oci.ContentHash(specA)
	if err != nil {
		t.Fatal(err)
	}
	hashB, err := oci.ContentHash(specA)
	if err != nil {
		t.Fatal(err)
	}
	if hashA != hashB {
		t.Error("expected stable hash for identical inputs")
	}

	// Port change must change the image identity.
	hashC, err := oci.ContentHash(dockerSpec(dockerBaseImagePinned, "linux/amd64", binaryPath, "8080"))
	if err != nil {
		t.Fatal(err)
	}
	if hashC == hashA {
		t.Error("expected hash change when port changes")
	}

	// Base pin bump must change the image identity.
	hashD, err := oci.ContentHash(dockerSpec("gcr.io/distroless/static@sha256:1111111111111111111111111111111111111111111111111111111111111111", "linux/amd64", binaryPath, "3000"))
	if err != nil {
		t.Fatal(err)
	}
	if hashD == hashA {
		t.Error("expected hash change when base pin changes")
	}
}

// --- dockerfileFor (workspace-builder path) ---

func TestDockerfileFor_ContentPure(t *testing.T) {
	got := dockerfileFor("", ".putnami/out/app/build/bin/app", "3000")

	if !strings.Contains(got, "FROM gcr.io/distroless/static:nonroot") {
		t.Errorf("expected base image, got:\n%s", got)
	}
	if !strings.Contains(got, "COPY .putnami/out/app/build/bin/app /app") {
		t.Errorf("expected binary COPY, got:\n%s", got)
	}
	// No git-derived bytes: a version or revision label would make the image
	// digest change per release id for identical content.
	if strings.Contains(got, "LABEL") || strings.Contains(got, "org.opencontainers.image") {
		t.Errorf("expected no labels in content-pure Dockerfile, got:\n%s", got)
	}
}

func TestDockerfileFor_WorkspaceBuilder(t *testing.T) {
	got := dockerfileFor("builder:latest", "bin/app", "8080")

	if !strings.Contains(got, "FROM ${WORKSPACE_BUILDER_IMAGE} AS builder") {
		t.Errorf("expected builder stage, got:\n%s", got)
	}
	if !strings.Contains(got, "COPY --from=builder /app/bin/app /app") {
		t.Errorf("expected builder COPY, got:\n%s", got)
	}
	if !strings.Contains(got, "ENV PORT=8080") {
		t.Errorf("expected port env, got:\n%s", got)
	}
	if strings.Contains(got, "LABEL") {
		t.Errorf("expected no labels in content-pure Dockerfile, got:\n%s", got)
	}
}

func TestValidateDockerCompositionRejectsIgnoredLocalProjectBase(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params dockerParams
		wantOK bool
	}{
		{name: "ordinary workspace builder", params: dockerParams{WorkspaceBuilderImage: "builder:latest"}, wantOK: true},
		{name: "native local base", params: dockerParams{BaseLayout: "/tmp/base", BaseDigest: "sha256:" + strings.Repeat("a", 64)}, wantOK: true},
		{name: "builder cannot ignore local base", params: dockerParams{WorkspaceBuilderImage: "builder:latest", BaseLayout: "/tmp/base", BaseDigest: "sha256:" + strings.Repeat("a", 64)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDockerComposition(tc.params)
			if tc.wantOK && err != nil {
				t.Fatalf("validateDockerComposition() error = %v", err)
			}
			if !tc.wantOK && (err == nil || !strings.Contains(err.Error(), "dockerBaseProject local OCI candidate")) {
				t.Fatalf("validateDockerComposition() error = %v, want local-candidate failure", err)
			}
		})
	}
}

// --- dockerContentHash ---

func TestDockerContentHash_StableAndInputSensitive(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "app")
	if err := os.WriteFile(binaryPath, []byte("binary-v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	dockerfile := dockerfileFor("", "bin/app", "3000")

	hashA, err := dockerContentHash(dockerfile, binaryPath)
	if err != nil {
		t.Fatalf("dockerContentHash: %v", err)
	}
	hashB, err := dockerContentHash(dockerfile, binaryPath)
	if err != nil {
		t.Fatalf("dockerContentHash: %v", err)
	}
	if hashA != hashB {
		t.Errorf("expected stable hash for identical inputs, got %q vs %q", hashA, hashB)
	}
	if len(hashA) != 12 {
		t.Errorf("expected 12-char hash, got %q", hashA)
	}

	// Binary change must change the hash.
	if err := os.WriteFile(binaryPath, []byte("binary-v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	hashC, err := dockerContentHash(dockerfile, binaryPath)
	if err != nil {
		t.Fatalf("dockerContentHash: %v", err)
	}
	if hashC == hashA {
		t.Error("expected hash to change when binary content changes")
	}

	// Dockerfile change (e.g. port) must change the hash.
	hashD, err := dockerContentHash(dockerfileFor("", "bin/app", "8080"), binaryPath)
	if err != nil {
		t.Fatalf("dockerContentHash: %v", err)
	}
	if hashD == hashC {
		t.Error("expected hash to change when Dockerfile changes")
	}
}

func TestDockerContentHash_MissingBinary(t *testing.T) {
	if _, err := dockerContentHash("FROM scratch\n", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("expected error for missing binary")
	}
}
