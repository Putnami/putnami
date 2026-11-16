package collabcli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

const (
	screenshotsRequirement = "uploads-screenshots-to-delivery"
	screenshotsHead        = "0123456789abcdef0123456789abcdef01234567"
)

type deliveryRequest struct {
	method, path, contentType, authorization string
	body                                     []byte
}

// fakeDelivery is Putnami Delivery's screenshots surface at the HTTP
// boundary, the one external boundary of the provider. It holds the digests
// in held, answers the others as missing, and fails a route with the status
// in refuse.
type fakeDelivery struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	calls  []deliveryRequest
	held   map[string]bool
	refuse map[string]int
}

func newFakeDelivery(t *testing.T) *fakeDelivery {
	t.Helper()
	d := &fakeDelivery{t: t, held: map[string]bool{}, refuse: map[string]int{}}
	d.server = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.server.Close)
	return d
}

func (d *fakeDelivery) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, deliveryRequest{method: r.Method, path: r.URL.Path, contentType: r.Header.Get("Content-Type"),
		authorization: r.Header.Get("Authorization"), body: body})
	base := "/v1/workspaces/" + workspaceID + "/changes/pr-7/screenshots"
	route := "manifest"
	if strings.HasPrefix(r.URL.Path, base+"/captures/") {
		route = "capture"
	} else if r.URL.Path != base {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if status := d.refuse[route]; status != 0 {
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"code":"refused","message":"Delivery says no to the %s"}`, route)
		return
	}
	if route == "capture" {
		digest := strings.TrimPrefix(r.URL.Path, base+"/captures/")
		sum := sha256.Sum256(body)
		if r.Method != http.MethodPut || hex.EncodeToString(sum[:]) != digest {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		d.held[digest] = true
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var manifest screenshotManifest
	if r.Method != http.MethodPost || json.Unmarshal(body, &manifest) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	missing := []string{}
	for _, capture := range manifest.Captures {
		if !d.held[capture.SHA256] && !slices.Contains(missing, capture.SHA256) {
			missing = append(missing, capture.SHA256)
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"stored": true, "captures": len(manifest.Captures), "missing": missing})
}

func (d *fakeDelivery) requests() []deliveryRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]deliveryRequest(nil), d.calls...)
}

// screenshotsWorkspace is a workspace linked to delivery, with a manifest of
// the head and its capture files. Two captures share one image, so the
// manifest lists three captures over two digests.
func screenshotsWorkspace(t *testing.T, delivery *fakeDelivery) (root string, env map[string]string, manifest string, digests []string) {
	t.Helper()
	root, env = linkedWorkspace(t)
	link, _ := json.Marshal(map[string]any{"workspace_id": workspaceID, "control_plane_url": delivery.server.URL})
	if err := os.WriteFile(clicore.LinkPath(root), link, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".putnami", "screenshots", "apps-web")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	images := map[string]string{"01-light.png": "light pixels", "02-dark.png": "dark pixels", "03-light-again.png": "light pixels"}
	var captures []map[string]any
	for _, name := range []string{"01-light.png", "02-dark.png", "03-light-again.png"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(images[name]), 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(images[name]))
		digest := hex.EncodeToString(sum[:])
		if !slices.Contains(digests, digest) {
			digests = append(digests, digest)
		}
		captures = append(captures, map[string]any{"file": name, "route": "/tasks", "side": "after", "sha256": digest})
	}
	data, _ := json.MarshalIndent(map[string]any{"protocolVersion": 1, "project": "apps/web", "headSHA": screenshotsHead, "captures": captures}, "", "  ")
	manifest = filepath.Join(dir, "screenshots.json")
	if err := os.WriteFile(manifest, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return root, env, manifest, digests
}

func screenshotsArguments(manifest string) string {
	path, _ := json.Marshal(manifest)
	return `{"ref":{"source":"github:acme/app","id":"7"},"commit":"` + screenshotsHead + `","manifest":` + string(path) + `}`
}

func uploadScreenshots(t *testing.T, root string, env map[string]string, manifest string) (string, collab.Response) {
	t.Helper()
	return newHarness(t, root, env).call(t, collab.ContractProposals, collab.OperationScreenshots, screenshotsArguments(manifest), "")
}

func screenshotsResult(t *testing.T, response collab.Response) collab.ProposalScreenshotsResult {
	t.Helper()
	if response.Outcome != collab.OutcomeOK {
		t.Fatalf("outcome = %s: %+v", response.Outcome, response.Error)
	}
	var result collab.ProposalScreenshotsResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// TestScreenshotsUploadInTwoSteps: the manifest goes first, as written, under
// the session's bearer; then each missing file, once per digest, as
// image/png. A relative manifest path resolves against the workspace root.
func TestScreenshotsUploadInTwoSteps(t *testing.T) {
	spectest.Proves(t, feature, screenshotsRequirement, "collab-provider-uploads-screenshots-in-two-steps")
	delivery := newFakeDelivery(t)
	root, env, manifest, digests := screenshotsWorkspace(t, delivery)
	bearer := signIn(t, env)
	relative, err := filepath.Rel(root, manifest)
	if err != nil {
		t.Fatal(err)
	}
	raw, response := uploadScreenshots(t, root, env, relative)
	result := screenshotsResult(t, response)
	if result.Ref.ID != "7" || result.Commit != screenshotsHead || result.Project != "apps/web" || result.Captures != 3 || result.Uploaded != 2 {
		t.Fatalf("result = %+v, want 3 captures and 2 uploads for apps/web", result)
	}
	calls := delivery.requests()
	if len(calls) != 3 {
		t.Fatalf("Delivery got %d requests, want the manifest and two files: %+v", len(calls), calls)
	}
	written, _ := os.ReadFile(manifest)
	base := "/v1/workspaces/" + workspaceID + "/changes/pr-7/screenshots"
	if first := calls[0]; first.method != http.MethodPost || first.path != base || first.contentType != "application/json" || string(first.body) != string(written) {
		t.Fatalf("first request = %s %s (%s), want the manifest as written", first.method, first.path, first.contentType)
	}
	for i, digest := range digests {
		call := calls[i+1]
		if call.method != http.MethodPut || call.path != base+"/captures/"+digest || call.contentType != "image/png" {
			t.Errorf("request %d = %s %s (%s), want PUT of %s as image/png", i+1, call.method, call.path, call.contentType, digest)
		}
	}
	for _, call := range calls {
		if call.authorization != "Bearer "+bearer {
			t.Fatalf("%s %s did not present the session's workspace bearer", call.method, call.path)
		}
	}
	if strings.Contains(raw, bearer) {
		t.Fatal("the answer carries the bearer")
	}
}

// TestScreenshotsReuploadSendsTheManifestOnly: when Delivery holds every
// file, the second upload sends the manifest and no file.
func TestScreenshotsReuploadSendsTheManifestOnly(t *testing.T) {
	spectest.Proves(t, feature, screenshotsRequirement, "collab-provider-reuploads-nothing-when-nothing-is-missing")
	delivery := newFakeDelivery(t)
	root, env, manifest, _ := screenshotsWorkspace(t, delivery)
	signIn(t, env)
	_, first := uploadScreenshots(t, root, env, manifest)
	screenshotsResult(t, first)
	before := len(delivery.requests())
	_, second := uploadScreenshots(t, root, env, manifest)
	if result := screenshotsResult(t, second); result.Captures != 3 || result.Uploaded != 0 {
		t.Fatalf("second result = %+v, want 3 captures and no upload", result)
	}
	calls := delivery.requests()[before:]
	if len(calls) != 1 || calls[0].method != http.MethodPost {
		t.Fatalf("the second upload sent %+v, want the manifest only", calls)
	}
}

// TestScreenshotsRefusals: Delivery's refusals keep their outcome and its
// message, a route it does not serve is unsupported, what the provider can
// see is wrong is refused before it is sent, and an unknown outcome is
// unresolved with the repeat as its reconciliation.
func TestScreenshotsRefusals(t *testing.T) {
	spectest.Proves(t, feature, screenshotsRequirement, "collab-provider-screenshots-refusals")
	for _, tc := range []struct {
		name, route string
		status      int
		outcome     collab.Outcome
		reason      string
		requests    int
	}{
		{"a head that is not a commit of the change", "manifest", http.StatusConflict, collab.OutcomeConflict, collab.ReasonRevisionConflict, 1},
		{"a manifest above the bound", "manifest", http.StatusRequestEntityTooLarge, collab.OutcomeInvalid, reasonScreenshotsInvalid, 1},
		{"a route Delivery does not serve", "manifest", http.StatusNotFound, collab.OutcomeUnsupported, reasonScreenshotsNotServed, 1},
		{"a person who may not submit", "manifest", http.StatusForbidden, collab.OutcomeDenied, reasonScreenshotsRefused, 1},
		{"a file Delivery refuses", "capture", http.StatusUnprocessableEntity, collab.OutcomeInvalid, reasonScreenshotsInvalid, 2},
		{"a file above the bound", "capture", http.StatusRequestEntityTooLarge, collab.OutcomeInvalid, reasonScreenshotsInvalid, 2},
		{"a digest no manifest names", "capture", http.StatusNotFound, collab.OutcomeNotFound, reasonNotFound, 2},
		{"a server error on the manifest", "manifest", http.StatusBadGateway, collab.OutcomeUnresolved, reasonUnanswered, 1},
		{"a server error on a file", "capture", http.StatusInternalServerError, collab.OutcomeUnresolved, reasonUnanswered, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delivery := newFakeDelivery(t)
			delivery.refuse[tc.route] = tc.status
			root, env, manifest, _ := screenshotsWorkspace(t, delivery)
			signIn(t, env)
			_, response := uploadScreenshots(t, root, env, manifest)
			if response.Outcome != tc.outcome || response.Error == nil || response.Error.Reason != tc.reason {
				t.Fatalf("answer = %s %+v, want %s %s", response.Outcome, response.Error, tc.outcome, tc.reason)
			}
			if got := len(delivery.requests()); got != tc.requests {
				t.Fatalf("Delivery got %d requests, want %d: nothing is sent again or after a refusal", got, tc.requests)
			}
			if tc.status < 500 && !strings.Contains(response.Error.Message, "Delivery says no to the "+tc.route) {
				t.Errorf("message %q does not carry Delivery's message", response.Error.Message)
			}
			if tc.outcome == collab.OutcomeUnresolved && (response.Error.Retryable || !strings.Contains(response.Error.Reconcile, "proposals.screenshots")) {
				t.Errorf("unresolved without its repeat: %+v", response.Error)
			}
		})
	}
	local := func(name string, spoil func(t *testing.T, manifest string) string, requests int) {
		t.Run(name, func(t *testing.T) {
			delivery := newFakeDelivery(t)
			root, env, manifest, _ := screenshotsWorkspace(t, delivery)
			signIn(t, env)
			arguments := spoil(t, manifest)
			_, response := newHarness(t, root, env).call(t, collab.ContractProposals, collab.OperationScreenshots, arguments, "")
			if response.Outcome != collab.OutcomeInvalid || response.Error == nil {
				t.Fatalf("answer = %s %+v, want invalid", response.Outcome, response.Error)
			}
			if got := len(delivery.requests()); got != requests {
				t.Fatalf("Delivery got %d requests, want %d", got, requests)
			}
		})
	}
	local("a manifest of another commit", func(_ *testing.T, manifest string) string {
		return strings.Replace(screenshotsArguments(manifest), screenshotsHead, strings.Repeat("f", 40), 1)
	}, 0)
	local("a manifest that is not JSON", func(t *testing.T, manifest string) string {
		if err := os.WriteFile(manifest, []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		return screenshotsArguments(manifest)
	}, 0)
	local("a missing manifest", func(_ *testing.T, manifest string) string {
		return screenshotsArguments(manifest + ".absent")
	}, 0)
	local("a proposal that is no pull request number", func(_ *testing.T, manifest string) string {
		return strings.Replace(screenshotsArguments(manifest), `"id":"7"`, `"id":"seven"`, 1)
	}, 0)
	local("a file that no longer hashes to its digest", func(t *testing.T, manifest string) string {
		if err := os.WriteFile(filepath.Join(filepath.Dir(manifest), "02-dark.png"), []byte("other pixels"), 0o600); err != nil {
			t.Fatal(err)
		}
		return screenshotsArguments(manifest)
	}, 1)
	local("a file that is gone", func(t *testing.T, manifest string) string {
		if err := os.Remove(filepath.Join(filepath.Dir(manifest), "01-light.png")); err != nil {
			t.Fatal(err)
		}
		return screenshotsArguments(manifest)
	}, 1)
}

// TestScreenshotsNeedAPerson: a process that holds only PUTNAMI_CLOUD_TOKEN
// sends nothing and answers unsupported; without any credential it is denied
// and names putnami cloud login; a stored session wins over the token.
func TestScreenshotsNeedAPerson(t *testing.T) {
	spectest.Proves(t, feature, screenshotsRequirement, "collab-provider-screenshots-need-a-person")
	t.Run("a machine token alone", func(t *testing.T) {
		delivery := newFakeDelivery(t)
		root, env, manifest, _ := screenshotsWorkspace(t, delivery)
		env[cloudTokenVar] = envToken
		raw, response := uploadScreenshots(t, root, env, manifest)
		if response.Outcome != collab.OutcomeUnsupported || response.Error == nil || response.Error.Reason != reasonScreenshotsSession {
			t.Fatalf("answer = %s %+v, want unsupported %s", response.Outcome, response.Error, reasonScreenshotsSession)
		}
		if got := len(delivery.requests()); got != 0 {
			t.Fatalf("Delivery got %d requests under a machine token, want none", got)
		}
		if strings.Contains(raw, envToken) {
			t.Fatal("the answer carries the machine token")
		}
	})
	t.Run("no credential", func(t *testing.T) {
		delivery := newFakeDelivery(t)
		root, env, manifest, _ := screenshotsWorkspace(t, delivery)
		_, response := uploadScreenshots(t, root, env, manifest)
		if response.Outcome != collab.OutcomeDenied || response.Error == nil || !strings.Contains(response.Error.Message, "putnami cloud login") {
			t.Fatalf("answer = %s %+v, want denied naming putnami cloud login", response.Outcome, response.Error)
		}
		if got := len(delivery.requests()); got != 0 {
			t.Fatalf("Delivery got %d requests without a credential, want none", got)
		}
	})
	t.Run("a session beside a machine token", func(t *testing.T) {
		delivery := newFakeDelivery(t)
		root, env, manifest, _ := screenshotsWorkspace(t, delivery)
		bearer := signIn(t, env)
		env[cloudTokenVar] = envToken
		_, response := uploadScreenshots(t, root, env, manifest)
		screenshotsResult(t, response)
		for _, call := range delivery.requests() {
			if call.authorization != "Bearer "+bearer {
				t.Fatalf("%s %s presented another bearer than the session's", call.method, call.path)
			}
		}
	})
}

// TestScreenshotsRedactARenewedBearerAndRefuseARedirect: after a 401 the
// upload renews the bearer once and sends again with it, and the answer
// carries neither bearer even when Delivery echoes one; a redirect is
// refused, so no bearer reaches another origin.
func TestScreenshotsRedactARenewedBearerAndRefuseARedirect(t *testing.T) {
	spectest.Proves(t, feature, "keeps-the-bearer-out-of-the-answer", "collab-provider-redacts-the-bearer")
	const stale, fresh = "stale-bearer-secret", "renewed-bearer-secret"
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		if len(seen) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `{"message":"refused for %s"}`, r.Header.Get("Authorization"))
	}))
	defer server.Close()
	p := &Provider{}
	workspace := &clicore.WorkspaceContext{WorkspaceID: workspaceID, ControlPlane: server.URL, AuthToken: clicore.NewBearer(stale),
		IO: clicore.IO{Client: server.Client()}, RefreshAuth: func() (clicore.Bearer, error) { return clicore.NewBearer(fresh), nil }}
	p.watchBearer(workspace)
	_, err := (&deliveryScreenshots{workspace: workspace, number: "7"}).postManifest(context.Background(), []byte(`{}`))
	if !slices.Equal(seen, []string{"Bearer " + stale, "Bearer " + fresh}) {
		t.Fatalf("Delivery saw %d requests; want the stale bearer, then the renewed one, once each", len(seen))
	}
	answer, _ := json.Marshal(screenshotsFailure(err))
	if redacted := string(p.Redact(answer)); strings.Contains(redacted, stale) || strings.Contains(redacted, fresh) || !strings.Contains(redacted, "[REDACTED]") {
		t.Fatalf("the answer keeps a bearer: %s", redacted)
	}

	elsewhere := 0
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere++ }))
	defer other.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer redirecting.Close()
	workspace = &clicore.WorkspaceContext{WorkspaceID: workspaceID, ControlPlane: redirecting.URL, AuthToken: clicore.NewBearer(stale),
		IO: clicore.IO{Client: redirecting.Client()}}
	_, err = (&deliveryScreenshots{workspace: workspace, number: "7"}).postManifest(context.Background(), []byte(`{}`))
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Status != http.StatusFound || elsewhere != 0 {
		t.Fatalf("a redirect: err %v, %d requests elsewhere; want a 302 refusal and none", err, elsewhere)
	}
}
