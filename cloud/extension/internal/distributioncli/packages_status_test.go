package distributioncli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	ciproto "go.putnami.dev/protocol/ci"
)

func TestPackagesStatusNodeFromMapsEveryProtocol(t *testing.T) {
	node := PackagesStatusNodeFrom([]PackageAnswer{
		{Protocol: "npm", Need: "putnami.ci.json declares distribution.registries.npm"},
		{Protocol: "gomod"},
		{Protocol: "oci", Need: "putnami.ci.json declares environments", Expected: "acme"},
		{Protocol: "put", Namespace: "acme", Need: "putnami.ci.json publishes release sets", Expected: "acme"},
	})
	want := strings.Join([]string{
		"packages  degraded  npm, oci not bound; go not needed; put bound",
		"",
		"  METRIC           VALUE  WINDOW",
		"  bindings active  1      now",
		"",
		"  STATE     CHECK  DETAIL",
		"  degraded  npm    not bound; putnami.ci.json declares distribution.registries.npm",
		"                   fix: putnami cloud packages namespaces activate --protocol npm --namespace <namespace> --idempotency-key <namespace>-npm",
		"  ok        go     not needed",
		"  degraded  oci    not bound; putnami.ci.json declares environments",
		"                   fix: putnami cloud packages namespaces activate --protocol oci --namespace acme --idempotency-key acme-oci",
		"  ok        put    namespace acme",
	}, "\n")
	if got := strings.Join(clicore.RenderStatusReport(node), "\n"); got != want {
		t.Fatalf("packages status = %q, want %q", got, want)
	}

	node = PackagesStatusNodeFrom([]PackageAnswer{
		{Protocol: "oci", Need: "putnami.ci.json declares environments", Expected: "acme", Shared: map[string]int{"putnami": 38}},
		{Protocol: "put", Namespace: "cloud", Need: "putnami.ci.json publishes release sets", Expected: "cloud"},
	})
	want = strings.Join([]string{
		"packages  ok  oci shared; put bound",
		"",
		"  METRIC           VALUE  WINDOW",
		"  bindings active  1      now",
		"",
		"  STATE  CHECK  DETAIL",
		"  ok     oci    not bound; publishes into namespace putnami through 38 package shares",
		"  ok     put    namespace cloud",
	}, "\n")
	if got := strings.Join(clicore.RenderStatusReport(node), "\n"); got != want {
		t.Fatalf("packages status through shares = %q, want %q", got, want)
	}
}

