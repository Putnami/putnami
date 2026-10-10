package configcli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
)

// writeFreshWorkspaceAuth writes an auth.json whose access token is fresh and
// workspace-scoped, so WorkspaceAuth resolves it without any network refresh.
func writeFreshWorkspaceAuth(t *testing.T, home, workspaceID string) {
	t.Helper()
	file := filepath.Join(home, clicore.AuthFileRelative)
	mustMkdir(t, filepath.Dir(file))
	writeJSONFile(t, file, map[string]any{
		"access_token":  jwt(map[string]any{"sub": "user-1", "scope_ref": map[string]any{"workspace_id": workspaceID}}),
		"refresh_token": "refresh-token",
		"token_type":    "Bearer",
		"expires_at":    "2026-12-31T00:00:00.000Z",
		"issuer":        testBaseURL,
		"client_id":     "putnami-cli",
	})
}

func TestFetchMetadataIDTokenFrom(t *testing.T) {
	var gotAudience, gotFlavor, gotFormat string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAudience = r.URL.Query().Get("audience")
		gotFormat = r.URL.Query().Get("format")
		gotFlavor = r.Header.Get("Metadata-Flavor")
		_, _ = w.Write([]byte("  minted-id-token\n"))
	}))
	defer srv.Close()

	tok, err := fetchMetadataIDTokenFrom(context.Background(), "https://api.putnami.cloud", srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("fetchMetadataIDTokenFrom: %v", err)
	}
	if tok != "minted-id-token" {
		t.Errorf("token = %q, want trimmed minted-id-token", tok)
	}
	if gotAudience != "https://api.putnami.cloud" {
		t.Errorf("audience query = %q", gotAudience)
	}
	if gotFormat != "full" {
		t.Errorf("format query = %q, want full", gotFormat)
	}
	if gotFlavor != "Google" {
		t.Errorf("Metadata-Flavor = %q, want Google", gotFlavor)
	}
}

func TestFetchMetadataIDTokenFrom_NonOKFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	if _, err := fetchMetadataIDTokenFrom(context.Background(), "aud", srv.URL, srv.Client()); err == nil {
		t.Fatal("non-200 metadata response must error (no anonymous fallthrough)")
	}
}

