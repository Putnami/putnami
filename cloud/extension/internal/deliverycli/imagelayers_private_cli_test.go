package deliverycli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func useImageLayersRegistryToken(t *testing.T, token string) {
	t.Helper()
	previous := imageLayersRegistryToken
	imageLayersRegistryToken = func(string) (string, string) { return token, "" }
	t.Cleanup(func() { imageLayersRegistryToken = previous })
}

func TestImageLayersPrivateCLIRequiresThePinnedBytes(t *testing.T) {
	const token = "pkt_private_layer"
	payload := []byte("private-pinned-linux-cli")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "private CLI", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("channel") != "1.2.3" || r.URL.Query().Get("arch") != "x64" {
			t.Error("image producer did not request the pinned platform and version")
		}
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	previousSource, previousToken := imageLayersCLIDistFor, imageLayersRegistryToken
	t.Cleanup(func() { imageLayersCLIDistFor, imageLayersRegistryToken = previousSource, previousToken })
	imageLayersCLIDistFor = func(*http.Client) imageLayersCLIDist {
		return putServerDist{client: server.Client(), base: server.URL}
	}
	imageLayersRegistryToken = func(host string) (string, string) {
		origin, _ := url.Parse(server.URL)
		if host != origin.Host {
			t.Error("credential resolution changed the native registry host")
		}
		return token, ""
	}
	workspace := imageLayersToolWorkspace(t)
	imageLayersLock(t, workspace, "1.2.3", map[string]string{"linux/amd64": sha256Hex(payload)})
	project := imageLayersProject(t, `{"layers":[`+imageLayersCLILayer+`]}`)
	if err := ImageLayers(map[string]any{"project": project}, nil, workspace, nil, imageLayersIO(new([]string))); err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(filepath.Join(project, ".gen", "layers", "bin", "putnami"))
	if err != nil || string(installed) != string(payload) {
		t.Fatalf("private CLI bytes were not installed exactly: %v", err)
	}
	badProject := imageLayersProject(t, `{"layers":[`+imageLayersCLILayer+`]}`)
	imageLayersLock(t, workspace, "1.2.3", map[string]string{"linux/amd64": strings.Repeat("0", 64)})
	if err := ImageLayers(map[string]any{"project": badProject}, nil, workspace, nil, imageLayersIO(new([]string))); err == nil || !strings.Contains(err.Error(), "integrity mismatch") {
		t.Fatalf("private credential bypassed the lock integrity: %v", err)
	}
	if _, err := os.Stat(filepath.Join(badProject, ".gen", "layers", "bin", "putnami")); !os.IsNotExist(err) {
		t.Fatal("unverified private bytes became an image layer")
	}
}

func TestPrivateCLIImageDownloadRefusesRedirectWithoutChangingCallerClient(t *testing.T) {
	useImageLayersRegistryToken(t, "pkt_origin_only")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("image download followed a redirect")
		_, _ = w.Write([]byte("unexpected"))
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer pkt_origin_only" {
			t.Error("native origin did not receive its credential")
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	client := origin.Client()
	dist := putServerDist{client: client, base: origin.URL}
	if body, err := dist.Open(context.Background(), "1.2.3"); err == nil {
		_ = body.Close()
		t.Fatal("redirect was accepted as a pinned CLI")
	}
	if client.CheckRedirect != nil || client.Timeout != 0 {
		t.Fatal("image download changed the shared caller client")
	}
}

func TestImageLayersReusesVerifiedCLIStoreWithoutCredentials(t *testing.T) {
	payload := []byte("pinned-runner-cli")
	integrity := sha256Hex(payload)
	store := t.TempDir()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", store)
	cached := filepath.Join(store, "cli", integrity, "putnami")
	if err := os.MkdirAll(filepath.Dir(cached), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cached, payload, 0o755); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("warm verified CLI unexpectedly required registry access")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	previous := imageLayersCLIDistFor
	t.Cleanup(func() { imageLayersCLIDistFor = previous })
	imageLayersCLIDistFor = func(*http.Client) imageLayersCLIDist {
		return putServerDist{client: server.Client(), base: server.URL}
	}
	workspace := imageLayersToolWorkspace(t)
	imageLayersLock(t, workspace, "1.2.3", map[string]string{"linux/amd64": integrity})
	project := imageLayersProject(t, `{"layers":[`+imageLayersCLILayer+`]}`)
	if err := ImageLayers(map[string]any{"project": project}, nil, workspace, nil, imageLayersIO(new([]string))); err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(filepath.Join(project, ".gen", "layers", "bin", "putnami"))
	if err != nil || string(installed) != string(payload) {
		t.Fatalf("cached CLI installation = %q, %v", installed, err)
	}
	if err := os.WriteFile(cached, []byte("tampered-cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	badProject := imageLayersProject(t, `{"layers":[`+imageLayersCLILayer+`]}`)
	if err := ImageLayers(map[string]any{"project": badProject}, nil, workspace, nil, imageLayersIO(new([]string))); err == nil || !strings.Contains(err.Error(), "integrity mismatch") {
		t.Fatalf("tampered cached CLI was accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(badProject, ".gen", "layers", "bin", "putnami")); !os.IsNotExist(err) {
		t.Fatal("tampered cached bytes became an image layer")
	}
}