// packagesFixture is a linked workspace. Its putnami.ci.json declares the
// namespace acme, an environment and the npm registry, and
// putnami.workspace.json publishes images under oci.putnami.dev/images.
func packagesFixture(t *testing.T) (string, map[string]string, clicore.IO) {
	t.Helper()
	root, env, ioctx := distributionWorkflowFixture(t)
	files := map[string]string{
		ciproto.Filename: `{"version": 3, "commands": ["lint"],
  "distribution": {"namespace": "acme", "registries": {"npm": {}}},
  "envs": {"prod": {"channel": "stable"}}}`,
		"putnami.workspace.json": `{"name": "acme", "registries": {"oci": {"publish": "oci.putnami.dev/images"}}}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, env, ioctx
}

const (
	packagesBindingsPath = "/v1/workspaces/ws-consumer/distribution/bindings"
	packagesReceivedPath = "/v1/workspaces/ws-consumer/distribution/package-shares/received"
)

// packagesProvider serves distribution-api: the bindings read answers the put
// binding acme, the received shares read answers shares with sharesStatus.
// Every request is recorded as "METHOD path".
func packagesProvider(requests *[]string, sharesStatus int, shares []any) *http.Client {
	return &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
		*requests = append(*requests, request.Method+" "+request.URL.Path)
		if !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ") {
			return workflowJSONResponse(http.StatusUnauthorized, map[string]any{"message": "no bearer"}), nil
		}
		switch request.URL.Path {
		case packagesBindingsPath:
			return workflowJSONResponse(http.StatusOK, map[string]any{"bindings": []any{
				map[string]any{"id": "binding-1", "protocol": "put", "namespace": "acme"},
			}}), nil
		case packagesReceivedPath:
			if sharesStatus != http.StatusOK {
				return workflowJSONResponse(sharesStatus, map[string]any{"message": "distribution-api is draining"}), nil
			}
			return workflowJSONResponse(http.StatusOK, map[string]any{"shares": shares}), nil
		}
		return workflowJSONResponse(http.StatusNotFound, map[string]any{"message": "no route"}), nil
	})}
}

// receivedShare is one share another workspace granted ws-consumer.
func receivedShare(id, protocol, namespace, pkg, permission string, revokedOrder int64) map[string]any {
	share := map[string]any{
		"id": id, "binding_id": "binding-owner", "owner_workspace_id": "ws-owner", "grantee_workspace_id": "ws-consumer",
		"protocol": protocol, "namespace": namespace, "package": pkg, "permission": permission,
		"creator": map[string]any{"kind": "user", "id": "owner-user"}, "idempotency_key": "api:" + id,
		"created_at": "2026-09-08T12:00:00Z", "activated_order": 10,
	}
	if revokedOrder != 0 {
		share["revoked_order"] = revokedOrder
		share["revoked_at"] = "2026-09-09T12:00:00Z"
		share["revoked_by"] = map[string]any{"kind": "user", "id": "owner-user"}
		share["revocation_reason"] = "retired image"
	}
	return share
}

func TestPackagesStatusReadsTheBindings(t *testing.T) {
	root, env, ioctx := packagesFixture(t)
	var requests []string
	ioctx.Client = packagesProvider(&requests, http.StatusOK, []any{})
	var stdout []string
	ioctx.Stdout = func(line string) { stdout = append(stdout, line) }
	if err := PackagesStatus(map[string]any{}, nil, root, env, ioctx); err != nil {
		t.Fatalf("packages status: %v", err)
	}
	text := strings.Join(stdout, "\n")
	for _, line := range []string{
		"packages  degraded  npm, oci not bound; go not needed; put bound",
		"fix: putnami cloud packages namespaces activate --protocol oci --namespace images --idempotency-key images-oci",
		"  ok        put    namespace acme",
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("packages status = %q, want %q", text, line)
		}
	}
	if strings.Join(requests, "\n") != "GET "+packagesBindingsPath+"\nGET "+packagesReceivedPath {
		t.Fatalf("requests = %q, want the bindings read, then the received shares read for oci", requests)
	}

	stdout, requests = nil, nil
	if err := PackagesStatus(map[string]any{}, []string{"go"}, root, env, ioctx); err != nil {
		t.Fatalf("packages status go: %v", err)
	}
	if text := strings.Join(stdout, "\n"); text != "go  ok  not needed" {
		t.Fatalf("packages status go = %q", text)
	}
	if strings.Join(requests, "\n") != "GET "+packagesBindingsPath {
		t.Fatalf("requests = %q, want no shares read for go", requests)
	}
	stdout = nil
	if err := PackagesStatus(map[string]any{}, []string{"put"}, root, env, ioctx); err != nil {
		t.Fatalf("packages status put: %v", err)
	}
	if text := strings.Join(stdout, "\n"); text != "put  ok  namespace acme" {
		t.Fatalf("packages status put = %q", text)
	}
	for _, args := range [][]string{{"pip"}, {"npm", "oci"}} {
		if err := PackagesStatus(map[string]any{}, args, root, env, ioctx); clicore.ExitCode(err) != clicore.ExitUsage {
			t.Fatalf("packages status %v = %v, want a usage error", args, err)
		}
	}

	node := PackagesStatusNode(map[string]any{}, root, env, ioctx)
	if node.State != clicore.StatusDegraded || len(node.Children) != 4 || node.Children[3].Detail != "namespace acme" {
		t.Fatalf("node = %+v, want the same node the command prints", node)
	}
}

// TestPackagesStatusOCIReadyThroughPackageShares is the cloud workspace's
// shape: it publishes every image into a namespace another workspace owns,
// through exact publisher shares, and binds no OCI namespace of its own.
func TestPackagesStatusOCIReadyThroughPackageShares(t *testing.T) {
	root, env, ioctx := packagesFixture(t)
	var requests []string
	ioctx.Client = packagesProvider(&requests, http.StatusOK, []any{
		receivedShare("share-1", "oci", "putnami", "control-api", "publisher", 0),
		receivedShare("share-2", "oci", "putnami", "delivery-api", "publisher", 0),
		receivedShare("share-3", "oci", "putnami", "retired", "publisher", 12),
		receivedShare("share-4", "oci", "putnami", "base", "reader", 0),
		receivedShare("share-5", "npm", "@putnami", "ui", "publisher", 0),
		receivedShare("share-6", "oci", "tools", "runner", "publisher", 0),
	})
	var stdout []string
	ioctx.Stdout = func(line string) { stdout = append(stdout, line) }
	if err := PackagesStatus(map[string]any{}, []string{"oci"}, root, env, ioctx); err != nil {
		t.Fatalf("packages status oci: %v", err)
	}
	want := "oci  ok  not bound; publishes into namespace putnami through 2 package shares, namespace tools through 1 package share"
	if text := strings.Join(stdout, "\n"); text != want {
		t.Fatalf("packages status oci = %q, want %q", text, want)
	}
	node := PackagesStatusNode(map[string]any{}, root, env, ioctx)
	if node.Detail != "npm not bound; go not needed; oci shared; put bound" {
		t.Fatalf("node detail = %q", node.Detail)
	}
	if oci, _ := node.Child("packages.oci"); oci.State != clicore.StatusOK || oci.Fix != "" {
		t.Fatalf("oci = %+v, want ok without the activate command", oci)
	}
	// npm stays degraded: a share never clears another protocol.
	if npm, _ := node.Child("packages.npm"); node.State != clicore.StatusDegraded || npm.State != clicore.StatusDegraded {
		t.Fatalf("node = %+v, want npm still degraded", node)
	}
}

// TestPackagesStatusUnreadSharesKeepTheBindingAnswer: when the received shares
// cannot be read (a distribution-api that predates the route answers 404), oci
// keeps the degraded answer and says why the shares were not read.
func TestPackagesStatusUnreadSharesKeepTheBindingAnswer(t *testing.T) {
	root, env, ioctx := packagesFixture(t)
	for _, status := range []int{http.StatusNotFound, http.StatusServiceUnavailable} {
		var requests []string
		ioctx.Client = packagesProvider(&requests, status, nil)
		var stdout []string
		ioctx.Stdout = func(line string) { stdout = append(stdout, line) }
		if err := PackagesStatus(map[string]any{}, []string{"oci"}, root, env, ioctx); err != nil {
			t.Fatalf("an unreadable shares source = %v, want exit 0", err)
		}
		text := strings.Join(stdout, "\n")
		prefix := "oci  degraded  not bound; putnami.ci.json declares environments; package shares not read: request failed for https://control.example" + packagesReceivedPath
		if !strings.HasPrefix(text, prefix) ||
			!strings.Contains(text, "next: putnami cloud packages namespaces activate --protocol oci --namespace images --idempotency-key images-oci") {
			t.Fatalf("status %d: packages status oci = %q, want prefix %q and the activate command", status, text, prefix)
		}
	}
}

func TestPackagesStatusWithoutCINeedsNothing(t *testing.T) {
	root, env, ioctx := distributionWorkflowFixture(t)
	ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(*http.Request) (*http.Response, error) {
		return workflowJSONResponse(http.StatusOK, map[string]any{"bindings": []any{}}), nil
	})}
	node := PackagesStatusNode(map[string]any{}, root, env, ioctx)
	if node.State != clicore.StatusOK || node.Detail != "npm, go, oci, put not needed" {
		t.Fatalf("node = %+v, want every protocol not needed", node)
	}
}

func TestPackagesStatusReportsAFailedRead(t *testing.T) {
	root, env, ioctx := packagesFixture(t)
	ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(*http.Request) (*http.Response, error) {
		return workflowJSONResponse(http.StatusServiceUnavailable, map[string]any{"message": "distribution-api is draining"}), nil
	})}
	var stdout []string
	ioctx.Stdout = func(line string) { stdout = append(stdout, line) }
	if err := PackagesStatus(map[string]any{}, nil, root, env, ioctx); err != nil {
		t.Fatalf("an unavailable provider = %v, want an unknown node and exit 0", err)
	}
	if text := strings.Join(stdout, "\n"); !strings.HasPrefix(text, "packages  unknown  request failed for https://control.example/v1/workspaces/ws-consumer/distribution/bindings: distribution-api is draining") {
		t.Fatalf("packages status = %q", text)
	}

	ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(*http.Request) (*http.Response, error) {
		return workflowJSONResponse(http.StatusUnauthorized, map[string]any{"message": "token expired"}), nil
	})}
	if err := PackagesStatus(map[string]any{}, nil, root, env, ioctx); clicore.ExitCode(err) != clicore.ExitAuth {
		t.Fatalf("a refused session = %v, want exit %d", err, clicore.ExitAuth)
	}
	if node := PackagesStatusNode(map[string]any{}, root, env, ioctx); node.State != clicore.StatusUnknown || node.Fix != "putnami cloud login" {
		t.Fatalf("node = %+v, want unknown with the login command", node)
	}

	if err := os.WriteFile(filepath.Join(root, ciproto.Filename), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if node := PackagesStatusNode(map[string]any{}, root, env, ioctx); node.State != clicore.StatusUnknown || !strings.HasPrefix(node.Detail, "parse putnami.ci.json") {
		t.Fatalf("invalid putnami.ci.json = %+v, want unknown", node)
	}
}
