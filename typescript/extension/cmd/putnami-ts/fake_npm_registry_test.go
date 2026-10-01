package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha1" //nolint:gosec // npm's legacy shasum field, not a security use
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeNPMPackage is one version of one package the fake registry serves.
type fakeNPMPackage struct {
	name    string
	version string
	// files are the tarball entries, relative to the package directory.
	files map[string]string
	// scripts are the lifecycle scripts the registry metadata declares.
	scripts map[string]string
}

// fakeNPMRegistry is an npm registry that answers only a request carrying
// `Authorization: Bearer <token>`, or every request when token is empty, and
// counts what it was asked.
type fakeNPMRegistry struct {
	server       *httptest.Server
	token        string
	packuments   map[string][]byte
	tarballs     map[string][]byte
	served       atomic.Int64
	unauthorized atomic.Int64
}

// startFakeNPMRegistry serves packages behind a bearer. Stop it with Close;
// the test cleanup closes it too.
func startFakeNPMRegistry(t *testing.T, token string, packages ...fakeNPMPackage) *fakeNPMRegistry {
	t.Helper()
	registry := &fakeNPMRegistry{
		token:      token,
		packuments: map[string][]byte{},
		tarballs:   map[string][]byte{},
	}
	registry.server = httptest.NewServer(http.HandlerFunc(registry.serve))
	t.Cleanup(registry.Close)
	for _, pkg := range packages {
		registry.add(t, pkg)
	}
	return registry
}

// URL is the registry root, with the trailing slash an .npmrc scope line
// carries.
func (r *fakeNPMRegistry) URL() string { return r.server.URL + "/" }

// Host is the registry's host:port, the form a credential's hosts carry.
func (r *fakeNPMRegistry) Host() string {
	parsed, _ := url.Parse(r.server.URL)
	return parsed.Host
}

// Close stops the registry: every later request fails to connect.
func (r *fakeNPMRegistry) Close() {
	r.server.CloseClientConnections()
	r.server.Close()
}

func (r *fakeNPMRegistry) add(t *testing.T, pkg fakeNPMPackage) {
	t.Helper()
	manifest := map[string]any{"name": pkg.name, "version": pkg.version}
	if len(pkg.scripts) > 0 {
		manifest["scripts"] = pkg.scripts
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"package.json": string(manifestJSON)}
	for name, content := range pkg.files {
		files[name] = content
	}
	tarball := fakeTarball(t, files)
	base := pkg.name[strings.LastIndex(pkg.name, "/")+1:]
	tarballPath := "/" + pkg.name + "/-/" + base + "-" + pkg.version + ".tgz"
	r.tarballs[tarballPath] = tarball

	sha512Sum := sha512.Sum512(tarball)
	sha1Sum := sha1.Sum(tarball) //nolint:gosec // npm's legacy shasum field
	version := map[string]any{
		"name":    pkg.name,
		"version": pkg.version,
		"dist": map[string]any{
			"tarball":   r.server.URL + tarballPath,
			"integrity": "sha512-" + base64.StdEncoding.EncodeToString(sha512Sum[:]),
			"shasum":    hex.EncodeToString(sha1Sum[:]),
		},
	}
	if len(pkg.scripts) > 0 {
		version["scripts"] = pkg.scripts
	}
	packument, err := json.Marshal(map[string]any{
		"name":      pkg.name,
		"dist-tags": map[string]string{"latest": pkg.version},
		"versions":  map[string]any{pkg.version: version},
		"time":      map[string]string{pkg.version: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.packuments[pkg.name] = packument
}

func (r *fakeNPMRegistry) serve(w http.ResponseWriter, req *http.Request) {
	if r.token != "" && req.Header.Get("Authorization") != "Bearer "+r.token {
		r.unauthorized.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	path, err := url.PathUnescape(req.URL.EscapedPath())
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if tarball, ok := r.tarballs[path]; ok {
		r.served.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(tarball)
		return
	}
	if packument, ok := r.packuments[strings.TrimPrefix(path, "/")]; ok {
		r.served.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(packument)
		return
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"not found"}`))
}

// fakeTarball builds an npm tarball: every file under `package/`.
func fakeTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		header := &tar.Header{
			Name:    "package/" + name,
			Mode:    0o644,
			Size:    int64(len(content)),
			ModTime: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		}
		if strings.HasSuffix(name, ".sh") {
			header.Mode = 0o755
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
