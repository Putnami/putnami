package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/tooling/cli/internal/jobs/remotecache"
	"go.putnami.dev/tooling/cli/internal/store"
)

func TestVerifyCachedReportRequiresCurrentNativeProducerAndManifest(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryPlanFixture(t, false)
	entry := fixture.publish(t, recoveryReportTask, []store.DeclaredEntryOutput{recoveryReportOutput}, recoveryReportBytes)
	answer := fixture.source.RecoverReports(fixture.ws.Projects)
	if len(answer.Reports) != 1 {
		t.Fatalf("native recovery = %+v, want one report", answer)
	}
	native := answer.Reports[0]
	sum := sha256.Sum256([]byte(recoveryReportBytes))
	reference := features.VerificationReportRef{Task: native.Task, Project: native.Project, Path: native.Field,
		Digest: "sha256:" + hex.EncodeToString(sum[:]), Restored: true}
	if data, err := fixture.source.verifyCachedReport(reference); err != nil || string(data) != recoveryReportBytes {
		t.Fatalf("current native cached report = %q, %v", data, err)
	}
	for name, altered := range map[string]features.VerificationReportRef{
		"forged task":     {Task: "/billing:forged", Project: reference.Project, Path: reference.Path, Digest: reference.Digest, Restored: true},
		"foreign project": {Task: reference.Task, Project: "/foreign", Path: reference.Path, Digest: reference.Digest, Restored: true},
		"forged path":     {Task: reference.Task, Project: reference.Project, Path: "other.json", Digest: reference.Digest, Restored: true},
		"forged digest":   {Task: reference.Task, Project: reference.Project, Path: reference.Path, Digest: "sha256:" + strings.Repeat("0", 64), Restored: true},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := fixture.source.verifyCachedReport(altered); err == nil {
				t.Fatal("forged restored reference was accepted")
			}
		})
	}
	if err := os.WriteFile(native.Path, []byte(`{"protocolVersion":1,"observations":[ ]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.source.verifyCachedReport(reference); err == nil {
		t.Fatal("cache bytes changed behind descriptor were accepted")
	}
	if err := os.WriteFile(native.Path, []byte(recoveryReportBytes), 0o644); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(filepath.Dir(entry.FilesDir), "result.json")
	resultBytes, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, []byte(`{"status":"failed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.source.verifyCachedReport(reference); err == nil {
		t.Fatal("failed task result was accepted as a restored producer")
	}
	if err := os.WriteFile(resultPath, resultBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(filepath.Dir(entry.FilesDir), "manifest.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(`{"files":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.source.verifyCachedReport(reference); err == nil {
		t.Fatal("entry without the declared report manifest was accepted")
	}
	if err := os.WriteFile(manifestPath, manifestBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if data, err := fixture.source.verifyCachedReport(reference); err != nil || string(data) != recoveryReportBytes {
		t.Fatalf("restored native entry was not usable after fixture repair: %q, %v", data, err)
	}
	if err := os.WriteFile(filepath.Join(fixture.ws.Root, "billing", "invoice_test.go"), []byte("package billing\n// changed test input\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	freshCache := store.NewCacheManager(store.NewLocalStore(filepath.Join(fixture.ws.Root, ".putnami", "store")))
	fresh := newCachedObservationRecovery(fixture.source.req, fixture.ws, fixture.source.extensions, freshCache)
	if _, err := fresh.verifyCachedReport(reference); err == nil {
		t.Fatal("stale task cache key was accepted after source input changed")
	}
	if entry.Result.Status != "success" {
		t.Fatalf("fixture was not a successful native producer: %+v", entry.Result)
	}
}

// TestVerifyCachedReportReadsTheLocalStoreOnly pins that the read-only bridge
// keeps no remote cache. A source built as VerifyCachedVerificationReport
// builds it carries none even when its request names the run's remote cache,
// so a report whose entry only a remote cache holds is refused, and nothing is
// fetched into the local store.
func TestVerifyCachedReportReadsTheLocalStoreOnly(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryPlanFixture(t, false)
	remote := newRemoteEntryFake()
	fixture.stageRemote(t, remote, recoveryReportTask, []store.DeclaredEntryOutput{recoveryReportOutput}, recoveryReportBytes)
	req := &Request{Commands: []string{"test"}, executionRemote: &remotecache.RemoteCache{}}
	bridge := newCachedObservationRecovery(req, fixture.ws, fixture.source.extensions, fixture.cache)
	if bridge.remote != nil {
		t.Fatal("the verification bridge's source reads a remote cache")
	}
	sum := sha256.Sum256([]byte(recoveryReportBytes))
	reference := features.VerificationReportRef{Task: recoveryReportTask, Project: recoveryProjectID,
		Path: ".putnami/store/report.json", Digest: "sha256:" + hex.EncodeToString(sum[:]), Restored: true}
	if _, err := bridge.verifyCachedReport(reference); err == nil {
		t.Fatal("a report without a local producer entry was accepted")
	}
	if entry, err := fixture.cache.LookupTaskEntry(fixture.keys[recoveryReportTask]); err != nil || entry != nil {
		t.Fatalf("the bridge published an entry into the local store (entry %v, err %v)", entry, err)
	}
	if asked := remote.askedKeys(); len(asked) != 0 {
		t.Fatalf("the bridge asked a remote cache for %v", asked)
	}
}
