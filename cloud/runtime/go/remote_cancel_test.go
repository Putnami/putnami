package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cloudkms "google.golang.org/api/cloudkms/v1"
	storagev1 "google.golang.org/api/storage/v1"

	config "go.putnami.dev/config"
)

// cancelTestSources builds each remote source against serverURL with a retry
// budget far longer than the test waits, so only cancellation can end a load.
// The prepared boot source is always required; the others follow required.
func cancelTestSources(serverURL string, required bool) map[string]config.ContextSource {
	return map[string]config.ContextSource{
		"config": NewRemoteConfigSource(RemoteSourceConfig{
			ServerURL: serverURL, AppName: "test-app", Environment: "prod",
			Timeout: time.Second, RetryBudget: time.Minute, Required: required,
		}),
		"secrets": NewRemoteSecretsSource(RemoteSecretsSourceConfig{
			ServerURL: serverURL, AppName: "test-app", Environment: "prod",
			Timeout: time.Second, RetryBudget: time.Minute, Required: required,
		}),
		"prepared-boot": NewPreparedBootSource(PreparedBootSourceConfig{
			ServerURL: serverURL, Audience: "https://config.example.test", Reference: testPreparedBootReference,
			Token: "signed-workload-token", Timeout: time.Second, RetryBudget: time.Minute,
		}),
	}
}

// After 4 failed attempts the next retry wait is 800ms. Canceling inside that
// wait must return well before it would have elapsed.
func TestRemoteSourcesStopRetryWaitWhenCanceled(t *testing.T) {
	for name := range cancelTestSources("", true) {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				http.Error(w, "warming up", http.StatusServiceUnavailable)
			}))
			defer server.Close()
			source := cancelTestSources(server.URL, true)[name]

			ctx, cancel := context.WithCancel(context.Background())
			canceledAt := make(chan time.Time, 1)
			go func() {
				for calls.Load() < 4 {
					time.Sleep(time.Millisecond)
				}
				time.Sleep(50 * time.Millisecond)
				canceledAt <- time.Now()
				cancel()
			}()
			data, err := loadWithin(t, ctx, source, 5*time.Second)
			if data != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("data=%#v err=%v, want context.Canceled", data, err)
			}
			if waited := time.Since(<-canceledAt); waited > 400*time.Millisecond {
				t.Fatalf("returned %s after cancellation, want the retry wait interrupted", waited)
			}
			if got := calls.Load(); got != 4 {
				t.Fatalf("server calls=%d, want no attempt after cancellation", got)
			}
		})
	}
}

func TestWaitRemoteRetryReturnsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitRemoteRetry(ctx, time.Hour) {
		t.Fatal("waitRemoteRetry reported a completed wait on a canceled context")
	}
	if !waitRemoteRetry(context.Background(), time.Millisecond) {
		t.Fatal("waitRemoteRetry reported cancellation on a live context")
	}
}

func TestRemoteSourcesAbortInFlightRequestWhenCanceled(t *testing.T) {
	for name := range cancelTestSources("", true) {
		t.Run(name, func(t *testing.T) {
			started := make(chan struct{}, 1)
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				started <- struct{}{}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer server.Close()
			defer close(release)
			source := cancelTestSources(server.URL, true)[name]

			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				<-started
				cancel()
			}()
			if _, err := loadWithin(t, ctx, source, 500*time.Millisecond); !errors.Is(err, context.Canceled) {
				t.Fatalf("err=%v, want context.Canceled", err)
			}
		})
	}
}

// A load canceled mid-fetch has no configuration at all. An optional source
// must return the cancellation instead of reporting empty local config, and
// no source may cache it: the next load fetches again and caches that result.
func TestRemoteSourcesRefetchAfterLoadCanceledMidFetch(t *testing.T) {
	for _, required := range []bool{true, false} {
		for name := range cancelTestSources("", required) {
			if name == "prepared-boot" && !required {
				continue
			}
			t.Run(fmt.Sprintf("%s/required=%t", name, required), func(t *testing.T) {
				var calls atomic.Int32
				started := make(chan struct{}, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if calls.Add(1) == 1 {
						// The server notices the client leaving only once the body is read.
						_, _ = io.Copy(io.Discard, r.Body)
						started <- struct{}{}
						<-r.Context().Done()
						return
					}
					body := map[string]any{"config": map[string]any{"ready": true}}
					if r.URL.Path != preparedBootPath {
						body["secrets"], body["resolved"], body["schemaMatch"] = map[string]any{"ready": true}, true, true
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(body)
				}))
				defer server.Close()
				source := cancelTestSources(server.URL, required)[name]

				ctx, cancel := context.WithCancel(context.Background())
				go func() {
					<-started
					cancel()
				}()
				if data, err := loadWithin(t, ctx, source, 500*time.Millisecond); data != nil || !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled load data=%#v err=%v, want context.Canceled", data, err)
				}
				for range 2 {
					data, err := source.Load()
					if err != nil || data["ready"] != true {
						t.Fatalf("data=%#v err=%v", data, err)
					}
				}
				if got := calls.Load(); got != 2 {
					t.Fatalf("server calls=%d, want the canceled fetch plus one cached fetch", got)
				}
			})
		}
	}
}