func TestDriftConfigAudience(t *testing.T) {
	cases := []struct {
		name         string
		params       map[string]any
		env          map[string]string
		controlPlane string
		want         string
	}{
		{"param wins", map[string]any{"config-audience": "https://param/"}, map[string]string{driftConfigAudienceEnv: "https://env"}, "https://cp", "https://param"},
		{"env next", map[string]any{}, map[string]string{driftConfigAudienceEnv: "https://env/"}, "https://cp", "https://env"},
		{"control plane fallback", map[string]any{}, map[string]string{}, "https://cp/", "https://cp"},
		{"empty", map[string]any{}, map[string]string{}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := driftConfigAudience(tc.params, tc.env, tc.controlPlane); got != tc.want {
				t.Errorf("driftConfigAudience = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDriftHasSession(t *testing.T) {
	home := t.TempDir()
	env := hometest.Env(home, nil)
	if driftHasSession(env) {
		t.Fatal("no auth.json → no session")
	}
	writeTestAuth(t, home)
	if !driftHasSession(env) {
		t.Fatal("auth.json present → session")
	}
}

func TestNewConfigDriftCtx_CloudTokenAndControlPlaneOverride(t *testing.T) {
	root := t.TempDir()
	writeLinkFile(t, root)
	env := hometest.Env(t.TempDir(), map[string]string{driftCloudTokenEnv: "explicit-bearer"})

	ctx, err := newConfigDriftCtx(map[string]any{
		"app": "my-app", "control-plane-url": "https://trusted-ci.example.test/",
	}, root, env, driftTestIO())
	if err != nil {
		t.Fatalf("newConfigDriftCtx: %v", err)
	}
	if ctx.authToken.Authorization() != "Bearer explicit-bearer" {
		t.Errorf("authToken = %q, want the explicit override", ctx.authToken.Authorization())
	}
	if ctx.workspaceID != "ws-acme" || ctx.app != "my-app" || ctx.environment != "prod" {
		t.Errorf("ctx = %+v", ctx)
	}
	if ctx.controlPlane != "https://trusted-ci.example.test" {
		t.Errorf("controlPlane = %q, want explicit trusted CI origin", ctx.controlPlane)
	}
}

func TestNewConfigDriftCtx_SessionPath(t *testing.T) {
	root := t.TempDir()
	writeLinkFile(t, root)
	home := t.TempDir()
	writeFreshWorkspaceAuth(t, home, "ws-acme")
	ioctx, _, _ := commonIO(t, home, http.DefaultClient)

	ctx, err := newConfigDriftCtx(map[string]any{
		"app": "my-app", "control-plane-url": "https://session-origin.example.test/",
	}, root, ioctx.Env, ioctx)
	if err != nil {
		t.Fatalf("session-path drift ctx: %v", err)
	}
	if ctx.authToken.Empty() {
		t.Error("session path must carry the session bearer")
	}
	if ctx.workspaceID != "ws-acme" {
		t.Errorf("workspace = %q, want ws-acme", ctx.workspaceID)
	}
	if ctx.controlPlane != "https://session-origin.example.test" {
		t.Errorf("session control plane = %q, want explicit origin", ctx.controlPlane)
	}
}

func TestNewConfigDriftCtx_MetadataServiceAccount(t *testing.T) {
	root := t.TempDir()
	writeLinkFile(t, root)
	env := hometest.Env(t.TempDir(), nil) // no session, no override

	var gotAudience string
	restore := metadataIDToken
	metadataIDToken = func(_ context.Context, audience string) (string, error) {
		gotAudience = audience
		return "sa-id-token", nil
	}
	defer func() { metadataIDToken = restore }()

	ctx, err := newConfigDriftCtx(map[string]any{
		"app": "my-app", "control-plane-url": "https://metadata-origin.example.test/",
	}, root, env, driftTestIO())
	if err != nil {
		t.Fatalf("metadata-SA drift ctx: %v", err)
	}
	if gotAudience != "https://metadata-origin.example.test" || ctx.controlPlane != gotAudience {
		t.Errorf("minted audience %q and control plane %q must match the explicit origin", gotAudience, ctx.controlPlane)
	}
	if ctx.authToken.Authorization() != "Bearer sa-id-token" {
		t.Errorf("authToken = %q, want the minted SA token", ctx.authToken.Authorization())
	}
	if ctx.workspaceID != "ws-acme" || ctx.app != "my-app" || ctx.environment != "prod" {
		t.Errorf("ctx = %+v", ctx)
	}
}

func TestNewConfigDriftCtx_MetadataUnavailableFailsClosed(t *testing.T) {
	root := t.TempDir()
	writeLinkFile(t, root)
	env := hometest.Env(t.TempDir(), nil)

	restore := metadataIDToken
	metadataIDToken = func(context.Context, string) (string, error) {
		return "", errors.New("metadata server unreachable")
	}
	defer func() { metadataIDToken = restore }()

	if _, err := newConfigDriftCtx(map[string]any{"app": "my-app"}, root, env, driftTestIO()); err == nil {
		t.Fatal("an unavailable metadata source must fail closed, not resolve anonymously")
	} else {
		assertContains(t, err.Error(), "could not mint a service-account id token")
	}
}

func TestNewConfigDriftCtx_EmptyMintedTokenFailsClosed(t *testing.T) {
	root := t.TempDir()
	writeLinkFile(t, root)
	env := hometest.Env(t.TempDir(), nil)

	restore := metadataIDToken
	metadataIDToken = func(context.Context, string) (string, error) { return "   ", nil }
	defer func() { metadataIDToken = restore }()

	if _, err := newConfigDriftCtx(map[string]any{"app": "my-app"}, root, env, driftTestIO()); err == nil {
		t.Fatal("an empty minted token must fail closed")
	}
}

func TestComputeConfigDrift_ManagedOverlayIgnoresOnlyProvisionedPlaceholders(t *testing.T) {
	blocks := []schemaBlock{{
		Path: "events",
		Fields: []schemaField{
			{Name: "delivery", Type: "string"},
			{Name: "push", Type: "object", Fields: []schemaField{
				{Name: "issuer", Type: "string"},
				{Name: "audience", Type: "string"},
				{Name: "allowedServiceAccounts", Type: "array", Items: []schemaField{{Type: "string"}}},
			}},
		},
	}}
	committed := map[string]any{"events": map[string]any{
		"delivery": "push",
		"push": map[string]any{
			"issuer":                 "https://accounts.google.com",
			"audience":               "",
			"allowedServiceAccounts": []any{},
		},
	}}
	published := map[string]any{"events": map[string]any{
		"delivery": "push",
		"push": map[string]any{
			"issuer":                 "https://accounts.google.com",
			"audience":               "https://service.run.app",
			"allowedServiceAccounts": []any{"runtime@example.test"},
		},
	}}
	managed := managedOverlayPaths([]string{"apps/auth-server/prod", "managed/events"})

	if entries, _ := computeConfigDrift(blocks, committed, published, managed); len(entries) != 0 {
		t.Fatalf("managed placeholder entries = %+v, want none", entries)
	}

	// A non-empty operator-authored value under the same managed root remains a
	// real drift. The layer exemption is deliberately narrower than the root.
	published["events"].(map[string]any)["delivery"] = "pull"
	entries, _ := computeConfigDrift(blocks, committed, published, managed)
	if len(entries) != 1 || entries[0].Key != "events.delivery" || entries[0].Kind != driftValueMismatch {
		t.Fatalf("authored entries = %+v, want events.delivery value-mismatch", entries)
	}
}

func TestComputeConfigDrift_ManagedOverlayReportsPreservedOperatorKeys(t *testing.T) {
	tests := []struct {
		name      string
		block     schemaBlock
		published map[string]any
		layer     string
		wantKey   string
	}{
		{
			name: "database",
			block: schemaBlock{Path: "database", Fields: []schemaField{
				{Name: "poolSize", Type: "int"},
				{Name: "protocolVersion", Type: "int"},
				{Name: "databases", Type: "map"},
			}},
			published: map[string]any{"database": map[string]any{
				"poolSize":        float64(20),
				"protocolVersion": float64(1),
				"databases": map[string]any{
					"primary": map[string]any{"engine": "postgres"},
				},
			}},
			layer:   "managed/database",
			wantKey: "database.poolSize",
		},
		{
			name: "storage",
			block: schemaBlock{Path: "storage", Fields: []schemaField{
				{Name: "cacheTTL", Type: "duration"},
				{Name: "protocolVersion", Type: "int"},
				{Name: "bindings", Type: "array"},
			}},
			published: map[string]any{"storage": map[string]any{
				"cacheTTL":        "15m",
				"protocolVersion": float64(1),
				"bindings": []any{
					map[string]any{"name": "uploads", "backend": "gcs", "bucket": "uploads-prod"},
				},
			}},
			layer:   "managed/storage",
			wantKey: "storage.cacheTTL",
		},
		{
			name: "events nested operator key",
			block: schemaBlock{Path: "events", Fields: []schemaField{
				{Name: "push", Type: "object", Fields: []schemaField{
					{Name: "audience", Type: "string"},
					{Name: "jwksUrl", Type: "string"},
				}},
			}},
			published: map[string]any{"events": map[string]any{
				"push": map[string]any{
					"audience": "https://service.run.app",
					"jwksUrl":  "https://operator.example/jwks.json",
				},
			}},
			layer:   "managed/events",
			wantKey: "events.push.jwksUrl",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries, _ := computeConfigDrift(
				[]schemaBlock{tt.block},
				map[string]any{},
				tt.published,
				managedOverlayPaths([]string{tt.layer}),
			)
			if len(entries) != 1 || entries[0].Key != tt.wantKey || entries[0].Kind != driftPublishedNotCommitted {
				t.Fatalf("entries = %+v, want only %s published-not-committed", entries, tt.wantKey)
			}
		})
	}
}

func TestConfigProjectionFingerprint_IsStableAndExcludesSensitiveFields(t *testing.T) {
	blocks := []schemaBlock{{
		Path: "service",
		Fields: []schemaField{
			{Name: "endpoint", Type: "string"},
			{Name: "token", Type: "string", Sensitive: true},
		},
	}}
	first := map[string]any{"service": map[string]any{
		"endpoint": "https://api.example.test", "token": "secret-one",
	}}
	second := map[string]any{"service": map[string]any{
		"token": "secret-two", "endpoint": "https://api.example.test",
	}}
	if got, want := configProjectionFingerprint(blocks, first), configProjectionFingerprint(blocks, second); got == "" || got != want {
		t.Fatalf("credential-free fingerprints differ: first=%q second=%q", got, want)
	}
	second["service"].(map[string]any)["endpoint"] = "https://api.changed.test"
	if got, want := configProjectionFingerprint(blocks, first), configProjectionFingerprint(blocks, second); got == want {
		t.Fatalf("public config change did not change fingerprint: %q", got)
	}
}

func TestManagedOverlayPaths_UnknownLayerHasNoExemption(t *testing.T) {
	if paths := managedOverlayPaths([]string{"managed/future"}); len(paths) != 0 {
		t.Fatalf("unknown managed layer paths = %v, want none", paths)
	}
	// A qualified dimension fails closed the same way, even when its base is a
	// declared overlay: only the exact dimension grants an exemption.
	for _, layer := range []string{"managed/future/active/x", "managed/events/active/x"} {
		if paths := managedOverlayPaths([]string{layer}); len(paths) != 0 {
			t.Fatalf("qualified managed layer %s paths = %v, want none", layer, paths)
		}
	}
	// Non-managed resolution layers never grant an exemption either.
	if paths := managedOverlayPaths([]string{"apps/api/prod", "managed"}); len(paths) != 0 {
		t.Fatalf("non-managed layer paths = %v, want none", paths)
	}
}

// eventsDriftSchemaBlock mirrors the api workload's published "events" schema
// block (its schema/config.json), so the fixtures below
// project exactly what the live drift check projects.
func eventsDriftSchemaBlock() schemaBlock {
	return schemaBlock{Path: "events", Fields: []schemaField{
		{Name: "delivery", Type: "string"},
		{Name: "push", Type: "object", Fields: []schemaField{
			{Name: "enabled", Type: "bool"},
			{Name: "issuer", Type: "string"},
			{Name: "audience", Type: "string"},
			{Name: "jwksUrl", Type: "string"},
			{Name: "allowedServiceAccounts", Type: "array", Items: []schemaField{{Type: "string"}}},
			{Name: "allowInsecure", Type: "bool"},
		}},
		{Name: "transport", Type: "string"},
		{Name: "pubsub", Type: "object", Fields: []schemaField{
			{Name: "projectId", Type: "string"},
			{Name: "topicTemplate", Type: "string"},
		}},
		{Name: "eventServer", Type: "object", Fields: []schemaField{
			{Name: "contractVersion", Type: "int"},
			{Name: "endpoint", Type: "string"},
			{Name: "audience", Type: "string"},
			{Name: "protocol", Type: "string"},
			{Name: "workspaceId", Type: "string"},
			{Name: "environment", Type: "string"},
			{Name: "workload", Type: "string"},
			{Name: "topologyGenerationId", Type: "string"},
		}},
	}}
}

// eventsCommittedProd is the api workload's conf/env.prod.yaml authored
// events block: the pre-activation direct Pub/Sub selection plus the empty push
// block the bootstrap gate requires.
func eventsCommittedProd() map[string]any {
	return map[string]any{"events": map[string]any{
		"delivery":  "push",
		"transport": "pubsub",
		"pubsub": map[string]any{
			"projectId":     "putnami",
			"topicTemplate": "events-{topic}",
		},
		"push": map[string]any{},
	}}
}

// TestComputeConfigDrift_ManagedEventsStaysAdditive proves the events overlay
// only fills UNSET keys, so a committed non-placeholder events.transport is
// effective under the managed/events layer and a mismatch there is genuine
// drift.
func TestComputeConfigDrift_ManagedEventsStaysAdditive(t *testing.T) {
	published := map[string]any{"events": map[string]any{
		"delivery":  "push",
		"transport": "eventserver",
		"pubsub": map[string]any{
			"projectId":     "putnami",
			"topicTemplate": "events-{topic}",
		},
	}}

	entries, suppressed := computeConfigDrift(
		[]schemaBlock{eventsDriftSchemaBlock()},
		eventsCommittedProd(),
		published,
		managedOverlayPaths([]string{"managed/events"}),
	)
	if len(entries) != 1 || entries[0].Key != "events.transport" || entries[0].Kind != driftValueMismatch {
		t.Fatalf("entries = %+v, want the events.transport value-mismatch under the additive layer", entries)
	}
	if entries[0].Committed != "pubsub" || entries[0].Published != "eventserver" {
		t.Errorf("entry values = %+v, want committed=pubsub published=eventserver", entries[0])
	}
	// The additive layer still owns what it provisions: the push binding paths
	// the committed config leaves unset.
	if len(suppressed) != 0 {
		t.Fatalf("suppressed = %+v, want none (nothing published under the owned push paths)", suppressed)
	}
}

func TestManagedOverlaySuppresses_AdditiveReasons(t *testing.T) {
	managed := managedOverlayPaths([]string{"managed/events"})
	cases := []struct {
		name        string
		key         string
		committed   any
		committedOK bool
		publishedOK bool
		wantReason  string
		wantOwned   bool
	}{
		{"unset committed", "events.push.audience", nil, false, true, driftReasonUnset, true},
		{"placeholder committed", "events.push.audience", "", true, true, driftReasonPlaceholder, true},
		{"authored committed", "events.transport", "pubsub", true, true, "", false},
		{"committed but unpublished", "events.push.audience", "https://x", true, false, "", false},
		{"unowned key", "events.delivery", nil, false, true, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, reason, ok := managedOverlaySuppresses(tc.key, tc.committed, tc.committedOK, tc.publishedOK, managed)
			if ok != tc.wantOwned || reason != tc.wantReason {
				t.Fatalf("suppresses = (%+v, %q, %v), want (%q, %v)", owner, reason, ok, tc.wantReason, tc.wantOwned)
			}
			if ok && owner.Layer != "managed/events" {
				t.Errorf("owner layer = %q, want managed/events", owner.Layer)
			}
		})
	}
}

func TestDriftCleanLine_ReportsSuppressedCount(t *testing.T) {
	ctx := &secretsCtx{app: "apps/api", environment: "prod"}
	if got := driftCleanLine(ctx, 2, 0); got != "No config drift for apps/api/prod (2 block(s) checked)." {
		t.Errorf("clean line without suppressions = %q", got)
	}
	got := driftCleanLine(ctx, 2, 15)
	if got != "No config drift for apps/api/prod (2 block(s) checked, 15 plane-owned key(s) suppressed)." {
		t.Errorf("clean line with suppressions = %q", got)
	}
}

func TestDriftReportLines_AppendsSuppressedAudit(t *testing.T) {
	ctx := &secretsCtx{app: "apps/api", environment: "prod"}
	lines := driftReportLines(ctx,
		[]DriftEntry{{Block: "session", Key: "session.ttl", Kind: driftValueMismatch, Committed: "24h", Published: "1h"}},
		[]suppressedEntry{{
			Block: "events", Key: "events.push.audience", Kind: driftPublishedNotCommitted,
			Layer: "managed/events", Reason: driftReasonUnset,
		}},
	)
	joined := strings.Join(lines, "\n")
	assertContains(t, joined, "session.ttl differs: committed=24h published=1h")
	assertContains(t, joined, "Reconcile with `putnami cloud config publish")
	assertContains(t, joined, "events.push.audience [published-not-committed] owned by layer managed/events: "+driftReasonUnset)
	if driftSuppressedLines(nil) != nil {
		t.Error("no suppressions must render no audit section")
	}
}

func TestConfigDriftFingerprints_FollowComparableDriftScope(t *testing.T) {
	blocks := []schemaBlock{{Path: "service", Fields: []schemaField{
		{Name: "parallelism", Type: "int"},
		{Name: "secret", Type: "string", Sensitive: true},
	}}}
	committed := map[string]any{"service": map[string]any{
		"parallelism": 64,
		"secret":      "never-hash-this",
	}}
	published := map[string]any{"service": map[string]any{
		// JSON decode represents this as float64, but it is the same schema
		// value as committed's YAML integer and must fingerprint identically.
		"parallelism": float64(64),
		"secret":      "also-never-hash-this",
	}}

	clean, err := configDriftFingerprints(blocks, committed, published, nil)
	if err != nil {
		t.Fatalf("clean fingerprints: %v", err)
	}
	if clean.Committed == "" || clean.Committed != clean.Published {
		t.Fatalf("clean fingerprints = %+v, want equal non-empty values", clean)
	}

	published["service"].(map[string]any)["parallelism"] = float64(16)
	drifted, err := configDriftFingerprints(blocks, committed, published, nil)
	if err != nil {
		t.Fatalf("drifted fingerprints: %v", err)
	}
	if drifted.Committed == drifted.Published {
		t.Fatalf("drifted fingerprints = %+v, want different values", drifted)
	}
}

func TestConfigDriftFingerprints_ExcludeManagedOverlayDifferences(t *testing.T) {
	blocks := []schemaBlock{eventsDriftSchemaBlock()}
	published := eventsCommittedProd()
	published["events"].(map[string]any)["push"] = map[string]any{
		"issuer":   "https://accounts.google.com",
		"audience": "https://api.putnami.cloud",
	}
	managed := managedOverlayPaths([]string{"managed/events"})
	fingerprints, err := configDriftFingerprints(blocks, eventsCommittedProd(), published, managed)
	if err != nil {
		t.Fatalf("managed fingerprints: %v", err)
	}
	if fingerprints.Committed == "" || fingerprints.Committed != fingerprints.Published {
		t.Fatalf("managed fingerprints = %+v, want equal values for suppressed managed differences", fingerprints)
	}
}

func driftTestIO() clicore.IO {
	return clicore.IO{
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: http.DefaultClient,
	}
}
