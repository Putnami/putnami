package extension

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/layout"
)

// The CLI ↔ extension contract-safety tests: discovery must never
// silently drop an extension — genuinely-unloadable ones become Skipped
// records that gates and guards chain into their errors, and older-contract
// manifests load with their reserved-flag shadows adapted away.

func writeExtensionManifest(t *testing.T, dir, manifest string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverExtensionsDetailed_SkipsContractTooNew(t *testing.T) {
	dir := t.TempDir()
	// Installed lock-pinned layout: .putnami/bin/extensions/@putnami/cloud
	stableDir := layout.StableDir(dir, layout.Extensions, "@putnami/cloud")
	writeExtensionManifest(t, stableDir, fmt.Sprintf(`{
		"name": "@putnami/cloud",
		"version": "9.9.9",
		"cliContract": %d,
		"commands": {"login": {"run": [{"id": "l", "task": "t"}]}},
		"tasks": {"t": {"kind": "command", "command": "echo"}}
	}`, protocolcli.LatestContract+1))

	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{"@putnami/cloud": ""}}}
	result, err := DiscoverExtensionsDetailed(dir, cfg, nil)
	if err != nil {
		t.Fatalf("DiscoverExtensionsDetailed: %v", err)
	}
	if len(result.Extensions) != 0 {
		t.Fatalf("a contract-too-new extension must not load, got %d extensions", len(result.Extensions))
	}
	if len(result.Skipped) != 1 {
		t.Fatalf("expected 1 skip record, got %v", result.Skipped)
	}
	skip := result.Skipped[0]
	if skip.Name != "@putnami/cloud" || skip.Ref != "@putnami/cloud" {
		t.Errorf("skip identity = %q/%q, want @putnami/cloud", skip.Name, skip.Ref)
	}
	if skip.Reason == nil || !strings.Contains(skip.Reason.Error(), "requires a newer putnami") {
		t.Errorf("skip reason should cite contract-too-new, got %v", skip.Reason)
	}
}

func TestDiscoverExtensionsDetailed_SkipsUnparseableManifest(t *testing.T) {
	dir := t.TempDir()
	writeExtensionManifest(t, filepath.Join(dir, "ext"), "{not json")

	result, err := DiscoverExtensionsDetailed(dir, &wsproto.Config{}, []string{"ext"})
	if err != nil {
		t.Fatalf("DiscoverExtensionsDetailed: %v", err)
	}
	if len(result.Extensions) != 0 {
		t.Fatalf("an unparseable extension must not load, got %d extensions", len(result.Extensions))
	}
	if len(result.Skipped) != 1 {
		t.Fatalf("expected 1 skip record, got %v", result.Skipped)
	}
	if result.Skipped[0].Ref != "ext" {
		t.Errorf("skip Ref = %q, want ext", result.Skipped[0].Ref)
	}
}

func TestDiscoverExtensionsDetailed_LoadedRefIsNotSkipped(t *testing.T) {
	dir := t.TempDir()
	// The workspace-path probe fails to parse, but the installed layout loads:
	// the ref resolved, so it must not surface as skipped.
	writeExtensionManifest(t, filepath.Join(dir, "cloud"), "{not json")
	stableDir := layout.StableDir(dir, layout.Extensions, "cloud")
	writeExtensionManifest(t, stableDir, `{
		"name": "@putnami/cloud",
		"cliContract": 4,
		"commands": {"login": {"run": [{"id": "l", "task": "t"}]}},
		"tasks": {"t": {"kind": "command", "command": "echo"}}
	}`)

	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{"cloud": ""}}}
	result, err := DiscoverExtensionsDetailed(dir, cfg, nil)
	if err != nil {
		t.Fatalf("DiscoverExtensionsDetailed: %v", err)
	}
	if len(result.Extensions) != 1 || result.Extensions[0].Name != "@putnami/cloud" {
		t.Fatalf("extension should load from the installed layout, got %v", result.Extensions)
	}
	if result.Extensions[0].LocalSource {
		t.Error("installed extension should not be marked as a mutable local source")
	}
	if len(result.Skipped) != 0 {
		t.Fatalf("a ref that ultimately loaded must not be skipped, got %v", result.Skipped)
	}
}

