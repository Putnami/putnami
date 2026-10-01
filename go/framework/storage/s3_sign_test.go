package storage

import (
	"net/http"
	"strings"
	"testing"
)

func TestSignRequestAddsAuthorizationHeader(t *testing.T) {
	backend := NewS3Backend(S3Config{
		Endpoint:  "https://s3.us-east-1.amazonaws.com",
		Region:    "us-east-1",
		AccessKey: "AKIAIOSFODNN7EXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Bucket:    "test",
	})

	req, _ := http.NewRequest(http.MethodGet, "https://s3.us-east-1.amazonaws.com/test/key.txt", nil)
	backend.signRequest(req, sha256Hex(nil))

	auth := req.Header.Get("Authorization")
	if auth == "" {
		t.Fatal("expected Authorization header to be set")
	}
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("expected AWS4-HMAC-SHA256 prefix, got %q", auth)
	}
	if !strings.Contains(auth, "Credential=AKIAIOSFODNN7EXAMPLE/") {
		t.Errorf("expected access key in credential, got %q", auth)
	}
	if !strings.Contains(auth, "SignedHeaders=") {
		t.Errorf("expected SignedHeaders in auth, got %q", auth)
	}
	if !strings.Contains(auth, "Signature=") {
		t.Errorf("expected Signature in auth, got %q", auth)
	}

	amzDate := req.Header.Get("X-Amz-Date")
	if amzDate == "" {
		t.Fatal("expected X-Amz-Date header")
	}
	if len(amzDate) != 16 || amzDate[8] != 'T' || amzDate[15] != 'Z' {
		t.Errorf("expected amz date format 20060102T150405Z, got %q", amzDate)
	}

	contentHash := req.Header.Get("X-Amz-Content-Sha256")
	if contentHash == "" {
		t.Fatal("expected X-Amz-Content-Sha256 header")
	}
}

func TestSignRequestNoCredentialsSkips(t *testing.T) {
	backend := NewS3Backend(S3Config{
		Endpoint: "https://s3.example.com",
		Region:   "us-east-1",
		Bucket:   "test",
	})

	req, _ := http.NewRequest(http.MethodGet, "https://s3.example.com/test/key.txt", nil)
	backend.signRequest(req, sha256Hex(nil))

	if req.Header.Get("Authorization") != "" {
		t.Error("expected no Authorization header when credentials are empty")
	}
}

func TestSignRequestWithBody(t *testing.T) {
	backend := NewS3Backend(S3Config{
		Endpoint:  "https://s3.us-east-1.amazonaws.com",
		Region:    "us-east-1",
		AccessKey: "AKID",
		SecretKey: "SECRET",
		Bucket:    "test",
	})

	body := []byte("hello world")
	req, _ := http.NewRequest(http.MethodPut, "https://s3.us-east-1.amazonaws.com/test/file.txt", nil)
	backend.signRequest(req, sha256Hex(body))

	auth := req.Header.Get("Authorization")
	if auth == "" {
		t.Fatal("expected Authorization header")
	}

	// Content hash should reflect the body, not empty
	contentHash := req.Header.Get("X-Amz-Content-Sha256")
	emptyHash := sha256Hex(nil)
	if contentHash == emptyHash {
		t.Error("expected content hash to differ from empty body hash")
	}
	if contentHash != sha256Hex(body) {
		t.Errorf("expected content hash %s, got %s", sha256Hex(body), contentHash)
	}
}

// TestSignRequestUnsignedPayload verifies the single unsigned payload signing
// path: when the body is sent with UNSIGNED-PAYLOAD, the
// X-Amz-Content-Sha256 header must carry that literal (so the server recomputes
// the canonical request with the same hashed-payload value) and the request
// must still be fully signed.
func TestSignRequestUnsignedPayload(t *testing.T) {
	backend := NewS3Backend(S3Config{
		Endpoint:  "https://s3.us-east-1.amazonaws.com",
		Region:    "us-east-1",
		AccessKey: "AKID",
		SecretKey: "SECRET",
		Bucket:    "test",
	})

	req, _ := http.NewRequest(http.MethodPut, "https://s3.us-east-1.amazonaws.com/test/file.txt", nil)
	backend.signRequest(req, unsignedPayload)

	if got := req.Header.Get("X-Amz-Content-Sha256"); got != unsignedPayload {
		t.Errorf("X-Amz-Content-Sha256 = %q, want %q", got, unsignedPayload)
	}
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("expected signed request with AWS4-HMAC-SHA256 prefix, got %q", auth)
	}
	if !strings.Contains(auth, "Signature=") {
		t.Errorf("expected Signature in auth header, got %q", auth)
	}
}

func TestDeriveSigningKey(t *testing.T) {
	key := deriveSigningKey("secret", "20230101", "us-east-1", "s3")
	if len(key) != 32 {
		t.Errorf("expected 32-byte signing key, got %d", len(key))
	}
}

func TestCanonicalQueryString(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/bucket?prefix=a&delimiter=/&list-type=2", nil)
	qs := canonicalQueryString(req)
	// Parameters should be sorted alphabetically
	if !strings.HasPrefix(qs, "delimiter=") {
		t.Errorf("expected sorted query string starting with delimiter, got %q", qs)
	}
	if !strings.Contains(qs, "list-type=2") {
		t.Errorf("expected list-type in query string, got %q", qs)
	}
}

func TestURIEncode(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"hello", "hello"},
		{"hello world", "hello%20world"},
		{"a/b", "a%2Fb"},
		{"test~value", "test~value"},
	}
	for _, tc := range tests {
		got := uriEncode(tc.in)
		if got != tc.want {
			t.Errorf("uriEncode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
