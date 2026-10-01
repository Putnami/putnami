package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestNewStorageHTTPClient_OwnsTransport(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "isolation", "http-backend-owns-its-transport")
	// Each backend must own its transport, not share the process-wide
	// DefaultTransport — otherwise Backend.Close()'s CloseIdleConnections would
	// drop idle keep-alive connections for every other consumer in the process.
	c1 := newStorageHTTPClient()
	c2 := newStorageHTTPClient()

	if c1.Transport == nil {
		t.Fatal("client has a nil Transport (would fall back to http.DefaultTransport)")
	}
	if c1.Transport == http.DefaultTransport {
		t.Error("backend must not share the process-wide DefaultTransport")
	}
	if c1.Transport == c2.Transport {
		t.Error("each backend should own a distinct transport")
	}
}

// fakeS3Objects is an S3 endpoint double that stores a PUT body only when the
// whole body arrived, as S3 commits nothing from an aborted upload, and serves
// stored objects on GET.
func fakeS3Objects(t *testing.T) *httptest.Server {
	t.Helper()
	var (
		mu      sync.Mutex
		objects = map[string][]byte{}
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "incomplete body", http.StatusBadRequest)
				return
			}
			mu.Lock()
			objects[r.URL.Path] = body
			mu.Unlock()
		case http.MethodGet:
			mu.Lock()
			body, ok := objects[r.URL.Path]
			mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(body)
		default:
			http.Error(w, "unsupported", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestBackendFailedPutKeepsObject(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "backend-contract", "memory-s3-gcs-failed-put-keeps-object")
	for name, newBackend := range map[string]func(t *testing.T) (Backend, string){
		"memory": func(*testing.T) (Backend, string) { return NewMemoryBackend(), "bucket" },
		"s3 streaming": func(t *testing.T) (Backend, string) {
			return NewS3Backend(S3Config{Endpoint: fakeS3Objects(t).URL}), "bucket"
		},
		"s3 spooled": func(t *testing.T) (Backend, string) {
			return NewS3Backend(S3Config{Endpoint: fakeS3Objects(t).URL, AccessKey: "AKID", SecretKey: "SECRET"}), "bucket"
		},
		"gcs": func(t *testing.T) (Backend, string) { return newFakeGCS(t).backend(t), "bucket" },
	} {
		t.Run(name, func(t *testing.T) {
			backend, bucket := newBackend(t)
			ctx := context.Background()
			if _, err := backend.Put(ctx, bucket, "doc", strings.NewReader("original"), nil); err != nil {
				t.Fatalf("seed Put: %v", err)
			}
			if _, err := backend.Put(ctx, bucket, "doc", failingReader("partial replacement"), nil); err == nil {
				t.Fatal("a Put whose reader fails should return an error")
			}
			if body, _ := readObject(t, backend, bucket, "doc"); body != "original" {
				t.Errorf("object after failed Put = %q, want %q", body, "original")
			}
		})
	}
}
