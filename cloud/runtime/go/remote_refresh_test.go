package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// refreshClock is a manual clock for a remote load.
type refreshClock struct {
	mu  sync.Mutex
	now time.Time
}

func newRefreshClock() *refreshClock {
	return &refreshClock{now: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
}

func (c *refreshClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *refreshClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// waitRefresh blocks until the background refresh running now, if any, ends.
func waitRefresh(l *remoteLoad) {
	l.mu.Lock()
	done := l.refreshing
	l.mu.Unlock()
	if done != nil {
		<-done
	}
}

// versionedServer answers each request with the current value of its
// version under key, wrapped as field ("config", "secrets"). A resolve server
// adds the resolve flags; the prepared boot server answers the tree alone.
type versionedServer struct {
	*httptest.Server
	calls   atomic.Int32
	version atomic.Int32
	status  atomic.Int32
}

func newVersionedServer(t *testing.T, field string) *versionedServer {
	return newVersionedServerWithFlags(t, field, true)
}

func newVersionedServerWithFlags(t *testing.T, field string, resolveFlags bool) *versionedServer {
	t.Helper()
	s := &versionedServer{}
	s.version.Store(1)
	s.status.Store(http.StatusOK)
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.calls.Add(1)
		if status := int(s.status.Load()); status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{field: map[string]any{"key": fmt.Sprintf("v%d", s.version.Load())}}
		if resolveFlags {
			body["resolved"], body["schemaMatch"] = true, true
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(s.Close)
	return s
}

func loadKey(t *testing.T, load func() (map[string]any, error)) any {
	t.Helper()
	tree, err := load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return tree["key"]
}

func TestRemoteRefresh_OffKeepsTheFirstLoad(t *testing.T) {
	spectest.Proves(t, "cloud/go-runtime-destination", "refresh", "refresh-off-keeps-the-first-load")
	server := newVersionedServer(t, "config")
	source := NewRemoteConfigSource(RemoteSourceConfig{ServerURL: server.URL, AppName: "app", Environment: "prod"})
	clock := newRefreshClock()
	source.loaded.now = clock.Now

	if got := loadKey(t, source.Load); got != "v1" {
		t.Fatalf("first load = %v, want v1", got)
	}
	server.version.Store(2)
	clock.Advance(24 * time.Hour)
	if got := loadKey(t, source.Load); got != "v1" {
		t.Fatalf("load without a refresh period = %v, want the first load v1", got)
	}
	waitRefresh(&source.loaded)
	if calls := server.calls.Load(); calls != 1 {
		t.Fatalf("server calls = %d, want 1", calls)
	}
}

func TestRemoteRefresh_StaleLoadReturnsTheHeldTreeAndRefreshes(t *testing.T) {
	spectest.Proves(t, "cloud/go-runtime-destination", "refresh", "stale-load-returns-the-held-tree-and-refreshes")
	server := newVersionedServer(t, "config")
	source := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL: server.URL, AppName: "app", Environment: "prod", RefreshPeriod: time.Minute,
	})
	clock := newRefreshClock()
	source.loaded.now = clock.Now

	if got := loadKey(t, source.Load); got != "v1" {
		t.Fatalf("first load = %v, want v1", got)
	}
	server.version.Store(2)
	clock.Advance(59 * time.Second)
	if got := loadKey(t, source.Load); got != "v1" {
		t.Fatalf("load inside the period = %v, want v1", got)
	}
	waitRefresh(&source.loaded)
	if calls := server.calls.Load(); calls != 1 {
		t.Fatalf("server calls inside the period = %d, want 1", calls)
	}

	clock.Advance(time.Second)
	if got := loadKey(t, source.Load); got != "v1" {
		t.Fatalf("stale load = %v, want the held tree v1 at once", got)
	}
	waitRefresh(&source.loaded)
	if got := loadKey(t, source.Load); got != "v2" {
		t.Fatalf("load after the refresh = %v, want v2", got)
	}
	waitRefresh(&source.loaded)
	if calls := server.calls.Load(); calls != 2 {
		t.Fatalf("server calls = %d, want 2: the refresh restarts the period", calls)
	}
}

func TestRemoteRefresh_ConcurrentStaleLoadsShareOneFetch(t *testing.T) {
	spectest.Proves(t, "cloud/go-runtime-destination", "refresh", "stale-load-returns-the-held-tree-and-refreshes")
	release := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) > 1 {
			<-release
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"config": map[string]any{"key": "v"}, "resolved": true, "schemaMatch": true})
	}))
	defer server.Close()
	source := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL: server.URL, AppName: "app", Environment: "prod", RefreshPeriod: time.Minute,
	})
	clock := newRefreshClock()
	source.loaded.now = clock.Now
	loadKey(t, source.Load)
	clock.Advance(time.Minute)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := source.Load(); err != nil {
				t.Errorf("stale load: %v", err)
			}
		}()
	}
	wg.Wait()
	close(release)
	waitRefresh(&source.loaded)
	if got := calls.Load(); got != 2 {
		t.Fatalf("server calls = %d, want the boot fetch and one shared refresh", got)
	}
}

