package compose

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// hangDetector bounds every wait in these tests. It detects a hang; it is not a
// latency assertion.
const hangDetector = 60 * time.Second

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func leaseDirs(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(Root(root))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read leases: %v", err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestUp_StartsMembersInOrderAndWaitsForTypedReady(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "typed-readiness-bounds-every-member",
		"a-member-starts-only-after-the-one-before-it-announced-readiness")
	spectest.Proves(t, "cli/workload-qualification", "runs-with-closure-served-behind-stable-proxies",
		"dependants-reach-a-provider-through-its-injected-proxy-url")
	spectest.Proves(t, "cli/workload-qualification", "ephemeral-databases-per-composition",
		"each-datasource-gets-a-database-created-and-dropped-with-the-composition")
	fixture := newCompositionFixture(t,
		fixtureProject{id: "/provider", name: "go.acme.dev/provider"},
		fixtureProject{
			id: "/consumer", name: "@acme/consumer", runsWith: []string{"go.acme.dev/provider"}, mode: fixtureRecordFirst,
			requirements: `{"protocolVersion":2,"databases":[{"name":"default","engine":"postgres","schemas":["public"]}]}`,
		},
	)
	writeFile(t, filepath.Join(fixture.root, "provider", "schema", "openapi.json"),
		`{"x-putnami-client":{"protocolVersion":1,"service":{"id":"items","audience":"urn:acme:items"},"credentials":{}}}`)
	isolator := &fakeIsolator{}

	ctx, cancel := context.WithTimeout(context.Background(), hangDetector)
	defer cancel()
	comp, err := up(ctx, Options{
		WorkspaceRoot: fixture.root,
		Target:        fixture.projects["/consumer"],
		ProxyPort:     0,
		WatchTarget:   true,
		ReadyTimeout:  hangDetector,
	}, fixture.deps(isolator))
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			comp.Close(context.Background())
		}
	}()

	if !slices.Equal(fixture.prepared, []string{"/provider", "/consumer"}) {
		t.Errorf("prepared %v, want the closure in start order", fixture.prepared)
	}
	status := comp.Status()
	if len(status.Members) != 2 || status.Isolation != IsolationDatabase || status.Target != "/consumer" {
		t.Fatalf("status = %+v", status)
	}
	provider, consumer := status.Members[0], status.Members[1]
	if provider.BackendPort == 0 || consumer.BackendPort == 0 {
		t.Fatalf("a ready member has no backend port: %+v", status.Members)
	}
	if !slices.Equal(consumer.ConfigSections, []string{SectionClients, SectionDatabase}) || len(provider.ConfigSections) != 0 {
		t.Errorf("config sections: provider %v, consumer %v", provider.ConfigSections, consumer.ConfigSections)
	}

	// The consumer started after the provider was ready: its first act was a
	// request to the provider's proxy URL, and that request was answered by
	// the provider itself.
	record := fixture.record(t, "/consumer")
	if !strings.HasPrefix(record.DependencyReply, "200 /provider") {
		t.Errorf("the consumer's startup call to its provider got %q", record.DependencyReply)
	}
	if !slices.Equal(record.ServiceKeys, []string{"go.acme.dev/provider", "items"}) {
		t.Errorf("service keys = %v, want the contract id and the project name", record.ServiceKeys)
	}
	if record.Port != "0" || record.NodeEnv != "" {
		t.Errorf("watched target env: PORT=%q NODE_ENV=%q, want 0 and development", record.Port, record.NodeEnv)
	}
	if providerRecord := fixture.record(t, "/provider"); providerRecord.NodeEnv != "production" || providerRecord.Port != "0" || len(providerRecord.ConfigSections) != 0 {
		t.Errorf("dependency env = %+v, want production mode, port 0, no document", providerRecord)
	}
	if code, body := get(t, provider.ProxyURL+"/whoami"); code != 200 || !strings.HasPrefix(body, "/provider host=127.0.0.1:") {
		t.Errorf("provider proxy answered %d %q", code, body)
	}
	if endpoint, ok := comp.Endpoint("/consumer"); !ok || endpoint != consumer.ProxyURL {
		t.Errorf("Endpoint(/consumer) = %q, %v", endpoint, ok)
	}

	if len(isolator.created) != 1 || !strings.HasPrefix(isolator.created[0], databasePrefix(comp.ID())) {
		t.Errorf("created databases = %v", isolator.created)
	}
	data, err := os.ReadFile(filepath.Join(Root(fixture.root), comp.ID(), leaseFileName))
	if err != nil {
		t.Fatalf("read lease: %v", err)
	}
	var lease Lease
	if err := json.Unmarshal(data, &lease); err != nil {
		t.Fatalf("decode lease: %v", err)
	}
	if lease.PID != os.Getpid() || len(lease.PGIDs) != 2 || !slices.Equal(lease.Databases, isolator.created) ||
		lease.ProvisionerDigest != "fakedigest" || len(lease.ProxyPorts) != 2 {
		t.Errorf("lease = %+v", lease)
	}
	if strings.Contains(string(data), fakePassword) {
		t.Errorf("the lease carries the database password: %s", data)
	}

	report := comp.Close(context.Background())
	closed = true
	if report.State != CleanupClean {
		t.Fatalf("cleanup = %+v", report)
	}
	for _, pgid := range lease.PGIDs {
		if processGroupAlive(pgid) {
			t.Errorf("process group %d survived Close", pgid)
		}
	}
	if !slices.Equal(isolator.dropped, isolator.created) {
		t.Errorf("dropped %v, want %v", isolator.dropped, isolator.created)
	}
	if dirs := leaseDirs(t, fixture.root); len(dirs) != 0 {
		t.Errorf("lease directories left after a clean Close: %v", dirs)
	}
	if again := comp.Close(context.Background()); again.State != CleanupClean {
		t.Errorf("a second Close = %+v", again)
	}
}

