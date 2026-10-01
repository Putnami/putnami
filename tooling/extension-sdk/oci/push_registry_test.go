package oci

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"

	"go.putnami.dev/sdk/extension/jsonl"
)

// seedLayout writes a random image to <dockerDir>/oci and returns its digest, as
// the in-process package step produces. dockerDir is created under wsRoot.
func seedLayout(t *testing.T, dockerDir string) string {
	t.Helper()
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := WriteLayout(filepath.Join(dockerDir, "oci"), img)
	if err != nil {
		t.Fatalf("WriteLayout: %v", err)
	}
	return digest
}

func TestPushContent_FromLayout(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	dockerDir := t.TempDir()
	digest := seedLayout(t, dockerDir)
	contentRef := host + "/team/app:c-hash"
	kc := authn.NewMultiKeychain()

	if err := PushContent(jsonl.New(), t.TempDir(), dockerDir, "oci", "", contentRef, false, kc, ""); err != nil {
		t.Fatalf("PushContent: %v", err)
	}
	got, err := crane.Digest(contentRef)
	if err != nil {
		t.Fatalf("crane.Digest(%s): %v", contentRef, err)
	}
	if got != digest {
		t.Errorf("pushed digest = %s, want %s", got, digest)
	}
}

// recordingServer serves the in-process registry and records each request as
// "METHOD path", so a test can count what a push asks the registry.
func recordingServer(t *testing.T) (host string, calls func() []string) {
	t.Helper()
	inner := registry.New()
	var mu sync.Mutex
	var recorded []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		recorded = append(recorded, r.Method+" "+r.URL.Path)
		mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), recorded...)
	}
}

// countManifestRequests counts the recorded manifest requests with method.
func countManifestRequests(calls []string, method string) int {
	count := 0
	for _, call := range calls {
		if strings.HasPrefix(call, method+" ") && strings.Contains(call, "/manifests/") {
			count++
		}
	}
	return count
}

// A layout push is remote.Write without its manifest HEAD: the caller's cache
// lookup has already asked, so the push sends the blobs and one manifest PUT,
// and reports the digest of the bytes that PUT carried.
func TestPushContentWithDigest_SendsNoManifestHead(t *testing.T) {
	host, calls := recordingServer(t)
	dockerDir := t.TempDir()
	digest := seedLayout(t, dockerDir)
	kc := authn.NewMultiKeychain()

	for _, contentRef := range []string{host + "/team/app:c-hash", host + "/team/app@" + digest} {
		before := len(calls())
		got, err := PushContentWithDigest(jsonl.New(), t.TempDir(), dockerDir, "oci", "", contentRef, false, kc, "")
		if err != nil {
			t.Fatalf("PushContentWithDigest(%s): %v", contentRef, err)
		}
		if got != digest {
			t.Fatalf("PushContentWithDigest(%s) digest = %s, want the layout digest %s", contentRef, got, digest)
		}
		pushed := calls()[before:]
		if heads := countManifestRequests(pushed, http.MethodHead); heads != 0 {
			t.Fatalf("push to %s sent %d manifest HEADs, want none; calls = %v", contentRef, heads, pushed)
		}
		if puts := countManifestRequests(pushed, http.MethodPut); puts != 1 {
			t.Fatalf("push to %s sent %d manifest PUTs, want one; calls = %v", contentRef, puts, pushed)
		}
		resolved, err := crane.Digest(contentRef)
		if err != nil || resolved != digest {
			t.Fatalf("crane.Digest(%s) = (%s, %v), want the pushed digest %s", contentRef, resolved, err, digest)
		}
	}
}

// A digest reference names the only bytes it may hold, so a layout whose
// manifest has another digest is refused before the registry sees a request.
func TestPushContentWithDigest_RefusesMismatchedDigestBeforeAnyRequest(t *testing.T) {
	host, calls := recordingServer(t)
	dockerDir := t.TempDir()
	seedLayout(t, dockerDir)
	contentRef := host + "/team/app@sha256:" + strings.Repeat("a", 64)

	got, err := PushContentWithDigest(jsonl.New(), t.TempDir(), dockerDir, "oci", "", contentRef, false, authn.NewMultiKeychain(), "")
	if err == nil || !strings.Contains(err.Error(), "does not match target digest") || got != "" {
		t.Fatalf("PushContentWithDigest(mismatched digest) = (%q, %v), want a digest refusal", got, err)
	}
	if recorded := calls(); len(recorded) != 0 {
		t.Fatalf("refused push still sent requests: %v", recorded)
	}
}

func TestPushContent_SkipsWhenExists(t *testing.T) {
	// contentExists=true short-circuits before any registry contact.
	if err := PushContent(jsonl.New(), t.TempDir(), t.TempDir(), "oci", "", "example.com/app:c-x", true, authn.NewMultiKeychain(), ""); err != nil {
		t.Fatalf("PushContent (exists) should be a no-op, got: %v", err)
	}
}

func TestPushContent_BadLayoutFails(t *testing.T) {
	// dockerDir has no "oci" layout → LoadFromLayout fails.
	err := PushContent(jsonl.New(), t.TempDir(), t.TempDir(), "oci", "", "example.com/app:c-x", false, authn.NewMultiKeychain(), "")
	if err == nil {
		t.Fatal("expected error for missing OCI layout, got nil")
	}
}