func TestRemoteRefresh_FailedRefreshKeepsTheHeldTree(t *testing.T) {
	spectest.Proves(t, "cloud/go-runtime-destination", "refresh", "failed-refresh-keeps-the-held-tree")
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	server := newVersionedServer(t, "config")
	source := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL: server.URL, AppName: "app", Environment: "prod", Required: true,
		RefreshPeriod: time.Minute, SnapshotURI: "gs://snapshots/never-read",
	})
	clock := newRefreshClock()
	source.loaded.now = clock.Now
	loadKey(t, source.Load)

	for _, status := range []int{http.StatusServiceUnavailable, http.StatusForbidden, http.StatusForbidden} {
		server.status.Store(int32(status))
		clock.Advance(time.Minute)
		if got := loadKey(t, source.Load); got != "v1" {
			t.Fatalf("load with a %d refresh = %v, want the held tree v1", status, got)
		}
		waitRefresh(&source.loaded)
		if got := loadKey(t, source.Load); got != "v1" {
			t.Fatalf("load after a %d refresh = %v, want the held tree v1", status, got)
		}
	}
	waitRefresh(&source.loaded)
	if calls := server.calls.Load(); calls != 4 {
		t.Fatalf("server calls = %d, want the boot fetch and one attempt per period", calls)
	}
	if got := strings.Count(logs.String(), "remote source refresh failed"); got != 2 {
		t.Fatalf("refresh warnings = %d, want one per new failure:\n%s", got, logs.String())
	}

	server.status.Store(http.StatusOK)
	server.version.Store(2)
	clock.Advance(time.Minute)
	loadKey(t, source.Load)
	waitRefresh(&source.loaded)
	if got := loadKey(t, source.Load); got != "v2" {
		t.Fatalf("load after recovery = %v, want v2", got)
	}
}

func TestRemoteRefresh_PinnedConfigIsNeverRefreshed(t *testing.T) {
	spectest.Proves(t, "cloud/go-runtime-destination", "refresh", "pinned-config-is-never-refreshed")
	server := newVersionedServer(t, "config")
	source := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL: server.URL, AppName: "app", Environment: "prod",
		Version: "rev-7", Pinned: true, RefreshPeriod: time.Minute,
	})
	clock := newRefreshClock()
	source.loaded.now = clock.Now
	loadKey(t, source.Load)
	server.version.Store(2)
	clock.Advance(time.Hour)
	loadKey(t, source.Load)
	waitRefresh(&source.loaded)
	if got := loadKey(t, source.Load); got != "v1" {
		t.Fatalf("pinned load = %v, want v1", got)
	}
	if calls := server.calls.Load(); calls != 1 {
		t.Fatalf("server calls = %d, want 1", calls)
	}
}

func TestRemoteRefresh_SecretsAndPreparedSourcesRefresh(t *testing.T) {
	spectest.Proves(t, "cloud/go-runtime-destination", "refresh", "secrets-and-prepared-sources-refresh")
	secretsServer := newVersionedServer(t, "secrets")
	secrets := NewRemoteSecretsSource(RemoteSecretsSourceConfig{
		ServerURL: secretsServer.URL, AppName: "app", Environment: "prod", RefreshPeriod: time.Minute,
	})
	bootServer := newVersionedServerWithFlags(t, "config", false)
	boot := NewPreparedBootSource(PreparedBootSourceConfig{
		ServerURL: bootServer.URL, Audience: "https://config.example.test", Reference: testPreparedBootReference,
		Token: "signed-workload-token", Timeout: time.Second, RetryBudget: -1, RefreshPeriod: time.Minute,
	})

	for _, tc := range []struct {
		name   string
		load   func() (map[string]any, error)
		loaded *remoteLoad
		server *versionedServer
	}{
		{"secrets", secrets.Load, &secrets.loaded, secretsServer},
		{"prepared boot", boot.Load, &boot.loaded, bootServer},
	} {
		clock := newRefreshClock()
		tc.loaded.now = clock.Now
		if got := loadKey(t, tc.load); got != "v1" {
			t.Fatalf("%s first load = %v, want v1", tc.name, got)
		}
		tc.server.version.Store(2)
		clock.Advance(time.Minute)
		if got := loadKey(t, tc.load); got != "v1" {
			t.Fatalf("%s stale load = %v, want the held tree v1", tc.name, got)
		}
		waitRefresh(tc.loaded)
		if got := loadKey(t, tc.load); got != "v2" {
			t.Fatalf("%s load after the refresh = %v, want v2", tc.name, got)
		}
	}
}

