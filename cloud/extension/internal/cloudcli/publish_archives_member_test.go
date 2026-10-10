package cloudcli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	distributioncli "go.putnami.dev/cloud/extension/internal/distributioncli"
	distributionproto "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/releaseset"
)

// TestPublishArchivesEmitsMemberOnlyAfterAcceptedPutResponse crosses the real
// executable entry point, publisher client and runtime emitter. The server-side IAM authorizer, Put
// handlers, blob store, and publish transaction are fixtures. This proves the
// client upload/request/response-validation path and exact published-member
// bytes; backend persistence and projection need their own composed proof.
func TestPublishArchivesEmitsMemberOnlyAfterAcceptedPutResponse(t *testing.T) {
	spectest.Proves(t, "cloud/cli-archive-publication", "member-only-after-accepted-put-response", "archive-publication-member-contract")
	workspaceRoot := archiveMemberWorkspace(t)
	bearer := jwt(map[string]any{"aud": "distribution", "scope": "put", "sub": "fixture-publisher"})
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls = append(calls, request.Method+" "+request.URL.Path)
		if got := request.Header.Get("Authorization"); got != "Bearer "+bearer {
			t.Fatalf("authorization = %q", got)
		}
		switch request.URL.Path {
		case "/put/putnami/cloud/blobs":
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			writeCreatedJSONResponse(t, w, map[string]any{
				"digest": archiveMemberDigest(body), "size": len(body),
			})
		case "/put/putnami/cloud/publish":
			var body map[string]json.RawMessage
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if string(body["version"]) != `"1.2.3"` || string(body["channel"]) != `""` {
				t.Fatalf("immutable publish request = %s", mustJSONBytes(t, body))
			}
			writeCreatedJSONResponse(t, w, map[string]any{
				"package": "putnami/cloud",
				"version": map[string]any{
					"id": "version-1", "package_id": "package-1", "version": "1.2.3",
					"manifest_id": "manifest-1", "state": "published", "visibility": "private",
				},
				"manifest": map[string]any{
					"id": "manifest-1", "package_id": "package-1",
					"media_type": "application/vnd.putnami.archive+json", "payload": body["payload"],
				},
				"channel": nil,
			})
		default:
			t.Fatalf("unexpected registry request: %s %s", request.Method, request.URL)
		}
	}))
	defer server.Close()

	parsedURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"PUTNAMI_HOME":             t.TempDir(),
		"PUTNAMI_REGISTRY_PUT_URL": server.URL,
	}
	if err := distributioncli.WriteRegistriesState(env, &distributioncli.RegistriesState{
		Version: 1,
		Keys: []distributioncli.KeyRef{{
			Registry: distributioncli.RegistryPut, Host: parsedURL.Host, URL: server.URL,
		}},
		PutAuth: map[string]string{parsedURL.Host: bearer},
	}); err != nil {
		t.Fatal(err)
	}
	contextFile := filepath.Join(t.TempDir(), "context.json")
	writeJSONFile(t, contextFile, map[string]any{
		"workspaceRoot": workspaceRoot,
		"params": map[string]any{
			"app": "apps/cli", "registry-put-url": server.URL, "channel": "stable,canary",
			releaseset.ContextParamName: &releaseset.Plan{
				ProtocolVersion: distributionproto.ProtocolVersion,
				Namespace:       "putnami-cloud",
				Channels:        []string{"stable", "canary"},
				Heads:           map[string]*distributionproto.ChannelHead{"stable": nil, "canary": nil},
				Members: []releaseset.PlannedMember{{
					Ecosystem: "archive", Coordinate: "putnami/cloud", Version: "1.2.3",
					SourceRevision: strings.Repeat("a", 40), SelectionFingerprint: "sha256:" + strings.Repeat("b", 64),
					ProjectID: "/apps/cli", Selected: true,
				}},
			},
		},
	})

	var output []string
	exitCode := RunMain([]string{"publish-archives", "--putnamiContext", contextFile}, IO{
		Env: env, Client: server.Client(),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
	})
	if exitCode != ExitSuccess {
		t.Fatalf("RunMain exit = %d, output = %v", exitCode, output)
	}
	if got, want := strings.Join(calls, ","), "POST /put/putnami/cloud/blobs,POST /put/putnami/cloud/publish"; got != want {
		t.Fatalf("registry calls = %q, want %q", got, want)
	}

	artifacts := eventsOfType(decodeEvents(t, output), "artifact")
	if len(artifacts) != 1 {
		t.Fatalf("artifact events = %+v, want one after atomic publish", artifacts)
	}
	event := artifacts[0]
	if event["kind"] != extensionproto.PublishedMemberEventKind || event["id"] != "archive-putnami-cloud" || event["name"] != "putnami/cloud" {
		t.Fatalf("artifact envelope = %+v", event)
	}
	platforms, ok := event["platforms"].(map[string]any)
	if !ok || platforms["linux/x64"] == nil {
		t.Fatalf("published platforms = %#v", event["platforms"])
	}
	member := extensionproto.PublishedMember{
		Ecosystem:      stringValue(event["ecosystem"]),
		Coordinate:     stringValue(event["coordinate"]),
		Version:        stringValue(event["version"]),
		ArtifactDigest: stringValue(event["artifactDigest"]),
		Platforms:      map[string]string{"linux/x64": stringValue(platforms["linux/x64"])},
	}
	if diagnostics := extensionproto.ValidatePublishedMember(&member); len(diagnostics) != 0 {
		t.Fatalf("published member = %+v diagnostics=%+v", member, diagnostics)
	}
	if member.Ecosystem != "archive" || member.Coordinate != "putnami/cloud" || member.Version != "1.2.3" {
		t.Fatalf("published member identity = %+v", member)
	}
	requireDeclaredMemberProfile(t, member.Ecosystem, member.Coordinate, member.Version)
}

func archiveMemberWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "apps", "cli")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "putnami.json"), []byte(`{"name":"apps/cli"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "putnami.extension.json"), []byte(`{"name":"@putnami/cloud"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	packageRoot := filepath.Join(root, ".putnami", "out", "apps", "cli", "package")
	archives := filepath.Join(packageRoot, "archives")
	if err := os.MkdirAll(archives, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "metadata.json"), []byte(`{
		"version":"1.2.3","artifact":"putnami-cloud","channels":["archives"]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archives, "putnami-cloud-linux-x64.tar.gz"), []byte("archive-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func archiveMemberDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func writeCreatedJSONResponse(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}
