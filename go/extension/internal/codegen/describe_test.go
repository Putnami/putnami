package codegen

import (
	"bytes"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/go/extension/internal/codegen/openapiutil"
	"go.putnami.dev/go/extension/internal/toolchain"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"

	"go.putnami.dev/protocol/features/spectest"
)

func TestSnapshotSchemaDirEmptyDir(t *testing.T) {
	dir := t.TempDir()
	snap := snapshotSchemaDir(filepath.Join(dir, "schema"))
	if len(snap) != 0 {
		t.Errorf("expected empty snapshot, got %d entries", len(snap))
	}
}

func TestCapabilityDiagnosticsFromDescribeOutput(t *testing.T) {
	output := "capability manifest validation failed:\n" +
		"[capabilities.missing_required_provider] requiredCapabilities[0].requires[2]: project \"orders\" required capability sql declared by src/sql.go needs readiness"
	event := captureCapabilityDiagnostic(t, output)
	if event["type"] != "diagnostic" || event["code"] != "capabilities.missing_required_provider" {
		t.Fatalf("event = %#v, want coded diagnostic", event)
	}
	message, _ := event["message"].(string)
	for _, context := range []string{"requiredCapabilities[0].requires[2]", `project "orders"`, "src/sql.go"} {
		if !strings.Contains(message, context) {
			t.Errorf("message = %q, want context %q", message, context)
		}
	}
}

func TestHTTPRouteDiagnosticsFromDescribeOutput(t *testing.T) {
	output := "http route inventory validation failed:\n" +
		"[http_routes.unsupported_pattern] routes[3].path: catch-all patterns must be expanded"
	event := captureCapabilityDiagnostic(t, output)
	if event["type"] != "diagnostic" || event["code"] != "http_routes.unsupported_pattern" {
		t.Fatalf("event = %#v, want coded HTTP route diagnostic", event)
	}
	if message, _ := event["message"].(string); !strings.Contains(message, "routes[3].path") {
		t.Fatalf("message = %q, want field context", message)
	}
}

func captureCapabilityDiagnostic(t *testing.T, output string) map[string]any {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = w
	emitCapabilityDiagnostics(jsonl.New(), output)
	_ = w.Close()
	os.Stdout = original
	defer r.Close()
	var event map[string]any
	if err := json.NewDecoder(r).Decode(&event); err != nil {
		t.Fatalf("decode JSONL diagnostic: %v", err)
	}
	return event
}

