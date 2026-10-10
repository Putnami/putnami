package distributioncli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/releaseset"
)

// A canonical declaration: '<' and '&' are written as < and &, the
// form encoding/json produces, and must reach Put exactly as the producer
// wrote them.
var deploymentMemberPayload = []byte(`{"protocolVersion":2,"workload":"shop/workloads/api","secrets":[{"name":"api-token","sources":[{"project":"a\u003cb\u0026c","contributor":"manual"}]}]}`)

func TestPublishDeploymentMemberStoresTheExactBytesWithoutChannel(t *testing.T) {
	var raw []byte
	server := newPutTestServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/put/cloud/shop-workloads-api-deployment/publish" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		var err error
		if raw, err = io.ReadAll(request.Body); err != nil {
			t.Fatal(err)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if string(body["channel"]) != `""` || string(body["version"]) != `"rs-1"` ||
			string(body["media_type"]) != `"application/vnd.putnami.infra.deployment.v2+json"` {
			t.Fatalf("body = %s", raw)
		}
		respondWithAtomicManifest(t, w, "cloud/shop-workloads-api-deployment", body)
	}))
	defer server.Close()
	result, err := PublishDeploymentMember(map[string]any{"registry-put-url": server.URL}, t.TempDir(), nativePutCredentialEnv(t, server.URL), clicore.IO{Client: server.Client()}, DeploymentMemberPublication{
		Namespace: "cloud", Package: "shop-workloads-api-deployment", Version: "rs-1", Payload: deploymentMemberPayload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, append([]byte(`"payload":`), deploymentMemberPayload...)) {
		t.Fatalf("request did not carry the payload byte for byte: %s", raw)
	}
	if result.Coordinate != "cloud/shop-workloads-api-deployment" || result.Version != "rs-1" || result.ArtifactDigest != digestOf(deploymentMemberPayload) {
		t.Fatalf("result = %+v", result)
	}
}

func TestPublishDeploymentMemberCollisionRequiresTheSameBytesAndMediaType(t *testing.T) {
	for _, test := range []struct {
		name      string
		mediaType string
		stored    []byte
		wantErr   bool
	}{
		{name: "exact", mediaType: DeploymentMemberMediaType, stored: deploymentMemberPayload},
		{name: "different bytes", mediaType: DeploymentMemberMediaType, stored: []byte(`{"protocolVersion":2,"workload":"other"}`), wantErr: true},
		{name: "different media type", mediaType: configAuthoredMemberMediaType, stored: deploymentMemberPayload, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newPutTestServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				switch request.Method {
				case http.MethodPost:
					w.WriteHeader(http.StatusConflict)
				case http.MethodGet:
					if request.URL.Path != "/put/cloud/shop-workloads-api-deployment/versions/rs-1/manifest" {
						t.Fatalf("readback path = %s", request.URL.Path)
					}
					_ = json.NewEncoder(w).Encode(immutablePutManifest{
						ID: "manifest-1", PackageID: "package-1", MediaType: test.mediaType, Payload: test.stored,
						CreatedAt: "2026-10-03T05:00:00Z",
					})
				default:
					t.Fatalf("method = %s", request.Method)
				}
			}))
			defer server.Close()
			result, err := PublishDeploymentMember(map[string]any{"registry-put-url": server.URL}, t.TempDir(), nativePutCredentialEnv(t, server.URL), clicore.IO{Client: server.Client()}, DeploymentMemberPublication{
				Namespace: "cloud", Package: "shop-workloads-api-deployment", Version: "rs-1", Payload: deploymentMemberPayload,
			})
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "deployment member publication collision does not match") {
					t.Fatalf("collision error = %v", err)
				}
				return
			}
			if err != nil || result.ArtifactDigest != digestOf(deploymentMemberPayload) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

// invalidDeploymentPublications break the immutable Put identity rule one way
// each; both the direct and the outbox path refuse them before any effect.
func invalidDeploymentPublications() map[string]func(*DeploymentMemberPublication) {
	return map[string]func(*DeploymentMemberPublication){
		"namespace path":   func(p *DeploymentMemberPublication) { p.Namespace = "cloud/x" },
		"package case":     func(p *DeploymentMemberPublication) { p.Package = "Api-deployment" },
		"empty version":    func(p *DeploymentMemberPublication) { p.Version = "" },
		"version segment":  func(p *DeploymentMemberPublication) { p.Version = "rs/1" },
		"version space":    func(p *DeploymentMemberPublication) { p.Version = " rs-1" },
		"empty payload":    func(p *DeploymentMemberPublication) { p.Payload = nil },
		"oversize payload": func(p *DeploymentMemberPublication) { p.Payload = bytes.Repeat([]byte("a"), 4<<20+1) },
	}
}

