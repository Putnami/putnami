package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

func TestS3Backend_objectURL_EncodesKeys(t *testing.T) {
	b := NewS3Backend(S3Config{Endpoint: "https://s3.example.com"})
	cases := map[string]string{
		"plain.txt":   "https://s3.example.com/bucket/plain.txt",
		"path/to/x":   "https://s3.example.com/bucket/path/to/x",
		"my file.png": "https://s3.example.com/bucket/my%20file.png",
		"a/b c.txt":   "https://s3.example.com/bucket/a/b%20c.txt",
		"evil?acl":    "https://s3.example.com/bucket/evil%3Facl",
	}
	for key, want := range cases {
		if got := b.objectURL("bucket", key); got != want {
			t.Errorf("objectURL(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestS3Backend_Put_KeyNotInjectedIntoQuery(t *testing.T) {
	var gotPath, gotRawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotRawQuery = r.URL.Path, r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	b := NewS3Backend(S3Config{Endpoint: srv.URL})
	if _, err := b.Put(context.Background(), "bucket", "evil?acl", strings.NewReader("x"), nil); err != nil {
		t.Fatalf("put: %v", err)
	}
	if gotRawQuery != "" {
		t.Errorf("object key leaked into the query string: %q", gotRawQuery)
	}
	if gotPath != "/bucket/evil?acl" {
		t.Errorf("decoded path = %q, want /bucket/evil?acl", gotPath)
	}
}

func TestS3Backend_Put_SpaceKeyRoundTrips(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	b := NewS3Backend(S3Config{Endpoint: srv.URL, AccessKey: "AKIA", SecretKey: "secret"})
	if _, err := b.Put(context.Background(), "bucket", "my file.png", strings.NewReader("x"), nil); err != nil {
		t.Fatalf("put: %v", err)
	}
	if gotPath != "/bucket/my file.png" {
		t.Errorf("server decoded path = %q, want /bucket/my file.png", gotPath)
	}
}

// The string signed as the canonical URI must equal the path transmitted on the
// wire, or AWS rejects the request with SignatureDoesNotMatch.
func TestCanonicalURI_MatchesEscapedPath(t *testing.T) {
	b := NewS3Backend(S3Config{Endpoint: "https://s3.example.com", AccessKey: "AKIA", SecretKey: "s"})
	req, err := http.NewRequest(http.MethodPut, b.objectURL("bucket", "my file.png"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := canonicalURI(req), req.URL.EscapedPath(); got != want {
		t.Errorf("canonicalURI %q != EscapedPath %q", got, want)
	}
	if got := canonicalURI(req); got != "/bucket/my%20file.png" {
		t.Errorf("canonicalURI = %q, want /bucket/my%%20file.png", got)
	}
}

func TestS3Backend_SignedGetURL(t *testing.T) {
	b := NewS3Backend(S3Config{
		Endpoint:  "https://s3.us-east-1.amazonaws.com",
		Region:    "us-east-1",
		AccessKey: "AKIA",
		SecretKey: "secret",
	})
	raw, err := b.SignedGetURL(context.Background(), "bucket", "k/file.txt", SignedURLOptions{Expiry: 10 * time.Minute})
	if err != nil {
		t.Fatalf("signed url: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	q := u.Query()
	for _, p := range []string{"X-Amz-Algorithm", "X-Amz-Credential", "X-Amz-Date", "X-Amz-Expires", "X-Amz-SignedHeaders", "X-Amz-Signature"} {
		if q.Get(p) == "" {
			t.Errorf("missing %s in %q", p, raw)
		}
	}
	if q.Get("X-Amz-Expires") != "600" {
		t.Errorf("X-Amz-Expires = %q, want 600", q.Get("X-Amz-Expires"))
	}
	if q.Get("X-Amz-SignedHeaders") != "host" {
		t.Errorf("X-Amz-SignedHeaders = %q, want host", q.Get("X-Amz-SignedHeaders"))
	}
}

func TestS3Backend_SignedPutURL_BindsContentType(t *testing.T) {
	b := NewS3Backend(S3Config{Endpoint: "https://s3.example.com", Region: "us-east-1", AccessKey: "AKIA", SecretKey: "secret"})
	raw, err := b.SignedPutURL(context.Background(), "bucket", "k.png", SignedURLOptions{ContentType: "image/png"})
	if err != nil {
		t.Fatalf("signed url: %v", err)
	}
	u, _ := url.Parse(raw)
	if sh := u.Query().Get("X-Amz-SignedHeaders"); sh != "content-type;host" {
		t.Errorf("X-Amz-SignedHeaders = %q, want content-type;host", sh)
	}
}

func TestS3Backend_SignedURL_RequiresCredentials(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "signed-access", "s3-signed-url-requires-credentials")
	b := NewS3Backend(S3Config{Endpoint: "https://s3.example.com"})
	if _, err := b.SignedGetURL(context.Background(), "bucket", "k", SignedURLOptions{}); err == nil {
		t.Fatal("expected an error when credentials are not configured")
	}
}
