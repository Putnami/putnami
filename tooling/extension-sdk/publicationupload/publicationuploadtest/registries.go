// Package publicationuploadtest holds fake npm, Go module and Put registries
// for the tests of a publication upload. Each one answers only requests that
// carry the one bearer it was started with, stores what it is sent, and stops
// when its test ends.
package publicationuploadtest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	put "go.putnami.dev/protocol/put"
	"go.putnami.dev/sdk/extension/npmpublish"
)

// NPMRegistry is an npm registry that stores what it is sent with its bearer
// and serves the stored tarballs back.
type NPMRegistry struct {
	// Server serves the registry.
	Server *httptest.Server

	bearer   string
	mu       sync.Mutex
	tarballs map[string]npmTarball
	requests int
}

type npmTarball struct {
	name, version string
	data          []byte
}

// NewNPMRegistry starts an npm registry that answers bearer alone.
func NewNPMRegistry(t testing.TB, bearer string) *NPMRegistry {
	t.Helper()
	reg := &NPMRegistry{bearer: bearer, tarballs: map[string]npmTarball{}}
	reg.Server = httptest.NewServer(http.HandlerFunc(reg.serve))
	t.Cleanup(reg.Server.Close)
	return reg
}

func (reg *NPMRegistry) serve(w http.ResponseWriter, r *http.Request) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.requests++
	if r.Header.Get("Authorization") != "Bearer "+reg.bearer {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var payload npmpublish.Payload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for version := range payload.Versions {
			for _, attachment := range payload.Attachments {
				data, err := base64.StdEncoding.DecodeString(attachment.Data)
				if err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				reg.tarballs[payload.Name+"@"+version] = npmTarball{name: payload.Name, version: version, data: data}
			}
		}
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		for _, tarball := range reg.tarballs {
			target, _ := npmpublish.TarballURL(reg.Server.URL, tarball.name, tarball.version)
			if strings.TrimPrefix(target, reg.Server.URL) == r.URL.EscapedPath() {
				_, _ = w.Write(tarball.data)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// Stored returns the tarball the registry holds for name@version, nil when it
// holds none, and the number of requests it received.
func (reg *NPMRegistry) Stored(name, version string) ([]byte, int) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.tarballs[name+"@"+version].data, reg.requests
}

// GoRegistry is a Go module registry that stores what it is sent with its
// bearer and serves the stored zips and go.mod files back.
type GoRegistry struct {
	// Server serves the registry.
	Server *httptest.Server

	bearer   string
	mu       sync.Mutex
	blobs    map[string][]byte
	zips     map[string][]byte
	mods     map[string][]byte
	requests int
}

// NewGoRegistry starts a Go module registry that answers bearer alone.
func NewGoRegistry(t testing.TB, bearer string) *GoRegistry {
	t.Helper()
	reg := &GoRegistry{bearer: bearer, blobs: map[string][]byte{}, zips: map[string][]byte{}, mods: map[string][]byte{}}
	reg.Server = httptest.NewServer(http.HandlerFunc(reg.serve))
	t.Cleanup(reg.Server.Close)
	return reg
}

func (reg *GoRegistry) serve(w http.ResponseWriter, r *http.Request) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.requests++
	if r.Header.Get("Authorization") != "Bearer "+reg.bearer {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	path := strings.TrimPrefix(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/-/blobs/upload"):
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256(body))
		reg.blobs[digest] = body
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"digest": digest})
	case r.Method == http.MethodPut && strings.Contains(path, "/@v/"):
		var request struct {
			GoMod     string `json:"go_mod"`
			ZipDigest string `json:"zip_digest"`
		}
		if err := json.Unmarshal(body, &request); err != nil || reg.blobs[request.ZipDigest] == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reg.zips[path] = reg.blobs[request.ZipDigest]
		reg.mods[path] = []byte(request.GoMod)
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet && strings.HasSuffix(path, ".zip"):
		reg.answer(w, reg.zips[strings.TrimSuffix(path, ".zip")])
	case r.Method == http.MethodGet && strings.HasSuffix(path, ".mod"):
		reg.answer(w, reg.mods[strings.TrimSuffix(path, ".mod")])
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (reg *GoRegistry) answer(w http.ResponseWriter, data []byte) {
	if data == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_, _ = w.Write(data)
}

// Stored returns the zip the registry holds for the lowercase module path at
// version, nil when it holds none, and the number of requests it received.
func (reg *GoRegistry) Stored(modulePath, version string) ([]byte, int) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.zips[modulePath+"/@v/"+version], reg.requests
}