func TestSnapshotChangedSinceDetectsNewAndModified(t *testing.T) {
	dir := t.TempDir()
	schemaDir := filepath.Join(dir, "schema")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Pre-existing file recorded in the snapshot.
	existing := filepath.Join(schemaDir, "old.json")
	if err := os.WriteFile(existing, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := snapshotSchemaDir(schemaDir)

	// changedSince should report unchanged for files we haven't touched.
	if snap.changedSince(existing) {
		t.Error("untouched file should not be reported as changed")
	}

	// Modify the file — bump mtime explicitly so the test isn't racy on
	// filesystems with low mtime resolution.
	future := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(existing, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(existing, future, future); err != nil {
		t.Fatal(err)
	}
	if !snap.changedSince(existing) {
		t.Error("modified file should be reported as changed")
	}

	// New file isn't in the snapshot, so it's reported as changed.
	created := filepath.Join(schemaDir, "new.json")
	if err := os.WriteFile(created, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !snap.changedSince(created) {
		t.Error("new file should be reported as changed")
	}
}

func TestCollectArtifactsCommitsOnlyChangedFiles(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "single-schema-committer", "only-changed-files-are-committed")
	projectPath := t.TempDir()
	genDir := filepath.Join(projectPath, ".gen")
	schemaDir := filepath.Join(genDir, "schema")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Pre-existing file (e.g. from the static codegen runner).
	preExisting := filepath.Join(schemaDir, "old.json")
	if err := os.WriteFile(preExisting, []byte("from-static"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := snapshotSchemaDir(schemaDir)

	// Describe runs and writes a NEW file; the existing file is untouched.
	newFile := filepath.Join(schemaDir, "fresh.json")
	if err := os.WriteFile(newFile, []byte("from-describe"), 0o644); err != nil {
		t.Fatal(err)
	}

	artifacts, err := collectArtifacts(projectPath, genDir, true, snap, nil)
	if err != nil {
		t.Fatalf("collectArtifacts: %v", err)
	}

	// Only fresh.json was written by describe — old.json must not appear.
	wantGen := filepath.Join(".gen", "schema", "fresh.json")
	wantCommit := filepath.Join("schema", "fresh.json")
	for _, want := range []string{wantGen, wantCommit} {
		found := false
		for _, a := range artifacts {
			if a == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing artifact %q in %v", want, artifacts)
		}
	}
	for _, a := range artifacts {
		if strings.Contains(a, "old.json") {
			t.Errorf("static-runner file leaked into describe artifacts: %v", artifacts)
		}
	}

	// And the project tree got the committed copy.
	if _, err := os.Stat(filepath.Join(projectPath, "schema", "fresh.json")); err != nil {
		t.Errorf("expected committed copy: %v", err)
	}
}

func TestCollectArtifactsPreservesUnmergedCanonicalOpenAPIBytesAndHash(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "single-schema-committer", "publishing-an-unmerged-canonical-provider-document-preserves-its-hash")
	projectPath := t.TempDir()
	genDir := filepath.Join(projectPath, ".gen")
	schemaDir := filepath.Join(genDir, "schema")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	snap := snapshotSchemaDir(schemaDir)

	// This is the shape emitted by the framework plugin: object keys are
	// canonical, while structural arrays are already sorted by GenerateSpec.
	providerDocument := map[string]any{
		"components": map[string]any{"schemas": map[string]any{
			"canonicalResponse": map[string]any{
				"properties": map[string]any{
					"alpha": map[string]any{"type": "string"},
					"zeta":  map[string]any{"type": "string"},
				},
				"required": []any{"alpha", "zeta"},
				"type":     "object",
			},
		}},
		"info":    map[string]any{"title": "Canonical API", "version": "1.0.0"},
		"openapi": "3.0.3",
		"paths": map[string]any{"/widgets/{zeta}/{alpha}": map[string]any{
			"get": map[string]any{
				"operationId": "getWidgetsByZetaByAlpha",
				"parameters": []any{
					map[string]any{"in": "path", "name": "alpha", "required": true, "schema": map[string]any{"type": "string"}},
					map[string]any{"in": "path", "name": "zeta", "required": true, "schema": map[string]any{"type": "string"}},
					map[string]any{"in": "query", "name": "alpha", "required": true, "schema": map[string]any{"type": "string"}},
					map[string]any{"in": "query", "name": "zulu", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{"200": map[string]any{"description": "Success"}},
			},
		}},
	}
	provider, err := json.MarshalIndent(providerDocument, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	provider = append(provider, '\n')
	generatedPath := filepath.Join(schemaDir, "openapi.json")
	if err := os.WriteFile(generatedPath, provider, 0o644); err != nil {
		t.Fatal(err)
	}

	beforeHash := fmt.Sprintf("%x", sha256.Sum256(provider))
	if _, err := collectArtifacts(projectPath, genDir, true, snap, nil); err != nil {
		t.Fatalf("collectArtifacts: %v", err)
	}
	for _, path := range []string{generatedPath, filepath.Join(projectPath, "schema", "openapi.json")} {
		published, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !bytes.Equal(published, provider) {
			t.Errorf("publication rewrote canonical provider bytes at %s\nprovider:\n%s\npublished:\n%s", path, provider, published)
		}
		if afterHash := fmt.Sprintf("%x", sha256.Sum256(published)); afterHash != beforeHash {
			t.Errorf("publication hash = %s, want %s", afterHash, beforeHash)
		}
	}
}

func TestCollectArtifactsSkipsCommitWhenOptedOut(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "generation-opt-out", "collect-skips-the-commit-for-an-opted-out-project")
	projectPath := t.TempDir()
	genDir := filepath.Join(projectPath, ".gen")
	schemaDir := filepath.Join(genDir, "schema")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}

	snap := snapshotSchemaDir(schemaDir)

	if err := os.WriteFile(filepath.Join(schemaDir, "spec.json"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	artifacts, err := collectArtifacts(projectPath, genDir, false, snap, nil)
	if err != nil {
		t.Fatalf("collectArtifacts: %v", err)
	}

	// .gen copy is recorded; committed copy is not.
	if len(artifacts) != 1 {
		t.Errorf("expected 1 artifact, got %d: %v", len(artifacts), artifacts)
	}
	if _, err := os.Stat(filepath.Join(projectPath, "schema", "spec.json")); !os.IsNotExist(err) {
		t.Errorf("expected committed copy to be skipped, got err=%v", err)
	}
}

// TestResetCededSchemaRestoresGenerateStagingAfterACacheHit pins describe's half
// of the generate-stages/describe-converges hand-off across a cache hit.
//
// build-generate cedes .gen/schema to this task, and a directory restore swaps a
// staged tree over its destination, so a generate CACHE HIT leaves no .gen/schema
// on disk at all. Everything describe does with generate's static output depends
// on that tree: the merge's `before` side, and the promotion of an artifact
// generate staged that the describe binary never rewrote. Rebuilding it from the
// generate-owned mirror is what makes describe's starting tree the same whether
// generate executed in this invocation or was served from cache.
func TestResetCededSchemaRestoresGenerateStagingAfterACacheHit(t *testing.T) {
	projectPath := t.TempDir()
	genDir := filepath.Join(projectPath, ".gen")
	mirror := filepath.Join(genDir, "generate-staging", "schema")
	if err := os.MkdirAll(filepath.Join(mirror, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	stub := []byte(`{"openapi":"3.1.0","paths":{"/a":{"get":{}}}}`)
	if err := os.WriteFile(filepath.Join(mirror, "openapi.json"), stub, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mirror, "nested", "extra.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The state a generate cache hit leaves: the mirror is restored with the rest
	// of .gen, the ceded subtree is not there at all.
	if err := resetCededSchemaToGenerateStaging(genDir); err != nil {
		t.Fatalf("reset: %v", err)
	}
	seeded, err := os.ReadFile(filepath.Join(genDir, "schema", "openapi.json"))
	if err != nil {
		t.Fatalf("the ceded subtree was not seeded: %v", err)
	}
	if !bytes.Equal(seeded, stub) {
		t.Errorf("seeded openapi.json = %q, want generate's staged bytes", seeded)
	}
	if _, err := os.Stat(filepath.Join(genDir, "schema", "nested", "extra.json")); err != nil {
		t.Errorf("the reset did not reach a nested staged artifact: %v", err)
	}

	// Running it over a tree an earlier describe left behind restores generate's
	// bytes AND drops everything else, which is what makes the starting state a
	// function of generate's output alone.
	if err := os.WriteFile(filepath.Join(genDir, "schema", "openapi.json"), []byte(`{"stale":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := resetCededSchemaToGenerateStaging(genDir); err != nil {
		t.Fatalf("second reset: %v", err)
	}
	again, err := os.ReadFile(filepath.Join(genDir, "schema", "openapi.json"))
	if err != nil || !bytes.Equal(again, stub) {
		t.Errorf("re-seeded openapi.json = %q (%v), want generate's staged bytes", again, err)
	}
}

// TestResetCededSchemaDropsARemovedProducersArtifact pins the pruning half.
//
// This task CAPTURES .gen/schema, and capture is disk-driven: the scheduler walks
// the declared directory, it does not read the job's artifact list. A describer
// that still runs removes its own stale output, but a producer DELETED from the
// app — a dropped proto.New, an openapi plugin that no longer configures — never
// runs at all, so nothing inside the app can drop its artifact. Left in place it
// would be recorded under this run's key and restored by every later hit on it,
// while a clean-tree build of the same sources produces an entry without it.
func TestResetCededSchemaDropsARemovedProducersArtifact(t *testing.T) {
	genDir := filepath.Join(t.TempDir(), ".gen")
	mirror := filepath.Join(genDir, "generate-staging", "schema")
	if err := os.MkdirAll(mirror, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := []byte(`{"openapi":"3.1.0"}`)
	if err := os.WriteFile(filepath.Join(mirror, "openapi.json"), stub, 0o644); err != nil {
		t.Fatal(err)
	}
	schemaDir := filepath.Join(genDir, "schema")
	if err := os.MkdirAll(filepath.Join(schemaDir, "feature-evidence"), 0o755); err != nil {
		t.Fatal(err)
	}
	// What an earlier build of a richer app left behind.
	for rel, body := range map[string]string{
		"api.proto":                   "syntax = \"proto3\";",
		"capabilities.json":           `{"stale":true}`,
		"http-routes.json":            `{"stale":true}`,
		"feature-evidence/stale.json": `{"stale":true}`,
	} {
		if err := os.WriteFile(filepath.Join(schemaDir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := resetCededSchemaToGenerateStaging(genDir); err != nil {
		t.Fatalf("reset: %v", err)
	}

	for _, rel := range []string{"api.proto", "capabilities.json", "http-routes.json", "feature-evidence/stale.json"} {
		if _, err := os.Stat(filepath.Join(schemaDir, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("a removed producer's %s survived the reset: %v", rel, err)
		}
	}
	kept, err := os.ReadFile(filepath.Join(schemaDir, "openapi.json"))
	if err != nil || !bytes.Equal(kept, stub) {
		t.Errorf("the reset dropped generate's own staged artifact: %q (%v)", kept, err)
	}
}

// A project whose generators produce no contract hands nothing over, and that is
// not an error: the mirror simply does not exist.
func TestResetCededSchemaWithoutAMirrorIsANoOp(t *testing.T) {
	genDir := filepath.Join(t.TempDir(), ".gen")
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := resetCededSchemaToGenerateStaging(genDir); err != nil {
		t.Fatalf("reset without a mirror: %v", err)
	}
	if _, err := os.Stat(filepath.Join(genDir, "schema")); !os.IsNotExist(err) {
		t.Errorf("the reset created a schema tree out of nothing: %v", err)
	}
}

func TestCollectArtifactsSkipsGzCompanionsFromCommit(t *testing.T) {
	projectPath := t.TempDir()
	genDir := filepath.Join(projectPath, ".gen")
	schemaDir := filepath.Join(genDir, "schema")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}

	snap := snapshotSchemaDir(schemaDir)

	for _, name := range []string{"spec.json", "spec.json.gz"} {
		if err := os.WriteFile(filepath.Join(schemaDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := collectArtifacts(projectPath, genDir, true, snap, nil); err != nil {
		t.Fatalf("collectArtifacts: %v", err)
	}

	// Both files exist under .gen/, but only the .json got committed.
	if _, err := os.Stat(filepath.Join(projectPath, "schema", "spec.json")); err != nil {
		t.Errorf("json should be committed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(projectPath, "schema", "spec.json.gz")); !os.IsNotExist(err) {
		t.Errorf("gz should NOT be committed, got err=%v", err)
	}
}

func TestCollectArtifactsMergesOpenAPIFromGenerateAndDescribe(t *testing.T) {
	projectPath := t.TempDir()
	genDir := filepath.Join(projectPath, ".gen")
	schemaDir := filepath.Join(genDir, "schema")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}

	staticSpec := []byte(`{
  "openapi": "3.0.3",
  "info": { "title": "Control Plane", "version": "1.0.0" },
  "paths": {
    "/v1/iam/users": {
      "get": {
        "operationId": "getV1_Iam_Users",
        "responses": { "200": { "description": "Successful response" } }
      }
    }
  }
}`)
	runtimeSpec := []byte(`{
  "openapi": "3.0.3",
  "info": { "title": "Control Plane", "version": "1.0.0" },
  "paths": {
    "/api/configs": {
      "post": {
        "operationId": "postApi_Configs",
        "responses": { "200": { "description": "Successful response" } }
      }
    },
    "/api/schemas": {
      "post": {
        "operationId": "postApi_Schemas",
        "responses": { "200": { "description": "Successful response" } }
      }
    },
    "/api/secrets": {
      "post": {
        "operationId": "postApi_Secrets",
        "responses": { "200": { "description": "Successful response" } }
      }
    }
  }
}`)

	first := runOpenAPIGenerateDescribeCycle(t, projectPath, staticSpec, runtimeSpec)
	second := runOpenAPIGenerateDescribeCycle(t, projectPath, staticSpec, runtimeSpec)
	if !bytes.Equal(first, second) {
		t.Fatalf("merged openapi changed between identical generation cycles:\nfirst:\n%s\nsecond:\n%s", first, second)
	}

	var doc struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(first, &doc); err != nil {
		t.Fatalf("parse merged openapi: %v", err)
	}
	for _, path := range []string{"/v1/iam/users", "/api/configs", "/api/schemas", "/api/secrets"} {
		if _, ok := doc.Paths[path]; !ok {
			t.Fatalf("merged openapi missing %s; paths=%v", path, doc.Paths)
		}
	}
}

func runOpenAPIGenerateDescribeCycle(t *testing.T, projectPath string, staticSpec, runtimeSpec []byte) []byte {
	t.Helper()
	genDir := filepath.Join(projectPath, ".gen")
	schemaDir := filepath.Join(genDir, "schema")
	openAPIPath := filepath.Join(schemaDir, "openapi.json")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(openAPIPath, staticSpec, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectPath, "schema"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "schema", "openapi.json"), staticSpec, 0o644); err != nil {
		t.Fatal(err)
	}
	snap := snapshotSchemaDir(schemaDir)
	if err := os.WriteFile(openAPIPath, runtimeSpec, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := collectArtifacts(projectPath, genDir, true, snap, nil); err != nil {
		t.Fatalf("collectArtifacts: %v", err)
	}
	merged, err := os.ReadFile(filepath.Join(projectPath, "schema", "openapi.json"))
	if err != nil {
		t.Fatalf("read merged openapi: %v", err)
	}
	genMerged, err := os.ReadFile(openAPIPath)
	if err != nil {
		t.Fatalf("read generated merged openapi: %v", err)
	}
	if !bytes.Equal(merged, genMerged) {
		t.Fatalf(".gen and committed openapi differ:\n.gen:\n%s\ncommitted:\n%s", genMerged, merged)
	}
	return merged
}

func TestCollectArtifactsFailsWhenUnknownSchemaProducerOverwrites(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "single-schema-committer", "an-unknown-schema-producer-overwriting-a-committed-file-is-an-error")
	projectPath := t.TempDir()
	genDir := filepath.Join(projectPath, ".gen")
	schemaDir := filepath.Join(genDir, "schema")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(schemaDir, "other.json")
	if err := os.WriteFile(path, []byte(`{"from":"generate"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := snapshotSchemaDir(schemaDir)
	if err := os.WriteFile(path, []byte(`{"from":"describe-phase"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := collectArtifacts(projectPath, genDir, true, snap, nil)
	if err == nil {
		t.Fatal("expected error for non-openapi duplicate producer")
	}
	if !strings.Contains(err.Error(), "multiple schema producers wrote schema/other.json") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestCollectArtifactsTakesFreshCapabilitiesOverStaleGen reproduces the
// incremental-rebuild scenario: capabilities.json has a single producer (the
// describe binary), so a stale .gen copy from a prior build (the .gen dir is
// not cleared between builds) must be overwritten by the fresh describe output,
// NOT rejected as a competing producer.
func TestCollectArtifactsTakesFreshCapabilitiesOverStaleGen(t *testing.T) {
	projectPath := t.TempDir()
	genDir := filepath.Join(projectPath, ".gen")
	schemaDir := filepath.Join(genDir, "schema")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(schemaDir, "capabilities.json")
	// V1 — describe's output from a prior incremental build.
	if err := os.WriteFile(path, []byte(`{"protocolVersion":1,"project":"a"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := snapshotSchemaDir(schemaDir)
	// V2 — this build's describe output after a manifest-changing source edit.
	v2 := `{"protocolVersion":1,"project":"a","migrations":[{"name":"iam"}]}` + "\n"
	if err := os.WriteFile(path, []byte(v2), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := collectArtifacts(projectPath, genDir, true, snap, nil); err != nil {
		t.Fatalf("incremental rebuild that changes the manifest must not conflict: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != v2 {
		t.Fatalf("expected the fresh describe output, got: %s", got)
	}
}

// TestCollectArtifactsTakesFreshAPIProtoOverStaleGen covers the same rule for
// the protobuf descriptor: the proto plugin inside the describe binary is its
// only producer, so the copy an earlier build staged in .gen must yield to the
// fresh one. Rejecting it made every provider that changed its proto surface
// fail its next build — the first one wrote the descriptor, the second one
// found a prior copy that disagreed.
func TestCollectArtifactsTakesFreshAPIProtoOverStaleGen(t *testing.T) {
	projectPath := t.TempDir()
	genDir := filepath.Join(projectPath, ".gen")
	schemaDir := filepath.Join(genDir, "schema")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(schemaDir, "api.proto")
	prior := "syntax = \"proto3\";\n\nservice ApiService {\n}\n"
	if err := os.WriteFile(path, []byte(prior), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := snapshotSchemaDir(schemaDir)
	// The provider declared one more route since that build.
	fresh := prior[:len(prior)-2] + "  rpc ListWhoami(ListWhoamiRequest) returns (ListWhoamiReply);\n}\n"
	if err := os.WriteFile(path, []byte(fresh), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := collectArtifacts(projectPath, genDir, true, snap, nil); err != nil {
		t.Fatalf("a provider that adds a route must not conflict with its own prior descriptor: %v", err)
	}
	committed, err := os.ReadFile(filepath.Join(projectPath, "schema", "api.proto"))
	if err != nil {
		t.Fatal(err)
	}
	if string(committed) != fresh {
		t.Fatalf("expected the fresh descriptor, got: %s", committed)
	}
}

// TestCollectArtifactsTakesFreshHTTPRoutesOverCachedStaleGen covers this case:
// build-generate's cache captures the shared .gen tree after an earlier
// describe run, then a later cache hit restores that old route inventory before
// the current describe binary rewrites it. HTTP routes have no static Go
// producer, so the restored copy must yield to the fresh describe output rather
// than being diagnosed as a second producer.
func TestCollectArtifactsTakesFreshHTTPRoutesOverCachedStaleGen(t *testing.T) {
	projectPath := t.TempDir()
	genDir := filepath.Join(projectPath, ".gen")
	schemaDir := filepath.Join(genDir, "schema")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(schemaDir, "http-routes.json")
	if err := os.WriteFile(path, []byte(`{"protocolVersion":1,"digest":"old","routes":[]}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Snapshot after the generate cache hit restored the prior describe output.
	snap := snapshotSchemaDir(schemaDir)
	fresh := `{"protocolVersion":1,"digest":"new","routes":[{"method":"GET","path":"/healthz"}]}` + "\n"
	if err := os.WriteFile(path, []byte(fresh), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := collectArtifacts(projectPath, genDir, true, snap, nil); err != nil {
		t.Fatalf("fresh route inventory after a generate cache restore must not conflict: %v", err)
	}
	for _, wantPath := range []string{path, filepath.Join(projectPath, "schema", "http-routes.json")} {
		got, err := os.ReadFile(wantPath)
		if err != nil {
			t.Fatalf("read %s: %v", wantPath, err)
		}
		if string(got) != fresh {
			t.Fatalf("%s = %s, want fresh describe output %s", wantPath, got, fresh)
		}
	}
}

// TestCollectArtifactsTakesFreshFeatureEvidenceOverStaleGen is the
// feature-evidence twin of the capabilities test above: the evidence files are
// emitted by the same single producer (the describe binary), so a stale .gen
// copy from a prior build must yield to the fresh output instead of failing as
// a false multi-producer conflict. Any change to a project's content restamps
// its evidence sourceBinding, so the first incremental rebuild after it meets
// exactly that stale copy.
func TestCollectArtifactsTakesFreshFeatureEvidenceOverStaleGen(t *testing.T) {
	projectPath := t.TempDir()
	genDir := filepath.Join(projectPath, ".gen")
	evidenceDir := filepath.Join(genDir, "schema", "feature-evidence")
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(evidenceDir, "go-framework.json")
	// V1 — describe's output from a prior incremental build (old sourceBinding).
	if err := os.WriteFile(path, []byte(`{"evidence":[{"source":{"binding":"source-v1:sha256:old"}}]}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := snapshotSchemaDir(filepath.Join(genDir, "schema"))
	// V2 — this build's describe output after the project's content changed.
	v2 := `{"evidence":[{"source":{"binding":"source-v1:sha256:new"}}]}` + "\n"
	if err := os.WriteFile(path, []byte(v2), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := collectArtifacts(projectPath, genDir, true, snap, nil); err != nil {
		t.Fatalf("evidence restamp on incremental rebuild must not conflict: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != v2 {
		t.Fatalf("expected the fresh describe output, got: %s", got)
	}
}

func TestMergeIntoManifestAppendsAndExportsRelative(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "relocatable-generation-manifest", "a-merged-manifest-exports-a-relative-path")
	projectPath := t.TempDir()
	genDir := filepath.Join(projectPath, ".gen")
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Pre-existing manifest (e.g. from the static codegen runner).
	initial := GenerateResult{
		Hash:    "abc",
		Mode:    "build",
		Exports: map[string]string{"foo": "bar"},
		Schemas: []string{"schema/static.json"},
	}
	data, _ := json.Marshal(&initial)
	if err := os.WriteFile(filepath.Join(genDir, "generate-result.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := mergeIntoManifest(projectPath, []string{".gen/schema/api.proto", "schema/api.proto"}); err != nil {
		t.Fatalf("mergeIntoManifest: %v", err)
	}

	merged, err := os.ReadFile(filepath.Join(genDir, "generate-result.json"))
	if err != nil {
		t.Fatalf("read merged manifest: %v", err)
	}
	var got GenerateResult
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Existing schemas stay; describe artifacts are appended.
	if len(got.Schemas) != 3 {
		t.Errorf("schemas = %v, want 3 entries", got.Schemas)
	}
	// Pre-existing exports preserved.
	if got.Exports["foo"] != "bar" {
		t.Errorf("static export overwritten: %v", got.Exports)
	}
	// Describe-emitted .gen/ paths surface as PROJECT-RELATIVE exports keyed by
	// "<basename>-spec". Relative because this file is captured into the cache
	// and restored into other checkouts.
	wantKey := "api-spec"
	if v, ok := got.Exports[wantKey]; !ok {
		t.Errorf("expected export %q, got %v", wantKey, got.Exports)
	} else if v != ".gen/schema/api.proto" {
		t.Errorf("export %q = %q, want .gen/schema/api.proto", wantKey, v)
	}
}

// TestMergeIntoManifestRelativizesLegacyAbsolutePaths covers the manifest
// describe actually finds on disk after a cache restore from an extension that
// predates the project-relative rule: rewriting its absolute values back out verbatim would make
// describe's own output checkout-dependent again, so the merge normalizes the
// whole file, not just the entries it adds.
func TestMergeIntoManifestRelativizesLegacyAbsolutePaths(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "relocatable-generation-manifest", "a-legacy-absolute-path-is-relativized")
	projectPath := t.TempDir()
	genDir := filepath.Join(projectPath, ".gen")
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// elsewhere is absolute on this host and outside the project. A rooted
	// path without a volume name is not absolute on Windows.
	elsewhere := filepath.Join(t.TempDir(), "x.json")
	legacy := GenerateResult{
		Hash: "abc",
		Mode: "build",
		Exports: map[string]string{
			"openapi-spec": filepath.Join(projectPath, ".gen", "schema", "openapi.json"),
			"elsewhere":    elsewhere,
		},
		Assets: map[string]string{
			"schema/http-routes.json": filepath.Join(projectPath, ".gen", "schema", "http-routes.json"),
		},
	}
	data, _ := json.Marshal(&legacy)
	if err := os.WriteFile(filepath.Join(genDir, "generate-result.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := mergeIntoManifest(projectPath, []string{".gen/schema/api.proto"}); err != nil {
		t.Fatalf("mergeIntoManifest: %v", err)
	}

	merged, err := os.ReadFile(filepath.Join(genDir, "generate-result.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The manifest is JSON, so look for the path as JSON spells it: a Windows
	// path's backslashes are escaped there.
	quotedProject, _ := json.Marshal(projectPath)
	if bytes.Contains(merged, quotedProject[1:len(quotedProject)-1]) {
		t.Fatalf("merged manifest still embeds the checkout path:\n%s", merged)
	}
	var got GenerateResult
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatal(err)
	}
	if got.Exports["openapi-spec"] != ".gen/schema/openapi.json" {
		t.Errorf("openapi-spec = %q, want .gen/schema/openapi.json", got.Exports["openapi-spec"])
	}
	if got.Assets["schema/http-routes.json"] != ".gen/schema/http-routes.json" {
		t.Errorf("http-routes asset = %q, want .gen/schema/http-routes.json", got.Assets["schema/http-routes.json"])
	}
	// A path outside the project has no relocatable form; it is kept verbatim
	// rather than rewritten into a "../.." chain that encodes checkout depth.
	if got.Exports["elsewhere"] != elsewhere {
		t.Errorf("out-of-project export was rewritten: %q", got.Exports["elsewhere"])
	}
}

func TestMergeIntoManifestCreatesWhenMissing(t *testing.T) {
	projectPath := t.TempDir()
	genDir := filepath.Join(projectPath, ".gen")
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := mergeIntoManifest(projectPath, []string{".gen/schema/x.json"}); err != nil {
		t.Fatalf("mergeIntoManifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(genDir, "generate-result.json")); err != nil {
		t.Errorf("expected manifest to be created: %v", err)
	}
}

func TestHasMainDetectsRootMainPackage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasMain(dir) {
		t.Error("expected hasMain to detect root main package")
	}
}

func TestHasMainDetectsCmdLayout(t *testing.T) {
	dir := t.TempDir()
	cmdDir := filepath.Join(dir, "cmd", "myapp")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cmdDir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasMain(dir) {
		t.Error("expected hasMain to detect cmd/<name>/main.go layout")
	}
}

func TestHasMainReturnsFalseForLibrary(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lib.go"), []byte("package lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hasMain(dir) {
		t.Error("expected hasMain to return false for library project")
	}
}

func TestResolveDescribeEntrypointUsesExplicitDescribeOption(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "explicit-entrypoint", "describe-uses-its-own-explicit-entrypoint-option")
	dir := t.TempDir()
	writeCmdMain(t, dir, "api")
	writeCmdMain(t, dir, "migrate")

	ctx := &pctx.Context{
		Project: pctx.Project{
			Name: "control-plane-api",
			Options: map[string]json.RawMessage{
				"@putnami/go": json.RawMessage(`{"describe":{"entrypoint":"./cmd/api"}}`),
			},
		},
		Params: pctx.Params{},
	}

	got, err := resolveDescribeEntrypoint(ctx, dir)
	if err != nil {
		t.Fatalf("resolveDescribeEntrypoint: %v", err)
	}
	if got != "./cmd/api" {
		t.Fatalf("entrypoint = %q, want ./cmd/api", got)
	}
}

func TestResolveDescribeEntrypointUsesBuildEntrypointFallback(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "explicit-entrypoint", "describe-falls-back-to-the-build-entrypoint")
	dir := t.TempDir()
	writeCmdMain(t, dir, "api")
	writeCmdMain(t, dir, "migrate")

	ctx := &pctx.Context{
		Project: pctx.Project{
			Name: "control-plane-api",
			Options: map[string]json.RawMessage{
				"@putnami/go": json.RawMessage(`{"entrypoint":"./cmd/api"}`),
			},
		},
		Params: pctx.Params{},
	}

	got, err := resolveDescribeEntrypoint(ctx, dir)
	if err != nil {
		t.Fatalf("resolveDescribeEntrypoint: %v", err)
	}
	if got != "./cmd/api" {
		t.Fatalf("entrypoint = %q, want ./cmd/api", got)
	}
}

func TestResolveDescribeEntrypointRejectsAmbiguousCmdLayout(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "explicit-entrypoint", "describe-rejects-an-ambiguous-cmd-layout")
	dir := t.TempDir()
	writeCmdMain(t, dir, "api")
	writeCmdMain(t, dir, "migrate")

	ctx := &pctx.Context{Project: pctx.Project{Name: "control-plane-api"}, Params: pctx.Params{}}
	_, err := resolveDescribeEntrypoint(ctx, dir)
	if err == nil {
		t.Fatal("expected ambiguous cmd layout to fail")
	}
	if !strings.Contains(err.Error(), "set options.@putnami/go.describe.entrypoint") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func writeCmdMain(t *testing.T, projectPath, name string) {
	t.Helper()
	cmdDir := filepath.Join(projectPath, "cmd", name)
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cmdDir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFwVersionSuffixFormatting(t *testing.T) {
	if got := fwVersionSuffix(""); got != "" {
		t.Errorf("empty version → %q, want empty string", got)
	}
	if got, want := fwVersionSuffix("v0.1.0-4385905a"), " (go.putnami.dev/app v0.1.0-4385905a)"; got != want {
		t.Errorf("fwVersionSuffix = %q, want %q", got, want)
	}
}

func TestAcquireDescribeLockSerializesConcurrentRuns(t *testing.T) {
	genDir := t.TempDir()

	// First holder takes the lock and pretends to do work.
	unlock1, err := acquireDescribeLock(genDir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}

	// Second acquirer must block until the first releases. We assert the
	// blocking behavior by attempting to acquire from a goroutine and
	// confirming it doesn't return within a short window.
	got := make(chan error, 1)
	go func() {
		unlock2, err := acquireDescribeLock(genDir)
		if err == nil {
			unlock2()
		}
		got <- err
	}()

	select {
	case err := <-got:
		t.Fatalf("second acquire returned early (err=%v); expected to block", err)
	case <-time.After(100 * time.Millisecond):
	}

	unlock1()

	select {
	case err := <-got:
		if err != nil {
			t.Errorf("second acquire after release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second acquire did not unblock after first released")
	}
}

func TestAcquireDescribeLockSurvivesParallelContention(t *testing.T) {
	genDir := t.TempDir()
	const goroutines = 8

	var (
		wg     sync.WaitGroup
		active int32 // accessed under mu
		max    int32
		mu     sync.Mutex
	)

	for range goroutines {
		wg.Go(func() {
			unlock, err := acquireDescribeLock(genDir)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			mu.Lock()
			active++
			if active > max {
				max = active
			}
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
			unlock()
		})
	}
	wg.Wait()

	if max != 1 {
		t.Errorf("max concurrent holders = %d, want 1 (lock failed to serialize)", max)
	}
}

// runDescribeBinary uses os/exec so we exercise it with real shell scripts that
// stand in for too-old / well-behaved binaries. Skip on Windows where the
// /bin/sh shim is unavailable; the codepath itself is platform-neutral.
// abortWallBound is how long the abort tests below may take before the wall
// clock stops distinguishing "aborted on the ready marker" from "waited the
// stand-in binary out". It is a scheduling allowance, not a performance target:
// the drain it covers is capped at 250ms, while a genuine regression parks on
// the stand-in's 120-second sleep — so any value between the two proves the
// same thing.
//
// It used to be 5s against a 30s sleep. Six times the margin sounds ample and
// is not: these tests spawn a shell, a descendant and a pipe drain, and their
// wall clock tracks how much CPU the test job was granted, not whether the
// abort works. A `go test` running its packages in parallel, coverage
// instrumentation, or a CPU-quota-limited CI host each cost multiple seconds
// here, and the resulting red gate says nothing about the invariant. Widening
// the gap between the two outcomes is what makes the signal robust; tightening
// the bound would only buy more false alarms.
const abortWallBound = 60 * time.Second

// testDescribeMaxRuntime is the run cap injected into runDescribeBinary by the
// tests below in place of the production describeMaxRuntime (60s). The
// production cap is a guard against a binary that hangs in Configure; here the
// stand-in scripts either exit immediately or emit the ready marker and park,
// so the cap only matters as a backstop — and on a loaded machine (this repo's
// gate shares 10 cores with other -race suites) 15s of scheduling delay before
// the marker line is even read is entirely possible. When that happened the
// select in runDescribeBinary took the timeout branch and the test failed with
// "describe run timed out after 15s" instead of exercising the abort path.
// The value sits above abortWallBound (so the wall assertion, not the cap, is
// what times a healthy abort) and below the stand-ins' 120s sleep (so a
// regressed abort still ends in a timeout error the assertions reject rather
// than parking the suite).
const testDescribeMaxRuntime = 90 * time.Second

func TestRunDescribeBinaryAbortsWhenReadyMarkerSeen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stand-in not portable to Windows")
	}

	binPath := writeShellBinary(t, `#!/bin/sh
echo '{"level":"info","msg":"🤖 ready","duration":42}'
sleep 120
`)
	projectPath := t.TempDir()
	genDir := filepath.Join(projectPath, ".gen")
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	out, err := runDescribeBinary(binPath, projectPath, genDir, "v0.1.0-4385905a", testDescribeMaxRuntime)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error when ready marker is observed")
	}
	if !strings.Contains(err.Error(), "did not honor PUTNAMI_DESCRIBE") {
		t.Errorf("error %q does not mention the misbehavior", err)
	}
	if !strings.Contains(err.Error(), "v0.1.0-4385905a") {
		t.Errorf("error %q does not name the framework version", err)
	}
	if !strings.Contains(out, "🤖 ready") {
		t.Errorf("captured output missing the marker line: %q", out)
	}
	if elapsed > abortWallBound {
		t.Errorf("abort took %s, want under %s (did not wait out the binary)", elapsed, abortWallBound)
	}
}

func TestRunDescribeBinaryAbortsWithBackgroundDescendant(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stand-in not portable to Windows")
	}

	// The descendant outlives the abort by a wide margin on purpose. What this
	// test proves is that the abort does not wait for an inherited descriptor
	// held by a process that is going to sit there — so the useful signal is the
	// SEPARATION between "aborted" and "waited for the descendant", not a tight
	// wall-clock number. A 30s sleep judged against a 5s bound left only 6× of
	// room, which a test job running its packages in parallel (or a CPU-quota
	// limited CI host) can eat through without the abort having regressed at all;
	// the failure mode was a red gate that said nothing about the invariant.
	// The 120s sleep against abortWallBound keeps the same proof with a wide
	// margin: a regression that actually blocks reads as the injected
	// testDescribeMaxRuntime cap firing (~90s), nowhere near the bound.
	descendantPIDFile := filepath.Join(t.TempDir(), "descendant.pid")
	binPath := writeShellBinary(t, `#!/bin/sh
(trap '' HUP; sleep 120) &
descendant_pid=$!
echo "$descendant_pid" > "`+descendantPIDFile+`"
echo '{"level":"info","msg":"🤖 ready"}'
wait "$descendant_pid"
`)
	t.Cleanup(func() {
		body, err := os.ReadFile(descendantPIDFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
		if err != nil {
			return
		}
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Kill()
		}
	})

	projectPath := t.TempDir()
	genDir := filepath.Join(projectPath, ".gen")
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	out, err := runDescribeBinary(binPath, projectPath, genDir, "", testDescribeMaxRuntime)
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "did not honor PUTNAMI_DESCRIBE") {
		t.Fatalf("error = %v, want ready-marker error", err)
	}
	if !strings.Contains(out, readyMarker) {
		t.Fatalf("captured output missing ready marker: %q", out)
	}
	if elapsed > abortWallBound {
		t.Fatalf("abort with inherited descriptors took %s, want under %s (did not block on the descendant)",
			elapsed, abortWallBound)
	}
}

func TestRunDescribeBinarySetsZeroPorts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stand-in not portable to Windows")
	}

	// The shim writes the env it observed to a sentinel file, then exits 0
	// (well-behaved binary case).
	envFile := filepath.Join(t.TempDir(), "env.txt")
	binPath := writeShellBinary(t, `#!/bin/sh
{ echo "PORT=$PORT"; echo "GRPC_PORT=$GRPC_PORT"; echo "PUTNAMI_DESCRIBE=$PUTNAMI_DESCRIBE"; } > "`+envFile+`"
exit 0
`)
	projectPath := t.TempDir()
	genDir := filepath.Join(projectPath, ".gen")
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := runDescribeBinary(binPath, projectPath, genDir, "", testDescribeMaxRuntime); err != nil {
		t.Fatalf("runDescribeBinary: %v", err)
	}

	body, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env sentinel: %v", err)
	}
	got := string(body)
	for _, want := range []string{"PORT=0", "GRPC_PORT=0", "PUTNAMI_DESCRIBE=all"} {
		if !strings.Contains(got, want) {
			t.Errorf("describe env missing %q; got:\n%s", want, got)
		}
	}
}

func TestRunDescribeBinaryPropagatesNonzeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stand-in not portable to Windows")
	}

	binPath := writeShellBinary(t, `#!/bin/sh
echo "boom" >&2
exit 2
`)
	projectPath := t.TempDir()
	genDir := filepath.Join(projectPath, ".gen")
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := runDescribeBinary(binPath, projectPath, genDir, "", testDescribeMaxRuntime)
	if err == nil {
		t.Fatal("expected non-nil error for exit-2 binary")
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("captured output missing stderr line: %q", out)
	}
	exitError := &exec.ExitError{}
	if !errors.As(err, &exitError) {
		t.Errorf("expected *exec.ExitError, got %T (%v)", err, err)
	}
}

func TestDescribeOutputWriterDetectsReadyMarkerAcrossWrites(t *testing.T) {
	readyCh := make(chan struct{}, 1)
	output := newDescribeOutputWriter(readyCh)

	for _, chunk := range []string{"stdout before\n🤖 re", "ady\nstderr after\n"} {
		if _, err := output.Write([]byte(chunk)); err != nil {
			t.Fatalf("write chunk: %v", err)
		}
	}

	select {
	case <-readyCh:
	default:
		t.Fatal("ready marker split across writes was not detected")
	}
	if got, want := output.String(), "stdout before\n🤖 ready\nstderr after\n"; got != want {
		t.Fatalf("captured output = %q, want %q", got, want)
	}

	if _, err := output.Write([]byte(readyMarker)); err != nil {
		t.Fatalf("write repeated marker: %v", err)
	}
	select {
	case <-readyCh:
		t.Fatal("ready marker was signaled more than once")
	default:
	}
}

func TestDescribeOutputWriterRetainsConcurrentWrites(t *testing.T) {
	const (
		writers         = 32
		writesPerWriter = 100
		payload         = "complete-write\n"
	)
	output := newDescribeOutputWriter(make(chan struct{}, 1))
	start := make(chan struct{})
	errCh := make(chan error, writers)
	var wg sync.WaitGroup
	wg.Add(writers)
	for range writers {
		go func() {
			defer wg.Done()
			<-start
			for range writesPerWriter {
				n, err := output.Write([]byte(payload))
				if err != nil {
					errCh <- err
					return
				}
				if n != len(payload) {
					errCh <- fmt.Errorf("write length = %d, want %d", n, len(payload))
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	got := output.String()
	wantWrites := writers * writesPerWriter
	if count := strings.Count(got, payload); count != wantWrites {
		t.Fatalf("complete payload count = %d, want %d", count, wantWrites)
	}
	if wantLen := wantWrites * len(payload); len(got) != wantLen {
		t.Fatalf("captured output length = %d, want %d", len(got), wantLen)
	}
}

// writeShellBinary writes a /bin/sh script as an executable temp file that the
// test cleans up, and returns its path. Used to simulate child-binary behavior
// without compiling a full Go binary in the test.
func writeShellBinary(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-binary")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDescribeWillCommit(t *testing.T) {
	const appBlock = "module example.com/x\n\nrequire (\n\tgo.putnami.dev/app v0.1.0\n)\n"
	const noApp = "module example.com/x\n"
	cases := []struct {
		name    string
		gomod   string
		hasMain bool
		want    bool
	}{
		{"app with main package", appBlock, true, true},
		{"app without main (library)", appBlock, false, false},
		{"main without framework app (plain CLI)", noApp, true, false},
		{"neither", noApp, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(tc.gomod), 0o644); err != nil {
				t.Fatal(err)
			}
			name, body := "lib.go", "package lib\n"
			if tc.hasMain {
				name, body = "main.go", "package main\n"
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := describeWillCommit(dir); got != tc.want {
				t.Errorf("describeWillCommit = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStagedSchemaArtifacts(t *testing.T) {
	projectPath := t.TempDir()
	genDir := filepath.Join(projectPath, ".gen")
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// A missing manifest must yield nil so describe falls back to legacy
	// behavior (commit only what its own binary touched).
	if got := stagedSchemaArtifacts(projectPath); got != nil {
		t.Errorf("missing manifest: got %v, want nil", got)
	}

	manifest := GenerateResult{Schemas: []string{
		".gen/schema/openapi.json", // staged under .gen/schema -> tracked
		".gen/schema/api.proto",    // staged under .gen/schema -> tracked
		"schema/config.json",       // committed path, not a .gen stage -> ignored
		".gen/config-schema.json",  // under .gen but not schema/ -> ignored
	}}
	data, _ := json.Marshal(&manifest)
	if err := os.WriteFile(filepath.Join(genDir, "generate-result.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	got := stagedSchemaArtifacts(projectPath)
	want := map[string]bool{"schema/openapi.json": true, "schema/api.proto": true}
	if len(got) != len(want) {
		t.Fatalf("staged = %v, want %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("staged missing %q; got %v", k, got)
		}
	}
}

// TestCollectArtifactsCommitsStagedStubDescribeLeftUntouched is the core single-committer
// guarantee for the OpenAPI path: build-generate staged a static stub and
// deferred committing it; the describe binary emitted nothing for it (an app
// with HTTP routes but no runtime OpenAPI plugin). describe is the sole
// committer, so it must still promote the staged stub to the tracked tree —
// otherwise a deferred-but-never-committed file would dirty/clear the worktree.
func TestCollectArtifactsCommitsStagedStubDescribeLeftUntouched(t *testing.T) {
	projectPath := t.TempDir()
	genDir := filepath.Join(projectPath, ".gen")
	schemaDir := filepath.Join(genDir, "schema")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := []byte(`{
  "openapi": "3.0.3",
  "info": { "title": "x", "version": "1.0.0" },
  "paths": {
    "/v1/things": {
      "get": { "operationId": "g", "responses": { "200": { "description": "ok" } } }
    }
  }
}`)
	if err := os.WriteFile(filepath.Join(schemaDir, "openapi.json"), stub, 0o644); err != nil {
		t.Fatal(err)
	}

	// Snapshot taken after generate staged the stub; the describe binary then
	// runs and changes nothing under schema/ (changedSince == false).
	snap := snapshotSchemaDir(schemaDir)
	staged := map[string]bool{"schema/openapi.json": true}

	artifacts, err := collectArtifacts(projectPath, genDir, true, snap, staged)
	if err != nil {
		t.Fatalf("collectArtifacts: %v", err)
	}

	committed, err := os.ReadFile(filepath.Join(projectPath, "schema", "openapi.json"))
	if err != nil {
		t.Fatalf("expected committed stub: %v", err)
	}
	wantCanon, err := openapiutil.Canonicalize(stub)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, wantCanon) {
		t.Errorf("committed stub not canonical:\n got: %s\nwant: %s", committed, wantCanon)
	}

	// The committed promotion is recorded, but a staged-but-unchanged file is
	// NOT re-listed as a describe-produced .gen artifact (generate already
	// listed it in the manifest).
	foundCommit := false
	for _, a := range artifacts {
		switch a {
		case filepath.Join("schema", "openapi.json"):
			foundCommit = true
		case filepath.Join(".gen", "schema", "openapi.json"):
			t.Errorf("staged-unchanged file re-listed as a describe .gen artifact: %v", artifacts)
		}
	}
	if !foundCommit {
		t.Errorf("expected committed openapi.json in artifacts, got %v", artifacts)
	}
}

// TestCollectArtifactsIgnoresUnstagedUntouchedFiles guards against describe
// resurrecting stale .gen/ leftovers: a file neither staged by generate this
// build nor changed by the describe binary must never be committed.
func TestCollectArtifactsIgnoresUnstagedUntouchedFiles(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "single-schema-committer", "an-unchanged-untouched-file-is-left-alone")
	projectPath := t.TempDir()
	genDir := filepath.Join(projectPath, ".gen")
	schemaDir := filepath.Join(genDir, "schema")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Stale leftover from a prior build whose producer was since removed.
	if err := os.WriteFile(filepath.Join(schemaDir, "stale.json"), []byte(`{"old":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := snapshotSchemaDir(schemaDir)

	// staged points only at a different file; stale.json is untouched + unstaged.
	staged := map[string]bool{"schema/openapi.json": true}
	artifacts, err := collectArtifacts(projectPath, genDir, true, snap, staged)
	if err != nil {
		t.Fatalf("collectArtifacts: %v", err)
	}

	if _, err := os.Stat(filepath.Join(projectPath, "schema", "stale.json")); !os.IsNotExist(err) {
		t.Errorf("stale leftover must not be committed, got err=%v", err)
	}
	for _, a := range artifacts {
		if strings.Contains(a, "stale.json") {
			t.Errorf("stale leftover leaked into artifacts: %v", artifacts)
		}
	}
}

// The describe host binary is built at a stable per-project path under the job's
// scratch root so `go build` can recognize an up-to-date target and skip
// materializing the linked binary. These tests pin the two halves that a
// refactor can silently lose: the target must be stable and sandboxed, and the
// reuse must never hand describe a binary that no longer matches the source.

func TestHostBinaryTargetIsStablePerProjectAndSandboxed(t *testing.T) {
	scratch := filepath.Join("/tmp", "scratch")
	first := hostBinaryTarget(&pctx.Context{
		CacheRoot: scratch,
		Project:   pctx.Project{Path: "go/samples/service-to-service"},
	})
	again := hostBinaryTarget(&pctx.Context{
		CacheRoot: scratch,
		Project:   pctx.Project{Path: "go/samples/service-to-service"},
	})
	if first == "" || first != again {
		t.Fatalf("target must be stable for one project: %q then %q", first, again)
	}
	want := filepath.Join(scratch, describeHostBinaryDir, filepath.FromSlash("go/samples/service-to-service"), describeHostBinaryName)
	if first != want {
		t.Fatalf("target = %q, want %q", first, want)
	}

	sibling := hostBinaryTarget(&pctx.Context{
		CacheRoot: scratch,
		Project:   pctx.Project{Path: "go/framework/api"},
	})
	if sibling == first {
		t.Fatalf("two projects share one target (%q); one describe would overwrite the other's binary", first)
	}

	// Every case that cannot produce a sandboxed stable path must yield "", which
	// falls the caller back to a throwaway temporary file.
	notLocal := map[string]*pctx.Context{
		"nil context":      nil,
		"no scratch root":  {Project: pctx.Project{Path: "go/framework/api"}},
		"no project path":  {CacheRoot: scratch},
		"escaping project": {CacheRoot: scratch, Project: pctx.Project{Path: "../../elsewhere"}},
		"absolute project": {CacheRoot: scratch, Project: pctx.Project{Path: string(filepath.Separator) + filepath.Join("etc", "cron.d")}},
	}
	if runtime.GOOS == "windows" {
		notLocal["volume-relative project"] = &pctx.Context{CacheRoot: scratch, Project: pctx.Project{Path: "C:elsewhere"}}
		notLocal["reserved device project"] = &pctx.Context{CacheRoot: scratch, Project: pctx.Project{Path: "apps/NUL"}}
	}
	for name, ctx := range notLocal {
		if got := hostBinaryTarget(ctx); got != "" {
			t.Errorf("%s: target = %q, want the temporary-file fallback", name, got)
		}
	}
}

// newDescribeHostModule writes a trivial main package whose output embeds body,
// so a rebuild is observable in the binary's own bytes.
func newDescribeHostModule(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	mustWriteDescribeFile(t, filepath.Join(dir, "go.mod"), "module example.com/describehost\n\ngo 1.24.0\n")
	writeDescribeHostMain(t, dir, body)
	return dir
}

func writeDescribeHostMain(t *testing.T, dir, body string) {
	t.Helper()
	mustWriteDescribeFile(t, filepath.Join(dir, "main.go"),
		"package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\""+body+"\") }\n")
}

func mustWriteDescribeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hostBinaryDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatalf("read host binary: %v", err)
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

func TestCompileHostBinaryReusesAnUpToDateTargetAndRebuildsOnSourceChange(t *testing.T) {
	if _, err := toolchain.ResolveGo(); err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}
	projectDir := newDescribeHostModule(t, "first")
	scratch := t.TempDir()
	ctx := &pctx.Context{CacheRoot: scratch, Project: pctx.Project{Path: "svc"}, Params: pctx.Params{}}
	target := hostBinaryTarget(ctx)

	binPath, release, err := compileHostBinary(ctx, projectDir)
	if err != nil {
		t.Fatalf("compileHostBinary: %v", err)
	}
	if binPath != target {
		t.Fatalf("binary at %q, want the stable target %q", binPath, target)
	}
	release()
	first, err := os.Stat(binPath)
	if err != nil {
		t.Fatalf("release must keep the stable target for the next describe: %v", err)
	}
	firstDigest := hostBinaryDigest(t, binPath)

	// Nothing changed: `go build` must recognize its own build ID in the target
	// and skip the link entirely. It does relink into a fresh file and rename it
	// over the target when it cannot, so file IDENTITY is the observable that
	// says the link was skipped — Go touches the modification time either way,
	// which is why that is not the thing to assert.
	binPath, release, err = compileHostBinary(ctx, projectDir)
	if err != nil {
		t.Fatalf("second compileHostBinary: %v", err)
	}
	release()
	second, err := os.Stat(binPath)
	if err != nil {
		t.Fatalf("stat after reuse: %v", err)
	}
	if !os.SameFile(first, second) {
		t.Error("an unchanged source relinked the host binary over the stable target; the stable target bought nothing")
	}
	if got := hostBinaryDigest(t, binPath); got != firstDigest {
		t.Errorf("unchanged source produced a different binary: %s then %s", firstDigest, got)
	}

	// Changed source: reuse must never win. The binary describe runs has to be
	// the one this source builds, or the job would report a contract derived
	// from code that is no longer there.
	writeDescribeHostMain(t, projectDir, "second")
	binPath, release, err = compileHostBinary(ctx, projectDir)
	if err != nil {
		t.Fatalf("compileHostBinary after edit: %v", err)
	}
	defer release()
	if binPath != target {
		t.Fatalf("rebuild moved off the stable target: %q", binPath)
	}
	if got := hostBinaryDigest(t, binPath); got == firstDigest {
		t.Fatal("a source change reused the previous host binary; describe would report a stale contract")
	}
}

func TestCompileHostBinaryFallsBackToATemporaryFileWithoutAScratchRoot(t *testing.T) {
	if _, err := toolchain.ResolveGo(); err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}
	projectDir := newDescribeHostModule(t, "no-scratch")
	ctx := &pctx.Context{Project: pctx.Project{Path: "svc"}, Params: pctx.Params{}}

	binPath, release, err := compileHostBinary(ctx, projectDir)
	if err != nil {
		t.Fatalf("compileHostBinary: %v", err)
	}
	if strings.HasPrefix(binPath, projectDir) {
		t.Errorf("fallback binary %q landed in the project tree", binPath)
	}
	if _, err := os.Stat(binPath); err != nil {
		t.Fatalf("fallback binary missing: %v", err)
	}
	release()
	if _, err := os.Stat(binPath); !os.IsNotExist(err) {
		t.Errorf("release must delete the throwaway binary, stat err = %v", err)
	}
}

func TestCompileHostBinaryLeavesNoTargetWhenTheBuildFails(t *testing.T) {
	if _, err := toolchain.ResolveGo(); err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}
	projectDir := newDescribeHostModule(t, "good")
	scratch := t.TempDir()
	ctx := &pctx.Context{CacheRoot: scratch, Project: pctx.Project{Path: "svc"}, Params: pctx.Params{}}

	if _, release, err := compileHostBinary(ctx, projectDir); err != nil {
		t.Fatalf("compileHostBinary: %v", err)
	} else {
		release()
	}

	mustWriteDescribeFile(t, filepath.Join(projectDir, "main.go"), "package main\n\nfunc main() { this is not go }\n")
	if _, _, err := compileHostBinary(ctx, projectDir); err == nil {
		t.Fatal("expected a compile failure")
	}
	if _, err := os.Stat(hostBinaryTarget(ctx)); !os.IsNotExist(err) {
		t.Errorf("a failed build must not leave a target behind, stat err = %v", err)
	}
}

// TestDescribeAndCompileBuildTheSameProgram pins that describe and compile
// build the same program, and it asserts it in the two binaries.
//
// `build~describe` compiles the workload's entrypoint to run the Describer
// plugins, and `build~compile` compiles the same entrypoint moments later,
// through the same GOCACHE. Go serves the second build from that cache only
// for packages whose action ID matches, and an action ID folds CGO_ENABLED,
// -tags, -gcflags, -asmflags, -race and -trimpath. Describe used to force
// CGO_ENABLED=0 and pass no flag, so on a host with a C compiler — every
// hosted Linux runner — `net` and every package above it compiled twice per
// run.
//
// The two binaries' recorded build settings are the observable: they are what
// `cmd/go` hashed. -ldflags is excluded on purpose (it reaches the link action
// alone, so the version stamp costs describe nothing), and so are the vcs.*
// settings, which describe the checkout rather than the build.
func TestDescribeAndCompileBuildTheSameProgram(t *testing.T) {
	if _, err := toolchain.ResolveGo(); err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}
	projectDir := newDescribeHostModule(t, "parity")
	params := pctx.Params{
		"cgo":      []byte(`true`),
		"tags":     []byte(`"integration"`),
		"trimpath": []byte(`true`),
		"gcflags":  []byte(`"-N -l"`),
	}
	ctx := &pctx.Context{
		CacheRoot:     t.TempDir(),
		WorkspaceRoot: projectDir,
		OutputPath:    t.TempDir(),
		Project:       pctx.Project{Name: "@putnami/parity", Path: "svc", FullPath: projectDir, Type: "application"},
		Params:        params,
	}

	describeBinary, release, err := compileHostBinary(ctx, projectDir)
	if err != nil {
		t.Fatalf("compileHostBinary: %v", err)
	}
	defer release()

	// The compile step's own build of the same entrypoint, with the version
	// stamp it always carries.
	compileArgs, compileEnv := toolchain.HostBuildInvocation(ctx, nil, os.Environ(), projectDir, mustResolveGo(t))
	compileBinary := filepath.Join(t.TempDir(), "compiled")
	args := append([]string{"build", "-o", compileBinary}, compileArgs...)
	args = append(args, "-ldflags", "-X main.Version=9.9.9", ".")
	cmd := exec.Command(mustResolveGo(t), args...) //nolint:gosec // test-owned argv
	cmd.Dir = projectDir
	cmd.Env = compileEnv
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile build: %v: %s", err, out)
	}

	describeSettings := hostBuildSettings(t, describeBinary)
	compileSettings := hostBuildSettings(t, compileBinary)
	if !reflect.DeepEqual(describeSettings, compileSettings) {
		t.Errorf("describe built a different program than compile:\n  describe %v\n  compile  %v",
			describeSettings, compileSettings)
	}
	for _, want := range []string{"-tags=integration", "-trimpath=true", "CGO_ENABLED=1"} {
		if !slices.Contains(describeSettings, want) {
			t.Errorf("describe settings %v do not carry %q; the parameters never reached the build", describeSettings, want)
		}
	}
}

// hostBuildSettings returns the binary's recorded build settings, minus the
// ones that are not part of what `cmd/go` hashes into a compile action:
// -ldflags (link only) and the vcs.* stamps (the checkout, not the build).
func hostBuildSettings(t *testing.T, binPath string) []string {
	t.Helper()
	info, err := buildinfo.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read build info of %s: %v", binPath, err)
	}
	settings := make([]string, 0, len(info.Settings))
	for _, setting := range info.Settings {
		if setting.Key == "-ldflags" || strings.HasPrefix(setting.Key, "vcs") {
			continue
		}
		settings = append(settings, setting.Key+"="+setting.Value)
	}
	sort.Strings(settings)
	return settings
}

func mustResolveGo(t *testing.T) string {
	t.Helper()
	goBinary, err := toolchain.ResolveGo()
	if err != nil {
		t.Fatalf("resolve go: %v", err)
	}
	return goBinary
}
