package distributioncli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

func TestPublishConfigMemberUsesImmutableManifestWithoutChannel(t *testing.T) {
	payload := []byte(`{"formatVersion":"config.authored-member.v1"}`)
	var body map[string]json.RawMessage
	server := newPutTestServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/put/workspace-native/my-app-config/publish" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if string(body["channel"]) != `""` || string(body["media_type"]) != `"application/vnd.putnami.config.authored-member+json"` || string(body["payload"]) != string(payload) {
			t.Fatalf("body = %s", mustJSON(t, body))
		}
		respondWithAtomicManifest(t, w, "workspace-native/my-app-config", body)
	}))
	defer server.Close()
	result, err := PublishConfigMember(map[string]any{"registry-put-url": server.URL}, t.TempDir(), nativePutCredentialEnv(t, server.URL), clicore.IO{Client: server.Client()}, ConfigMemberPublication{
		Namespace: "workspace-native", Package: "my-app-config", Version: "1.2.3", Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Coordinate != "workspace-native/my-app-config" || result.Version != "1.2.3" || result.ArtifactDigest != digestOf(payload) {
		t.Fatalf("result = %+v", result)
	}
}

func TestPublishConfigMemberCollisionRequiresExactReadback(t *testing.T) {
	payload := []byte(`{"formatVersion":"config.authored-member.v1"}`)
	for _, test := range []struct {
		name    string
		stored  []byte
		wantErr bool
	}{
		{name: "exact", stored: payload},
		{name: "different", stored: []byte(`{"formatVersion":"different"}`), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newPutTestServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				switch request.Method {
				case http.MethodPost:
					w.WriteHeader(http.StatusConflict)
				case http.MethodGet:
					if request.URL.Path != "/put/workspace-native/my-app-config/versions/1.2.3/manifest" {
						t.Fatalf("readback path = %s", request.URL.Path)
					}
					_ = json.NewEncoder(w).Encode(immutablePutManifest{
						ID: "manifest-1", PackageID: "package-1", MediaType: configAuthoredMemberMediaType, Payload: test.stored,
						CreatedAt: "2026-09-06T05:00:00Z",
					})
				default:
					t.Fatalf("method = %s", request.Method)
				}
			}))
			defer server.Close()
			result, err := PublishConfigMember(map[string]any{"registry-put-url": server.URL}, t.TempDir(), nativePutCredentialEnv(t, server.URL), clicore.IO{Client: server.Client()}, ConfigMemberPublication{
				Namespace: "workspace-native", Package: "my-app-config", Version: "1.2.3", Payload: payload,
			})
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "immutable version") {
					t.Fatalf("collision error = %v", err)
				}
				return
			}
			if err != nil || result.ArtifactDigest != digestOf(payload) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestPublishConfigMemberRejectsInvalidIdentityBeforeCredentials(t *testing.T) {
	requests := 0
	_, err := PublishConfigMember(nil, t.TempDir(), nil, clicore.IO{Client: &http.Client{Transport: workflowRoundTrip(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, nil
	})}}, ConfigMemberPublication{Namespace: "bad/path", Package: "app-config", Version: "1", Payload: []byte("{}")})
	if err == nil || requests != 0 {
		t.Fatalf("error=%v requests=%d", err, requests)
	}
}