func TestRemoteRefresh_DiscoveryReadsTheRefreshPeriod(t *testing.T) {
	spectest.Proves(t, "cloud/go-runtime-destination", "refresh", "discovery-reads-the-refresh-period")
	isolateDiscoveryEnv(t, false)
	t.Setenv("CONFIG_SERVER_URL", "https://config.putnami.test")
	t.Setenv("CONFIG_SERVER_TOKEN", "test-token")
	t.Setenv("CONFIG_VERSION", "")

	t.Setenv("CONFIG_SERVER_REFRESH", "")
	if got := DiscoverRemoteSource().loaded.period; got != 0 {
		t.Fatalf("unset refresh period = %v, want 0", got)
	}
	t.Setenv("CONFIG_SERVER_REFRESH", "not-a-duration")
	if got := DiscoverRemoteSource().loaded.period; got != 0 {
		t.Fatalf("invalid refresh period = %v, want 0", got)
	}
	t.Setenv("CONFIG_SERVER_REFRESH", "90s")
	if got := DiscoverRemoteSource().loaded.period; got != 90*time.Second {
		t.Fatalf("config refresh period = %v, want 90s", got)
	}
	if got := DiscoverRemoteSecretsSource().loaded.period; got != 90*time.Second {
		t.Fatalf("secrets refresh period = %v, want 90s", got)
	}
	t.Setenv("CONFIG_VERSION", "rev-7")
	if got := DiscoverRemoteSource().loaded.period; got != 0 {
		t.Fatalf("pinned config refresh period = %v, want 0", got)
	}

	t.Setenv(PreparedBootBindingEnv, testPreparedBootReference)
	t.Setenv("CONFIG_SERVER_AUDIENCE", "https://config.putnami.test")
	if got := DiscoverPreparedBootSource().loaded.period; got != 90*time.Second {
		t.Fatalf("prepared boot refresh period = %v, want 90s", got)
	}
}

func TestRemoteRefresh_AnswerWithoutATreeKeepsTheHeldTree(t *testing.T) {
	spectest.Proves(t, "cloud/go-runtime-destination", "refresh", "failed-refresh-keeps-the-held-tree")
	var empty atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if empty.Load() {
			_, _ = w.Write([]byte(`{"resolved":true,"schemaMatch":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"config":{"key":"v1"},"resolved":true,"schemaMatch":true}`))
	}))
	defer server.Close()
	source := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL: server.URL, AppName: "app", Environment: "prod", RefreshPeriod: time.Minute,
	})
	clock := newRefreshClock()
	source.loaded.now = clock.Now
	loadKey(t, source.Load)
	empty.Store(true)
	clock.Advance(time.Minute)
	loadKey(t, source.Load)
	waitRefresh(&source.loaded)
	if got := loadKey(t, source.Load); got != "v1" {
		t.Fatalf("load after an answer without config = %v, want the held tree v1", got)
	}
}

func TestRemoteRefresh_URLPinnedConfigIsNeverRefreshed(t *testing.T) {
	spectest.Proves(t, "cloud/go-runtime-destination", "refresh", "pinned-config-is-never-refreshed")
	for _, tc := range []struct {
		url    string
		period time.Duration
	}{
		{"https://config.putnami.test/api/configs/resolve?appName=app&version=rev-7&pinned=true", 0},
		{"https://config.putnami.test/api/configs/resolve?appName=app&version=rev-7", time.Minute},
		{"https://config.putnami.test/api/configs/resolve?appName=app", time.Minute},
	} {
		source := NewRemoteConfigSource(RemoteSourceConfig{URL: tc.url, RefreshPeriod: time.Minute})
		if got := source.loaded.period; got != tc.period {
			t.Fatalf("refresh period of %s = %v, want %v", tc.url, got, tc.period)
		}
	}
	if urlPinsVersion("://bad") {
		t.Fatal("an unparseable URL pins no version")
	}
}