func TestUp_ReadyTimeoutNamesMemberAndPhase(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "typed-readiness-bounds-every-member",
		"a-member-that-never-announces-readiness-fails-naming-member-and-phase")
	fixture := newCompositionFixture(t,
		fixtureProject{id: "/provider", name: "provider", mode: fixtureNeverReady},
		fixtureProject{id: "/consumer", name: "consumer", runsWith: []string{"provider"}},
	)
	ctx, cancel := context.WithTimeout(context.Background(), hangDetector)
	defer cancel()
	_, err := up(ctx, Options{
		WorkspaceRoot: fixture.root,
		Target:        fixture.projects["/consumer"],
		ReadyTimeout:  2 * time.Second,
	}, fixture.deps(nil))
	composeErr := composeError(t, err)
	if composeErr.Code != CodeReadyTimeout || composeErr.Member != "/provider" || composeErr.Phase != PhaseReadiness {
		t.Fatalf("error = %v, want compose.ready_timeout for /provider in readiness", err)
	}
	if !slices.ContainsFunc(composeErr.Detail, func(line string) bool { return strings.Contains(line, "fixture /provider starting") }) {
		t.Errorf("the error carries no output of the member: %q", composeErr.Detail)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "consumer", "record.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the consumer started although its provider never became ready")
	}
	if dirs := leaseDirs(t, fixture.root); len(dirs) != 0 {
		t.Errorf("a failed start left leases behind: %v", dirs)
	}
	// The teardown of what the failed start acquired travels on the error, so a
	// caller records it instead of assuming it.
	if composeErr.Cleanup == nil || composeErr.Cleanup.State != CleanupClean || len(composeErr.Cleanup.Leftovers) != 0 {
		t.Errorf("error cleanup = %+v, want the clean teardown report", composeErr.Cleanup)
	}

	// A start refused before it acquired anything has no teardown to report.
	_, err = up(ctx, Options{WorkspaceRoot: fixture.root}, fixture.deps(nil))
	if refused := composeError(t, err); refused.Cleanup != nil {
		t.Errorf("a refused start reports a teardown: %+v", refused.Cleanup)
	}
}

func TestUp_MemberThatExitsBeforeReadyFailsWithoutWaitingForTheTimeout(t *testing.T) {
	fixture := newCompositionFixture(t, fixtureProject{id: "/app", name: "app", mode: fixtureExitEarly})
	for _, watchTarget := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), hangDetector)
		_, err := up(ctx, Options{
			WorkspaceRoot: fixture.root,
			Target:        fixture.projects["/app"],
			WatchTarget:   watchTarget,
			// Far longer than the test's own hang detector: only the exit can
			// end this wait in time.
			ReadyTimeout: 10 * hangDetector,
		}, fixture.deps(nil))
		cancel()
		composeErr := composeError(t, err)
		if composeErr.Code != CodeMemberExited || composeErr.Member != "/app" || composeErr.Phase != PhaseReadiness {
			t.Fatalf("watch=%v: error = %v, want compose.member_exited for /app", watchTarget, err)
		}
		if !strings.Contains(strings.Join(composeErr.Detail, "\n"), "cannot start") {
			t.Errorf("watch=%v: error detail lacks the member's output: %q", watchTarget, composeErr.Detail)
		}
		if strings.Contains(composeErr.Error(), "cannot start") {
			t.Errorf("watch=%v: the error message carries the member's output: %v", watchTarget, err)
		}
	}
}

func TestUp_ReportsOrphansReapedBeforeAFailure(t *testing.T) {
	fixture := newCompositionFixture(t, fixtureProject{id: "/app", name: "app", mode: fixtureExitEarly})
	orphan := writeOrphanLease(t, fixture.root, Lease{PGIDs: []int{}})
	_, err := up(t.Context(), Options{WorkspaceRoot: fixture.root, Target: fixture.projects["/app"]}, fixture.deps(nil))
	composeErr := composeError(t, err)
	if len(composeErr.Reaped) != 1 || composeErr.Reaped[0].ID != orphan {
		t.Fatalf("reaped on failure = %+v, want the orphan %s", composeErr.Reaped, orphan)
	}
}