func TestResolveDigest(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	dockerDir := t.TempDir()
	digest := seedLayout(t, dockerDir)
	contentRef := host + "/team/app:c-hash"
	kc := authn.NewMultiKeychain()
	if err := PushContent(jsonl.New(), t.TempDir(), dockerDir, "oci", "", contentRef, false, kc, ""); err != nil {
		t.Fatalf("PushContent: %v", err)
	}

	// Empty registry → "" (no lookup).
	if got := ResolveDigest(jsonl.New(), "", contentRef, kc); got != "" {
		t.Errorf("ResolveDigest(empty registry) = %q, want \"\"", got)
	}
	// Resolvable ref → the pushed digest.
	if got := ResolveDigest(jsonl.New(), host, contentRef, kc); got != digest {
		t.Errorf("ResolveDigest = %q, want %q", got, digest)
	}
	// Unknown ref → "" (best-effort, never fails).
	if got := ResolveDigest(jsonl.New(), host, host+"/team/app:nope", kc); got != "" {
		t.Errorf("ResolveDigest(missing) = %q, want \"\"", got)
	}
}

func TestApplySessionRefs_StandardFallback(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	dockerDir := t.TempDir()
	digest := seedLayout(t, dockerDir)
	contentRef := host + "/team/app:c-hash"
	kc := authn.NewMultiKeychain()
	if err := PushContent(jsonl.New(), t.TempDir(), dockerDir, "oci", "", contentRef, false, kc, ""); err != nil {
		t.Fatalf("PushContent: %v", err)
	}

	// contentDigest="" forces the standard crane.Tag fallback (no fast path).
	if err := ApplySessionRefs(jsonl.New(), host, "team/app", contentRef, "", []string{"v1.0.0", "latest"}, kc, ""); err != nil {
		t.Fatalf("ApplySessionRefs: %v", err)
	}
	for _, tag := range []string{"v1.0.0", "latest"} {
		ref := host + "/team/app:" + tag
		got, err := crane.Digest(ref)
		if err != nil {
			t.Errorf("expected %s in registry: %v", ref, err)
			continue
		}
		if got != digest {
			t.Errorf("%s digest = %s, want %s", ref, got, digest)
		}
	}
}

func TestApplySessionRefs_NoTagsIsNoOp(t *testing.T) {
	// The only session tag equals srcRef, so nothing is applied — early return.
	srcRef := "example.com/team/app:only"
	if err := ApplySessionRefs(jsonl.New(), "example.com", "team/app", srcRef, "", []string{"only"}, authn.NewMultiKeychain(), ""); err != nil {
		t.Fatalf("ApplySessionRefs (no tags) should be a no-op, got: %v", err)
	}
}

// putnamiRegistry wraps the in-memory distribution registry with the protocol/oci
// tag-digest endpoints, acting as a putnami OCI registry. It applies posted tags
// against the inner registry so crane assertions can see them.
type putnamiRegistry struct {
	inner    http.Handler
	host     func() string
	tagCalls atomic.Int32
}

func (p *putnamiRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v2/_putnami/capabilities" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"apis":["tag-digest/v1"]}`)
		return
	case r.URL.Path == "/v2/_putnami/tag-digest" && r.Method == http.MethodPost:
		var req struct {
			Repository string   `json:"repository"`
			Digest     string   `json:"digest"`
			Tags       []string `json:"tags"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		p.tagCalls.Add(1)
		src := p.host() + "/" + req.Repository + "@" + req.Digest
		for _, tag := range req.Tags {
			if err := crane.Tag(src, tag); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	p.inner.ServeHTTP(w, r)
}

func TestApplySessionRefs_TagDigestFastPath(t *testing.T) {
	preg := &putnamiRegistry{inner: registry.New()}
	srv := httptest.NewServer(preg)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	preg.host = func() string { return host }

	dockerDir := t.TempDir()
	digest := seedLayout(t, dockerDir)
	contentRef := host + "/team/app:c-hash"
	kc := authn.NewMultiKeychain()
	if err := PushContent(jsonl.New(), t.TempDir(), dockerDir, "oci", "", contentRef, false, kc, ""); err != nil {
		t.Fatalf("PushContent: %v", err)
	}

	// srcRef addresses the content by digest; contentDigest set → fast path taken.
	srcRef := host + "/team/app@" + digest
	if err := ApplySessionRefs(jsonl.New(), host, "team/app", srcRef, digest, []string{"v3.0.0", "latest"}, kc, ""); err != nil {
		t.Fatalf("ApplySessionRefs (fast path): %v", err)
	}
	if got := preg.tagCalls.Load(); got != 1 {
		t.Errorf("tag-digest calls = %d, want exactly 1 batched call", got)
	}
	for _, tag := range []string{"v3.0.0", "latest"} {
		ref := host + "/team/app:" + tag
		got, err := crane.Digest(ref)
		if err != nil {
			t.Errorf("expected %s in registry: %v", ref, err)
			continue
		}
		if got != digest {
			t.Errorf("%s digest = %s, want %s", ref, got, digest)
		}
	}
}
