package oci

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"

	ociproto "go.putnami.dev/protocol/oci"
)

// tagDigestServer is a minimal stand-in for a Putnami OCI registry: it answers
// the /v2/ auth ping (so the transport resolves anonymous credentials) plus the
// two protocol/oci endpoints. capsBody / capsStatus and tagStatus are knobs the
// individual tests set to drive each branch of tagdigest.go.
type tagDigestServer struct {
	capsStatus int
	capsBody   string
	tagStatus  int
	lastReq    *ociproto.TagDigestRequest
}

func (s *tagDigestServer) handler() http.Handler {
	mux := http.NewServeMux()
	// Auth ping: 200 → the transport negotiates anonymous access.
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc(ociproto.CapabilitiesPath, func(w http.ResponseWriter, r *http.Request) {
		if s.capsStatus != http.StatusOK {
			http.Error(w, "no caps", s.capsStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, s.capsBody) //nolint:errcheck // test fixture
	})
	mux.HandleFunc(ociproto.TagDigestPath, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req ociproto.TagDigestRequest
		_ = json.Unmarshal(body, &req)
		s.lastReq = &req
		if s.tagStatus != http.StatusOK {
			http.Error(w, "tag boom", s.tagStatus)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func newTestTagClient(t *testing.T, h http.Handler) (*ociTagClient, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	host := strings.TrimPrefix(srv.URL, "http://")
	client, err := newOCITagClient(context.Background(), host+"/team/app:latest", authn.NewMultiKeychain(), nil)
	if err != nil {
		srv.Close()
		t.Fatalf("newOCITagClient: %v", err)
	}
	return client, srv.Close
}

func TestNewOCITagClient_BadReference(t *testing.T) {
	if _, err := newOCITagClient(context.Background(), "::::not a ref", authn.NewMultiKeychain(), nil); err == nil {
		t.Fatal("expected parse error for malformed reference, got nil")
	}
}

func TestSupportsTagDigest_Advertised(t *testing.T) {
	srv := &tagDigestServer{capsStatus: http.StatusOK, capsBody: `{"apis":["tag-digest/v1"]}`}
	client, closeFn := newTestTagClient(t, srv.handler())
	defer closeFn()

	if !client.supportsTagDigest(context.Background()) {
		t.Fatal("supportsTagDigest = false, want true when tag-digest/v1 is advertised")
	}
}

func TestSupportsTagDigest_NotAdvertised(t *testing.T) {
	srv := &tagDigestServer{capsStatus: http.StatusOK, capsBody: `{"apis":["something-else/v9"]}`}
	client, closeFn := newTestTagClient(t, srv.handler())
	defer closeFn()

	if client.supportsTagDigest(context.Background()) {
		t.Fatal("supportsTagDigest = true, want false when tag-digest/v1 is absent")
	}
}

func TestSupportsTagDigest_NotFound(t *testing.T) {
	srv := &tagDigestServer{capsStatus: http.StatusNotFound}
	client, closeFn := newTestTagClient(t, srv.handler())
	defer closeFn()

	if client.supportsTagDigest(context.Background()) {
		t.Fatal("supportsTagDigest = true, want false on a 404 (standard registry)")
	}
}

func TestSupportsTagDigest_MalformedBody(t *testing.T) {
	srv := &tagDigestServer{capsStatus: http.StatusOK, capsBody: `not json at all`}
	client, closeFn := newTestTagClient(t, srv.handler())
	defer closeFn()

	if client.supportsTagDigest(context.Background()) {
		t.Fatal("supportsTagDigest = true, want false on a malformed capabilities body")
	}
}

func TestTagDigest_Success(t *testing.T) {
	srv := &tagDigestServer{tagStatus: http.StatusOK}
	client, closeFn := newTestTagClient(t, srv.handler())
	defer closeFn()

	const digest = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tags := []string{"v1.0.0", "latest"}
	if err := client.tagDigest(context.Background(), digest, tags); err != nil {
		t.Fatalf("tagDigest: %v", err)
	}

	// The wire contract: repository without host, the digest, and every tag.
	if srv.lastReq == nil {
		t.Fatal("server never received a tag-digest request")
	}
	if srv.lastReq.Repository != "team/app" {
		t.Errorf("repository = %q, want team/app", srv.lastReq.Repository)
	}
	if srv.lastReq.Digest != digest {
		t.Errorf("digest = %q, want %q", srv.lastReq.Digest, digest)
	}
	if strings.Join(srv.lastReq.Tags, ",") != "v1.0.0,latest" {
		t.Errorf("tags = %v, want [v1.0.0 latest]", srv.lastReq.Tags)
	}
}

func TestTagDigest_ServerError(t *testing.T) {
	srv := &tagDigestServer{tagStatus: http.StatusInternalServerError}
	client, closeFn := newTestTagClient(t, srv.handler())
	defer closeFn()

	err := client.tagDigest(context.Background(), "sha256:dead", []string{"latest"})
	if err == nil {
		t.Fatal("expected error when tag-digest returns 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %q, want it to mention the 500 status", err.Error())
	}
}