func TestRemoteConfigSourceSkipsSnapshotFallbackWhenCanceled(t *testing.T) {
	var snapshotCalls atomic.Int32
	prevStorage, prevKMS := newSnapshotStorageService, newSnapshotKMSService
	newSnapshotStorageService = func(context.Context) (*storagev1.Service, error) {
		snapshotCalls.Add(1)
		return nil, errors.New("storage disabled")
	}
	newSnapshotKMSService = func(context.Context) (*cloudkms.Service, error) {
		snapshotCalls.Add(1)
		return nil, errors.New("kms disabled")
	}
	t.Cleanup(func() { newSnapshotStorageService, newSnapshotKMSService = prevStorage, prevKMS })

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	source := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL: server.URL, AppName: "test-app", Environment: "prod",
		Timeout: time.Second, RetryBudget: time.Minute, Required: true,
		SnapshotURI: "gs://bucket/object", SnapshotKMSKey: "projects/p/locations/l/keyRings/r/cryptoKeys/k",
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for calls.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	if _, err := loadWithin(t, ctx, source, 2*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if snapshotCalls.Load() != 0 {
		t.Fatalf("snapshot fallback ran %d times after cancellation", snapshotCalls.Load())
	}
}

// Canceling during the snapshot read is a shutdown, not a snapshot outage:
// the fallback-failure WARN that alerts key on must stay quiet.
func TestRemoteConfigSourceCanceledSnapshotReadLogsNoOutage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	prevStorage := newSnapshotStorageService
	newSnapshotStorageService = func(context.Context) (*storagev1.Service, error) {
		cancel()
		return nil, context.Canceled
	}
	t.Cleanup(func() { newSnapshotStorageService = prevStorage })
	var logs bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	source := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL: server.URL, AppName: "test-app", Environment: "prod",
		Timeout: time.Second, RetryBudget: -1, Required: true,
		SnapshotURI: "gs://bucket/object", SnapshotKMSKey: "projects/p/locations/l/keyRings/r/cryptoKeys/k",
	})
	if _, err := source.LoadContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if strings.Contains(logs.String(), "snapshot fallback failed") {
		t.Fatalf("canceled snapshot read logged an outage: %s", logs.String())
	}
}

func TestConfigLoadContextCancelsRemoteSource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "warming up", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	source := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL: server.URL, AppName: "test-app", Environment: "prod",
		Timeout: time.Second, RetryBudget: time.Minute, Required: true,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := config.LoadContext(ctx, config.Config[struct{}]("app"), source)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("config.LoadContext returned after %s, want the 200ms deadline", elapsed)
	}
}

func TestRemoteLoadWaiterHonorsItsOwnCancellation(t *testing.T) {
	var load remoteLoad
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := load.load(context.Background(), func(context.Context) (map[string]any, error) {
			close(started)
			<-release
			return map[string]any{"ready": true}, nil
		})
		done <- err
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := load.load(ctx, func(context.Context) (map[string]any, error) {
		t.Fatal("a waiter must not start a second fetch")
		return nil, nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter err=%v, want context.Canceled", err)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	data, err := load.load(context.Background(), func(context.Context) (map[string]any, error) {
		t.Fatal("a completed load must be reused")
		return nil, nil
	})
	if err != nil || data["ready"] != true {
		t.Fatalf("data=%#v err=%v", data, err)
	}
}

func loadWithin(t *testing.T, ctx context.Context, source config.ContextSource, limit time.Duration) (map[string]any, error) {
	t.Helper()
	type result struct {
		data map[string]any
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := source.LoadContext(ctx)
		done <- result{data, err}
	}()
	select {
	case r := <-done:
		return r.data, r.err
	case <-time.After(limit):
		t.Fatalf("%s LoadContext did not return within %s of cancellation", source.Name(), limit)
		return nil, nil
	}
}