// TestDiscoverExtensionsDetailed_SkipsOlderContractManifest is the converted
// tolerate-v2 discovery test. Discovery used to load an
// older-contract manifest with its reserved-flag shadows adapted away and a
// note attached; there is no adaptation left, so the same fixture must now
// become a SKIP record — visible, with the root cause on it — rather than a
// silently reduced extension.
func TestDiscoverExtensionsDetailed_SkipsOlderContractManifest(t *testing.T) {
	dir := t.TempDir()
	// No cliContract field (contract 0): unstamped, and it declares commands, so
	// the contract governs it and it cannot load.
	writeExtensionManifest(t, filepath.Join(dir, "ext"), `{
		"name": "@putnami/legacy",
		"commands": {
			"deploy": {
				"flags": {
					"json": {"type": "boolean"},
					"env": {"type": "string"}
				},
				"run": [{"id": "d", "task": "t"}]
			}
		},
		"tasks": {"t": {"kind": "command", "command": "echo"}}
	}`)

	result, err := DiscoverExtensionsDetailed(dir, &wsproto.Config{}, []string{"ext"})
	if err != nil {
		t.Fatalf("DiscoverExtensionsDetailed: %v", err)
	}
	if len(result.Extensions) != 0 {
		t.Fatalf("an older-contract extension must not load, got %d", len(result.Extensions))
	}
	if len(result.Skipped) != 1 {
		t.Fatalf("expected 1 skip record, got %v", result.Skipped)
	}
	skip := result.Skipped[0]
	if skip.Ref != "ext" {
		t.Errorf("skip Ref = %q, want ext", skip.Ref)
	}
	if skip.Reason == nil {
		t.Fatal("skip must carry the load error")
	}
	for _, want := range []string{
		fmt.Sprintf("requires %d", protocolcli.CurrentContract),
		"re-package the extension",
	} {
		if !strings.Contains(skip.Reason.Error(), want) {
			t.Errorf("skip reason missing %q: %v", want, skip.Reason)
		}
	}
}

