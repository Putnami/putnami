package cloudcli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	distributioncli "go.putnami.dev/cloud/extension/internal/distributioncli"
	protocol "go.putnami.dev/protocol/distribution"
)

// releaseSetRunFixture wires the `release-set` command exactly as a hosted run
// invokes it: a Put endpoint with a direct bearer, CI_RUN_ID set, and a request
// file holding one strictly valid release exchange.
func releaseSetRunFixture(t *testing.T) (map[string]string, string, *protocol.ReleaseResponse) {
	t.Helper()
	bearer := base64url(map[string]any{"alg": "none", "typ": "JWT"}) + "." +
		base64url(map[string]any{"aud": "distribution", "scope": "put", "sub": "ci-run"}) + ".test"
	env := map[string]string{
		"PUTNAMI_HOME":             t.TempDir(),
		"PUTNAMI_WORKSPACE_ROOT":   t.TempDir(),
		"PUTNAMI_REGISTRY_PUT_URL": "https://put.test",
		"PUTNAMI_CLOUD_TOKEN":      "ci-token",
		"CI_RUN_ID":                "run-1",
	}
	if err := distributioncli.WriteRegistriesState(env, &distributioncli.RegistriesState{
		Version: 1,
		Keys:    []distributioncli.KeyRef{{Registry: distributioncli.RegistryPut, Host: "put.test", URL: "https://put.test"}},
		PutAuth: map[string]string{"put.test": bearer},
	}); err != nil {
		t.Fatal(err)
	}

	releaseSet := protocol.ReleaseSet{
		ProtocolVersion: protocol.ProtocolVersion,
		Namespace:       "putnami",
		Members: []protocol.ReleaseSetMember{{
			Ecosystem:            protocol.Ecosystem("oci"),
			Coordinate:           "putnami/service",
			Version:              "1.0.0",
			ArtifactDigest:       "sha256:" + strings.Repeat("a", 64),
			Dependencies:         []protocol.ReleaseSetDependency{},
			SourceRevision:       strings.Repeat("1", 40),
			SelectionFingerprint: "sha256:" + strings.Repeat("2", 64),
		}},
	}
	ref, diagnostics := protocol.DeriveReleaseSetRef(&releaseSet)
	if len(diagnostics) > 0 {
		t.Fatalf("derive release-set ref: %+v", diagnostics)
	}
	request := protocol.ReleaseRequest{
		ProtocolVersion: protocol.ProtocolVersion,
		Namespace:       releaseSet.Namespace,
		ReleaseSet:      releaseSet,
		Channels:        []protocol.ChannelRequest{{Name: "canary", Visibility: protocol.VisibilityInternal}},
		Visibility:      protocol.VisibilityChain{Repo: protocol.VisibilityInternal, Versions: protocol.VersionVisibility{}},
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "release.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	response := &protocol.ReleaseResponse{
		ProtocolVersion: protocol.ProtocolVersion,
		Outcome:         protocol.ReleaseOutcomeReleased,
		Current:         map[string]*protocol.ChannelHead{"canary": {Ref: ref, Generation: 2}},
	}
	return env, path, response
}

// The CI run emits only its own lifecycle: the release-set node advances
// the channel, prints its one protocol line and hands nothing to Control. The
// HTTP double refuses every request that is not the Put release exchange, so a
// resurrected handoff fails this test instead of a production run.
func TestReleaseSetOnACIRunEmitsOneLineAndNoPublishHandoff(t *testing.T) {
	env, requestPath, response := releaseSetRunFixture(t)
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		seen = append(seen, request.Method+" "+request.URL.String())
		if request.URL.Host != "put.test" || !strings.HasPrefix(request.URL.Path, "/put/_/release-sets/") {
			t.Errorf("unexpected request %s %s", request.Method, request.URL)
			return jsonResponse(http.StatusNotFound, map[string]any{"error": "refused"}), nil
		}
		return jsonResponse(http.StatusOK, json.RawMessage(body)), nil
	})}

	var stdout []string
	err = RunCommand("release-set", IO{
		Env:    env,
		Client: client,
		Stdout: func(line string) { stdout = append(stdout, line) },
		Stderr: func(string) {},
		Now:    fixedNow,
	}, []string{protocol.ReleaseCommand, protocol.RequestFileFlag, requestPath})
	if err != nil {
		t.Fatalf("release-set release = %v, want exit 0", err)
	}
	if len(stdout) != 1 || !strings.Contains(stdout[0], string(protocol.ReleaseOutcomeReleased)) {
		t.Fatalf("stdout = %q, want exactly one protocol line", stdout)
	}
	if len(seen) != 1 || !strings.HasSuffix(seen[0], "/put/_/release-sets/"+protocol.ReleaseCommand) {
		t.Fatalf("requests = %q, want only the release exchange", seen)
	}
	for _, request := range seen {
		if strings.Contains(request, "publish-handoff") {
			t.Fatalf("release-set called the retired publish handoff: %s", request)
		}
	}
}