// PutRegistry is a Put registry that serves put-write/v1 to its bearer: one
// blob per digest, immutable versions, and the manifest read. It records
// every request and every publish body as sent.
type PutRegistry struct {
	// Server serves the registry.
	Server *httptest.Server

	bearer    string
	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string]put.Manifest
	requests  []string
	publishes [][]byte
}

// NewPutRegistry starts a Put registry that answers bearer alone.
func NewPutRegistry(t testing.TB, bearer string) *PutRegistry {
	t.Helper()
	reg := &PutRegistry{bearer: bearer, blobs: map[string][]byte{}, manifests: map[string]put.Manifest{}}
	reg.Server = httptest.NewServer(http.HandlerFunc(reg.serve))
	t.Cleanup(reg.Server.Close)
	return reg
}

func (reg *PutRegistry) serve(w http.ResponseWriter, r *http.Request) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.requests = append(reg.requests, r.Method+" "+r.URL.EscapedPath())
	if r.Header.Get("Authorization") != "Bearer "+reg.bearer {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "blobs" && len(body) > 0:
		digest := put.Digest(body)
		reg.blobs[digest] = body
		putAnswer(w, http.StatusCreated, put.BlobReceipt{
			ID: "blob-" + digest[7:15], Digest: digest, Size: int64(len(body)),
			MediaType: r.Header.Get("Content-Type"), CreatedAt: "2026-10-02T10:00:00Z",
		})
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "publish":
		reg.publish(w, parts[0]+"/"+parts[1], body)
	case r.Method == http.MethodGet && len(parts) == 5 && parts[2] == "versions" && parts[4] == "manifest":
		manifest, ok := reg.manifests[parts[0]+"/"+parts[1]+"@"+parts[3]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		putAnswer(w, http.StatusOK, manifest)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// publish stores a version whose blobs are all uploaded. The strict request
// reader refuses a channel, a visibility and a source_ref.
func (reg *PutRegistry) publish(w http.ResponseWriter, coordinate string, body []byte) {
	reg.publishes = append(reg.publishes, body)
	request, diagnostics := put.ParsePublishRequest(body)
	if request == nil || len(diagnostics) > 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	references, err := put.BlobReferences(request.Payload)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	for _, reference := range references {
		if reg.blobs[reference] == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	key := coordinate + "@" + request.Version
	if _, exists := reg.manifests[key]; exists {
		w.WriteHeader(http.StatusConflict)
		return
	}
	manifest := put.Manifest{ID: "manifest-" + request.Version, PackageID: "package-1", MediaType: request.MediaType, Payload: request.Payload, CreatedAt: "2026-10-02T10:00:00Z"}
	reg.manifests[key] = manifest
	putAnswer(w, http.StatusCreated, put.PublishResponse{
		Package: coordinate,
		Version: put.Version{
			ID: "version-" + request.Version, PackageID: "package-1", Version: request.Version, ManifestID: manifest.ID,
			State: put.VersionStatePublished, Visibility: put.VisibilityPrivate, CreatedAt: "2026-10-02T10:00:00Z",
		},
		Manifest: manifest,
		Channel:  json.RawMessage("null"),
	})
}

func putAnswer(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// Recorded returns the requests and the publish bodies received so far.
func (reg *PutRegistry) Recorded() ([]string, [][]byte) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return slices.Clone(reg.requests), slices.Clone(reg.publishes)
}
