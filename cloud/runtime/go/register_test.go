package runtime

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	config "go.putnami.dev/config"
)

// collectViaRegister drives Register the way the framework registry does:
// each discoverer is invoked and any non-nil source kept. It exercises the
// typed-nil guard in Register (an unset CONFIG_SERVER_URL must yield a nil
// interface, not a non-nil box around a nil pointer).
func collectViaRegister() []config.Source {
	var got []config.Source
	Register(func(discover SourceDiscoverer) {
		if s := discover(); s != nil {
			got = append(got, s)
		}
	})
	return got
}

func TestRegister_DiscoversConfigAndSecrets(t *testing.T) {
	isolateDiscoveryEnv(t, false)
	t.Setenv("CONFIG_SERVER_URL", "https://config.putnami.test")
	t.Setenv("CONFIG_SERVER_TOKEN", "unit-token")
	t.Setenv("APP_NAME", "task-api")

	got := collectViaRegister()
	if len(got) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(got))
	}
	if got[0].Name() != "config-server" || got[1].Name() != "secrets-server" {
		t.Errorf("names = %q, %q; want config-server, secrets-server", got[0].Name(), got[1].Name())
	}
	if got[0].Priority() != 50 || got[1].Priority() != 55 {
		t.Errorf("priorities = %d, %d; want 50, 55", got[0].Priority(), got[1].Priority())
	}
}

func TestRegister_OnlyConfigForExactResolveURL(t *testing.T) {
	// An exact /api/configs/resolve URL already merges secrets server-side, so
	// the secrets discoverer must contribute nothing.
	isolateDiscoveryEnv(t, false)
	t.Setenv("CONFIG_SERVER_URL", "https://config.example.test/api/configs/resolve?appName=shop%2Fserver&environment=prod&secretsMode=reveal")
	t.Setenv("CONFIG_SERVER_TOKEN", "unit-token")
	t.Setenv("APP_NAME", "shop/server")

	got := collectViaRegister()
	if len(got) != 1 || got[0].Name() != "config-server" {
		t.Fatalf("expected only config-server, got %#v", got)
	}
}

func TestRegister_EmptyWithoutURL(t *testing.T) {
	isolateDiscoveryEnv(t, false)
	t.Setenv("CONFIG_SERVER_URL", "")

	if got := collectViaRegister(); len(got) != 0 {
		t.Fatalf("expected no sources without CONFIG_SERVER_URL, got %d", len(got))
	}
}

func TestRegister_SharedRemoteSourcePaysOneRetryBudgetBeforeSnapshot(t *testing.T) {
	isolateDiscoveryEnv(t, false)
	plaintext, _ := json.Marshal(map[string]any{"oci": map[string]any{"server": map[string]any{"port": 8084}}})
	stub := newSnapshotStub(t, []byte(base64.StdEncoding.EncodeToString(plaintext)))
	defer stub.restore()

	t.Setenv("CONFIG_SERVER_URL", "http://127.0.0.1:1/api/configs/resolve?appName=distribution%2Fworkloads%2Foci-server&environment=prod&secretsMode=reveal")
	t.Setenv("CONFIG_SERVER_REQUIRED", "true")
	// The remote endpoint refuses the connection immediately, while the snapshot
	// fallback makes two local HTTP round trips (GCS and KMS). Give the latter
	// ordinary CI scheduling headroom; the retry-budget assertion below measures
	// only subsequent cached loads.
	t.Setenv("CONFIG_SERVER_TIMEOUT", "1s")
	t.Setenv("CONFIG_SERVER_RETRY_BUDGET", "100ms")
	t.Setenv("CONFIG_SNAPSHOT_URI", "gs://snapshots/config-snapshots/ws/oci/rev.json.enc")
	t.Setenv("CONFIG_SNAPSHOT_KMS_KEY", "projects/p/locations/l/keyRings/putnami/cryptoKeys/ws")
	t.Setenv("APP_NAME", "distribution/workloads/oci-server")

	var discoverers []SourceDiscoverer
	Register(func(discover SourceDiscoverer) { discoverers = append(discoverers, discover) })
	if len(discoverers) != 3 {
		t.Fatalf("discoverers = %d, want config + secrets + prepared boot", len(discoverers))
	}

	first := discoverers[0]()
	if first == nil {
		t.Fatal("remote config source was not discovered")
	}
	got, err := first.Load()
	if err != nil {
		t.Fatalf("initial schema load from snapshot: %v", err)
	}
	if got["oci"] == nil {
		t.Fatalf("initial schema load did not receive snapshot tree: %#v", got)
	}

	// The first load performs the deliberate remote failure and the two snapshot
	// HTTP calls. Time only the schema loads that must reuse its cached tree.
	start := time.Now()
	for i := 0; i < 9; i++ {
		source := discoverers[0]()
		if source != first {
			t.Fatalf("discovery %d returned a distinct source; retry budgets would multiply", i+1)
		}
		got, err := source.Load()
		if err != nil {
			t.Fatalf("schema load %d from snapshot: %v", i+1, err)
		}
		if got["oci"] == nil {
			t.Fatalf("schema load %d did not receive snapshot tree: %#v", i+1, got)
		}
	}
	elapsed := time.Since(start)
	// Nine independent sources would consume roughly 900ms. One shared source
	// spends the 100ms budget once, then all loaders reuse its snapshot tree.
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("nine schema loads took %s; want one shared retry budget before snapshot", elapsed)
	}
	if defaultRemoteSourceRetryBudget+2*defaultRemoteSourceTimeout >= 4*time.Minute {
		t.Fatalf("default remote + snapshot bound no longer fits Cloud Run startup: retry=%s timeout=%s", defaultRemoteSourceRetryBudget, defaultRemoteSourceTimeout)
	}
}
