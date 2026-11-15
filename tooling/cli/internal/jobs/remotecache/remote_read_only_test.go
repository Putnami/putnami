package remotecache

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/store"
)

// uploadAndPublish runs the write path of one run against the fake provider
// with env: the upload of a published task entry, then the run marker. It
// returns the ops the provider received and the run's notices.
func uploadAndPublish(t *testing.T, env map[string]string) (remote *RemoteCache, ops []string, notices string) {
	t.Helper()
	useInProcessFakeProvider(t)
	wsRoot := t.TempDir()
	opsOut := filepath.Join(wsRoot, "ops.txt")
	merged := map[string]string{"FAKE_OPS_OUT": opsOut, "FAKE_MARKER_SHA": "cafef00d"}
	for k, v := range env {
		merged[k] = v
	}
	remote, buf := loadFakeProvider(t, wsRoot, merged)
	producer := publishFetchTestEntry(t, &store.EntryResult{Status: "success"})
	remote.UploadTaskEntry(context.Background(), fetchTestKey, fetchTestJob(), producer)
	remote.DrainUploads()
	remote.PublishRunMarker(context.Background(), "ws-id", "main", []string{"build"}, "", "cafef00d", "")
	remote.UploadTaskEntry(context.Background(), fetchTestKey, fetchTestJob(), producer)
	remote.DrainUploads()
	return remote, readProviderOps(t, opsOut), buf.String()
}

func TestRemoteCache_ListsReadOnly(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "a-read-only-session-is-declared", "the-cli-lists-read-only")
	useInProcessFakeProvider(t)
	wsRoot := t.TempDir()
	capabilitiesOut := filepath.Join(wsRoot, "capabilities.txt")
	remote, _ := loadFakeProvider(t, wsRoot, map[string]string{"FAKE_CAPABILITIES_OUT": capabilitiesOut})
	if err := remote.StartProvider(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(capabilitiesOut)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(strings.Split(string(got), ","), cache.CapabilityReadOnly) {
		t.Fatalf("initialize capabilities = %s, want %s listed", got, cache.CapabilityReadOnly)
	}
}

func TestRemoteCache_WritesNothingAfterTheReadOnlyEcho(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "a-read-only-session-is-declared", "the-cli-writes-nothing-after-the-read-only-echo")
	remote, ops, notices := uploadAndPublish(t, map[string]string{"FAKE_READ_ONLY_ECHO": "1"})
	for _, op := range ops {
		if op == string(cache.OpUpload) || op == string(cache.OpMarkerWrite) {
			t.Fatalf("a read-only session received %s; ops %v", op, ops)
		}
	}
	lines := strings.Split(strings.TrimSuffix(notices, "\n"), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "read-only for this credential") {
		t.Fatalf("notices = %q, want one line that the cache is read-only for this credential", notices)
	}
	if errs := remote.Stats().UploadErrors; errs != 0 {
		t.Fatalf("upload errors = %d, want 0", errs)
	}

	// Without the echo, the same run uploads and publishes.
	_, ops, notices = uploadAndPublish(t, nil)
	if !slices.Contains(ops, string(cache.OpUpload)) || !slices.Contains(ops, string(cache.OpMarkerWrite)) {
		t.Fatalf("a writable session did not upload and publish; ops %v", ops)
	}
	if notices != "" {
		t.Fatalf("a writable session printed %q", notices)
	}
}

func TestRemoteCache_ADeclinedUploadIsNotAnUploadError(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "a-read-only-session-is-declared", "a-declined-upload-is-not-an-upload-error")
	remote, ops, _ := uploadAndPublish(t, map[string]string{"FAKE_UPLOAD_DECLINE": "1"})
	if !slices.Contains(ops, string(cache.OpUpload)) {
		t.Fatalf("the provider received no upload; ops %v", ops)
	}
	if errs := remote.Stats().UploadErrors; errs != 0 {
		t.Fatalf("upload errors = %d, want 0 for uploads the provider declined", errs)
	}
	if err := uploadError(nil); err == nil || !strings.Contains(err.Error(), "no result") {
		t.Fatalf("uploadError(nil) = %v, want the missing-result error", err)
	}
}