func TestPublishDeploymentMemberRefusesAnInvalidIdentityBeforeCredentials(t *testing.T) {
	valid := DeploymentMemberPublication{Namespace: "cloud", Package: "api-deployment", Version: "rs-1", Payload: []byte("{}")}
	for name, mutate := range invalidDeploymentPublications() {
		t.Run(name, func(t *testing.T) {
			publication := valid
			mutate(&publication)
			requests := 0
			_, err := PublishDeploymentMember(nil, t.TempDir(), nil, clicore.IO{Client: &http.Client{Transport: workflowRoundTrip(func(*http.Request) (*http.Response, error) {
				requests++
				return nil, nil
			})}}, publication)
			if err == nil || !strings.Contains(err.Error(), "invalid immutable Put identity") || requests != 0 {
				t.Fatalf("error=%v requests=%d", err, requests)
			}
		})
	}
}

// Under publication-v1 the deployment step packs the bytes the direct path
// publishes: the same manifest payload under the same media type, no blob,
// and so the same artifact digest. The engine admits the packed member as
// the planned deployment member and its upload check accepts it.
func TestPackDeploymentMemberPacksTheBytesPublishDeploymentMemberPublishes(t *testing.T) {
	publication := DeploymentMemberPublication{Namespace: "cloud", Package: "shop-workloads-api-deployment", Version: "rs-1", Payload: deploymentMemberPayload}
	direct, srv := newRecordingPutServer(t)
	published, err := PublishDeploymentMember(map[string]any{"registry-put-url": srv.URL}, t.TempDir(), nativePutCredentialEnv(t, srv.URL), clicore.IO{Client: srv.Client()}, publication)
	if err != nil {
		t.Fatalf("direct publish: %v", err)
	}
	if string(direct.channel) != `""` {
		t.Fatalf("direct publish channel = %s, want none", direct.channel)
	}

	outbox := filepath.Join(t.TempDir(), "outbox")
	packedResult, err := PackDeploymentMember(outbox, "/shop/workloads/api", publication)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	packed := readPackedMember(t, outbox)
	requireSameBytes(t, packed, direct)
	planned := releaseset.PlannedMember{Ecosystem: "put", Coordinate: "cloud/shop-workloads-api-deployment", Version: "rs-1", ProjectID: "/shop/workloads/api"}
	requireEngineAdmits(t, packed, planned, "deployment", "cloud-publish-deployment")
	if len(packed.member.Put.Blobs) != 0 || !reflect.DeepEqual(packedResult, published) {
		t.Fatalf("packed result %+v with %d blobs, direct result %+v", packedResult, len(packed.member.Put.Blobs), published)
	}
}

func TestPackDeploymentMemberRefusesBeforeWritingADescriptor(t *testing.T) {
	valid := DeploymentMemberPublication{Namespace: "cloud", Package: "shop-workloads-api-deployment", Version: "rs-1", Payload: deploymentMemberPayload}
	for name, mutate := range invalidDeploymentPublications() {
		t.Run(name, func(t *testing.T) {
			publication := valid
			mutate(&publication)
			outbox := filepath.Join(t.TempDir(), "outbox")
			if _, err := PackDeploymentMember(outbox, "/shop/workloads/api", publication); err == nil || !strings.Contains(err.Error(), "invalid immutable Put identity") {
				t.Fatalf("PackDeploymentMember error = %v", err)
			}
			if _, statErr := os.Stat(outbox); !os.IsNotExist(statErr) {
				t.Fatalf("a refused pack touched the outbox: %v", statErr)
			}
		})
	}
	for name, test := range map[string]struct {
		payload   []byte
		projectID string
		want      string
	}{
		// A valid Put payload that is no canonical deployment declaration: the
		// engine's put-write/v1 upload check refuses it, so the step does too.
		"not a declaration": {payload: []byte(`{"workload":"shop/workloads/api"}`), projectID: "/shop/workloads/api", want: "deployment manifest"},
		"non-canonical":     {payload: []byte(`{"workload":"shop/workloads/api","protocolVersion":2}`), projectID: "/shop/workloads/api", want: "deployment manifest"},
		"no project":        {payload: deploymentMemberPayload, want: "project"},
	} {
		t.Run(name, func(t *testing.T) {
			publication := valid
			publication.Payload = test.payload
			outbox := filepath.Join(t.TempDir(), "outbox")
			if _, err := PackDeploymentMember(outbox, test.projectID, publication); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("PackDeploymentMember error = %v, want %q", err, test.want)
			}
			if _, statErr := os.Stat(filepath.Join(outbox, extensionproto.PublicationOutboxDescriptor)); !os.IsNotExist(statErr) {
				t.Fatalf("a refused pack wrote a descriptor: %v", statErr)
			}
		})
	}
}