// TestDiscoverExtensionsDetailed_LoadsCurrentContractManifest is the other half:
// the same manifest, stamped at the current contract, loads whole. Its
// reserved-flag shadow is what the ENFORCE arm rejects, so the flags here are
// extension-owned only — the loader no longer has a third answer between
// "loads" and "does not".
func TestDiscoverExtensionsDetailed_LoadsCurrentContractManifest(t *testing.T) {
	dir := t.TempDir()
	writeExtensionManifest(t, filepath.Join(dir, "ext"), fmt.Sprintf(`{
		"name": "@putnami/current",
		"cliContract": %d,
		"commands": {
			"deploy": {
				"flags": {"env": {"type": "string"}},
				"run": [{"id": "d", "task": "t"}]
			}
		},
		"tasks": {"t": {"kind": "command", "command": "echo"}}
	}`, protocolcli.CurrentContract))

	result, err := DiscoverExtensionsDetailed(dir, &wsproto.Config{}, []string{"ext"})
	if err != nil {
		t.Fatalf("DiscoverExtensionsDetailed: %v", err)
	}
	if len(result.Skipped) != 0 {
		t.Fatalf("a conforming extension must not be skipped, got %v", result.Skipped)
	}
	if len(result.Extensions) != 1 {
		t.Fatalf("expected 1 extension, got %d", len(result.Extensions))
	}
	job := result.Extensions[0].Jobs["deploy"]
	if job == nil {
		t.Fatal("missing deploy job")
	}
	if _, ok := job.Flags["env"]; !ok {
		t.Error("extension-owned flag 'env' should survive")
	}

	// Determinism: resolved descriptions feed cache keys, so two discoveries of
	// the same manifest must produce identical jobs.
	again, err := DiscoverExtensionsDetailed(dir, &wsproto.Config{}, []string{"ext"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Extensions[0].Jobs["deploy"].Flags, job.Flags) {
		t.Error("resolved flags differ between runs")
	}
}

// TestDiscoverExtensionsDetailed_SkipsReservedShadowAtCurrentContract pins the
// ENFORCE arm end to end: a manifest that CLAIMS this contract and then shadows
// a reserved global is a hard error, and discovery surfaces it as a skip rather
// than quietly dropping the flag as the adapt arm used to.
func TestDiscoverExtensionsDetailed_SkipsReservedShadowAtCurrentContract(t *testing.T) {
	dir := t.TempDir()
	writeExtensionManifest(t, filepath.Join(dir, "ext"), fmt.Sprintf(`{
		"name": "@putnami/shadow",
		"cliContract": %d,
		"commands": {
			"deploy": {
				"flags": {"json": {"type": "boolean"}},
				"run": [{"id": "d", "task": "t"}]
			}
		},
		"tasks": {"t": {"kind": "command", "command": "echo"}}
	}`, protocolcli.CurrentContract))

	result, err := DiscoverExtensionsDetailed(dir, &wsproto.Config{}, []string{"ext"})
	if err != nil {
		t.Fatalf("DiscoverExtensionsDetailed: %v", err)
	}
	if len(result.Extensions) != 0 {
		t.Fatalf("a reserved-flag shadow at the current contract must not load, got %d", len(result.Extensions))
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Reason == nil ||
		!strings.Contains(result.Skipped[0].Reason.Error(), "reserved global flag shadows") {
		t.Fatalf("skip should cite the reserved shadow, got %v", result.Skipped)
	}
}

func TestDiscoverExtensionsDetailed_SkipWarningDedupedPerProcess(t *testing.T) {
	dir := t.TempDir()
	writeExtensionManifest(t, filepath.Join(dir, "ext"), "{not json")

	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	// Discovery runs from ~7 call sites per invocation; the same skip must
	// warn at most once per process.
	for i := 0; i < 3; i++ {
		if _, err := DiscoverExtensionsDetailed(dir, &wsproto.Config{}, []string{"ext"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(buf.String(), "extension skipped"); got != 1 {
		t.Fatalf("skip warning emitted %d times, want exactly 1:\n%s", got, buf.String())
	}
}

// TestResolveReservedProvider_AbsentStaysSilent pins the ordinary answer for a
// capability nobody installed: absence is (nil, nil), never an error, so the
// caller decides whether to degrade silently or say so once.
func TestResolveReservedProvider_AbsentStaysSilent(t *testing.T) {
	provider, err := ResolveReservedProvider(nil, "cache-provider")
	if provider != nil || err != nil {
		t.Fatalf("absent provider should be (nil, nil), got (%v, %v)", provider, err)
	}
}

// TestSkippedProviderCause_CitesRootCause proves an UNLOADABLE extension is
// still distinguishable from an uninstalled one after slice C6b deleted the
// name-based gate. Core cannot know which skipped manifest would have declared
// the provider command — that manifest is the one that failed to parse — so it
// reports the skips it has, with their reasons, instead of degrading in silence
// on a fixable fault.
func TestSkippedProviderCause_CitesRootCause(t *testing.T) {
	cause := SkippedProviderCause([]SkippedExtension{
		{Ref: "@acme/provider", Name: "@acme/provider", Reason: errors.New("extension manifest x requires a newer putnami (contract 4 > 3)")},
	})
	for _, want := range []string{"@acme/provider", "requires a newer putnami", "putnami upgrade"} {
		if !strings.Contains(cause, want) {
			t.Errorf("cause %q missing %q", cause, want)
		}
	}
	if got := SkippedProviderCause(nil); got != "" {
		t.Errorf("no skips should produce no cause, got %q", got)
	}
}
