package mapgen

import (
	"strings"
	"testing"
)

// TestSplitWords_KebabSnakeCamelAndAcronyms pins the tokenizer every alias and
// every hint is built on: get this wrong and `tokenURL` or `controlPlaneAPI`
// silently stops matching, which would show up only as a missing edge.
func TestSplitWords_KebabSnakeCamelAndAcronyms(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"auth-server", []string{"auth", "server"}},
		{"jwks_url", []string{"jwks", "url"}},
		{"configServerUrl", []string{"config", "server", "url"}},
		{"tokenURL", []string{"token", "url"}},
		{"controlPlaneAPI", []string{"control", "plane", "api"}},
		{"APIServer", []string{"api", "server"}},
		{"otlpEndpoint", []string{"otlp", "endpoint"}},
		{"db-gateway", []string{"db", "gateway"}},
		{"v2Api", []string{"v2", "api"}},
		{"@putnami/cache-server", []string{"putnami", "cache", "server"}},
		{"", nil},
		{"---", nil},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := splitWords(tc.in)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("splitWords(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestIntercallHints_OnlyURLShapedKeysParticipate pins which config keys can
// name a callee at all, and in what order their candidates are tried: the leaf
// stripped of its address words first (most specific), then each ancestor
// segment from the innermost outwards.
func TestIntercallHints_OnlyURLShapedKeysParticipate(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want []string
	}{
		{"not url shaped", "server.port", nil},
		{"leaf named url", "cacheServer.auth.introspection.url", []string{"introspection", "auth", "cacheserver"}},
		{"jwks leaf contributes nothing itself", "collector.auth.jwksUrl", []string{"auth", "collector"}},
		{"bare jwks leaf is url shaped", "dbGateway.auth.putnami.jwks", []string{"putnami", "auth", "dbgateway"}},
		{"qualified leaf survives stripping", "controlPlaneAPI.cloud.configServerUrl",
			[]string{"configserver", "cloud", "controlplaneapi"}},
		{"base is stripped too", "controlPlaneAPI.delivery.ci.ingestBaseUrl",
			[]string{"ingest", "ci", "delivery", "controlplaneapi"}},
		{"endpoint leaf", "eventServer.metrics.otlpEndpoint", []string{"otlp", "metrics", "eventserver"}},
		{"single segment with nothing left", "jwksUrl", nil},
		{"host leaf", "cache.redisHost", []string{"redis", "cache"}},
		{"origin leaf", "web.consoleOrigin", []string{"console", "web"}},
		{"addr leaf", "app.metricsAddr", []string{"metrics", "app"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := intercallHints(tc.key)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("intercallHints(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

// intercallFixture is the reduced entry set the resolution tests run against. It
// mirrors the real shapes that motivated the design: role-suffixed services, a
// library that shares a word with a config segment, two libraries claiming the
// same basename, and an application with no artifacts at all.
func intercallFixture() []ProjectEntry {
	return []ProjectEntry{
		// A project whose package name carries the role word its PATH does not, so
		// the name-derived alias is the only way to reach it.
		{ID: "/data/workloads/events", Path: "data/workloads/events", Name: "@cloud/event-server", Type: "application"},
		{ID: "/delivery/libs/ci", Path: "delivery/libs/ci", Name: "@cloud/ci", Type: "library"},
		{
			ID: "/delivery/workloads/cache-server", Path: "delivery/workloads/cache-server",
			Name: "@cloud/cache-server", Type: "application",
		},
		{ID: "/distribution/libs/core", Path: "distribution/libs/core", Name: "@cloud/distribution-core", Type: "library",
			ConfigKeys: []ConfigKey{{Key: "core.auth.introspection.url", Type: "string"}}},
		{ID: "/identity/libs/core", Path: "identity/libs/core", Name: "@cloud/identity-core", Type: "library",
			ConfigKeys: []ConfigKey{{Key: "core.mode", Type: "string"}}},
		{
			ID: "/identity/workloads/auth-server", Path: "identity/workloads/auth-server",
			Name: "@cloud/auth-server", Type: "application",
			ConfigKeys: []ConfigKey{{Key: "auth.baseUrl", Type: "string"}},
		},
		{
			ID: "/observability/workloads/otel-server", Path: "observability/workloads/otel-server",
			Name: "@cloud/otel-server", Type: "application",
			ConfigKeys: []ConfigKey{{Key: "collector.auth.jwksUrl", Type: "string"}},
		},
	}
}

// TestLinkIntercalls_ResolvesAgainstTheLiveProjectSet is the acceptance test for
// gap 1: the edges a real config schema implies, and — just as
// important — the ones the derivation must REFUSE to invent.
func TestLinkIntercalls_ResolvesAgainstTheLiveProjectSet(t *testing.T) {
	entries := intercallFixture()
	// A key that would resolve to a library (delivery/libs/ci) if the addressable
	// guard were missing, one that resolves to nothing, and one that resolves to
	// a role-stemmed service.
	entries = append(entries, ProjectEntry{
		ID: "/surfaces/workloads/api", Path: "surfaces/workloads/api", Name: "@cloud/api", Type: "application",
		ConfigKeys: []ConfigKey{
			{Key: "controlPlaneAPI.auth.introspection.url", Type: "string"},
			{Key: "controlPlaneAPI.auth.putnamiJwks", Type: "string"},
			{Key: "controlPlaneAPI.delivery.ci.ingestBaseUrl", Type: "string"},
			{Key: "controlPlaneAPI.cloud.configServerUrl", Type: "string"},
			{Key: "controlPlaneAPI.server.port", Type: "integer"},
		},
	})
	sortEntries(entries)
	linkIntercalls(entries)

	byID := map[string]ProjectEntry{}
	for _, e := range entries {
		byID[e.ID] = e
	}

	api := byID["/surfaces/workloads/api"]
	if len(api.Intercalls) != 1 {
		t.Fatalf("api intercalls = %+v, want exactly one edge (auth-server)", api.Intercalls)
	}
	if api.Intercalls[0].To != "/identity/workloads/auth-server" {
		t.Errorf("api calls %q, want the auth server", api.Intercalls[0].To)
	}
	wantEvidence := []string{"controlPlaneAPI.auth.introspection.url", "controlPlaneAPI.auth.putnamiJwks"}
	if strings.Join(api.Intercalls[0].ConfigKeys, "|") != strings.Join(wantEvidence, "|") {
		t.Errorf("api edge evidence = %v, want %v (sorted, one entry per distinct target)",
			api.Intercalls[0].ConfigKeys, wantEvidence)
	}

	// The library that a config segment names is NOT a runtime unit, so
	// `…delivery.ci.ingestBaseUrl` must not become an edge to it.
	if got := byID["/delivery/libs/ci"]; len(got.CalledBy) != 0 {
		t.Errorf("a library with no endpoints, config keys, or application type was called by %v", got.CalledBy)
	}

	// A config root named for its own project is a self-reference, not a call.
	if got := byID["/identity/workloads/auth-server"]; len(got.Intercalls) != 0 {
		t.Errorf("auth-server's own `auth.baseUrl` produced outbound edges: %+v", got.Intercalls)
	}

	// The reverse edge is what makes "who calls X at runtime" a lookup.
	wantCallers := []string{
		"/distribution/libs/core", "/observability/workloads/otel-server", "/surfaces/workloads/api",
	}
	if got := byID["/identity/workloads/auth-server"].CalledBy; strings.Join(got, "|") != strings.Join(wantCallers, "|") {
		t.Errorf("auth-server calledBy = %v, want %v", got, wantCallers)
	}
	// A library IS addressable once it declares config keys, and it is a caller
	// like any other.
	if got := byID["/distribution/libs/core"].Intercalls; len(got) != 1 ||
		got[0].To != "/identity/workloads/auth-server" {
		t.Errorf("distribution/libs/core intercalls = %+v, want the auth server", got)
	}
}

// TestLinkIntercalls_RefusesToGuess pins the fail-closed cases one at a time:
// an ambiguous alias, an unresolvable hint, and a target that is not a runtime
// unit each yield NO edge rather than a plausible one.
func TestLinkIntercalls_RefusesToGuess(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		wantT string
	}{
		{"ambiguous basename claimed by two addressable projects", "svc.core.baseUrl", ""},
		{"unresolvable hint", "svc.metrics.otlpEndpoint", ""},
		{"not a runtime unit", "svc.ci.ingestUrl", ""},
		{"role stem resolves", "svc.auth.jwksUrl", "/identity/workloads/auth-server"},
		{"basename alias", "svc.cacheServer.url", "/delivery/workloads/cache-server"},
		{"package-name alias", "svc.eventServer.url", "/data/workloads/events"},
		{"package-name role stem", "svc.event.jwksUrl", "/data/workloads/events"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries := append(intercallFixture(), ProjectEntry{
				ID: "/svc/caller", Path: "svc/caller", Name: "@cloud/caller", Type: "application",
				ConfigKeys: []ConfigKey{{Key: tc.key, Type: "string"}},
			})
			sortEntries(entries)
			linkIntercalls(entries)

			var got []Intercall
			for _, e := range entries {
				if e.ID == "/svc/caller" {
					got = e.Intercalls
				}
			}
			if tc.wantT == "" {
				if len(got) != 0 {
					t.Fatalf("key %q produced %+v, want no edge", tc.key, got)
				}
				return
			}
			if len(got) != 1 || got[0].To != tc.wantT {
				t.Fatalf("key %q produced %+v, want a single edge to %s", tc.key, got, tc.wantT)
			}
		})
	}
}

// TestLinkIntercalls_IsDeterministic guards the package's whole reason to exist:
// the derivation walks maps internally, so repeated runs over the same entries
// must produce identical, sorted output.
func TestLinkIntercalls_IsDeterministic(t *testing.T) {
	build := func() []ProjectEntry {
		entries := append(intercallFixture(), ProjectEntry{
			ID: "/svc/caller", Path: "svc/caller", Name: "@cloud/caller", Type: "application",
			ConfigKeys: []ConfigKey{
				{Key: "svc.auth.jwksUrl", Type: "string"},
				{Key: "svc.auth.introspection.url", Type: "string"},
				{Key: "svc.cacheServer.url", Type: "string"},
				{Key: "svc.otelServer.endpoint", Type: "string"},
			},
		})
		sortEntries(entries)
		linkIntercalls(entries)
		return entries
	}
	first := renderEdges(t, build())
	for i := range 8 {
		if got := renderEdges(t, build()); got != first {
			t.Fatalf("iteration %d: intercall derivation is not deterministic\n--- first:\n%s\n--- got:\n%s", i, first, got)
		}
	}
	// The evidence for a multi-key edge is sorted, and each target appears once.
	for _, e := range build() {
		if e.ID != "/svc/caller" {
			continue
		}
		if len(e.Intercalls) != 3 {
			t.Fatalf("caller intercalls = %+v, want three distinct targets", e.Intercalls)
		}
		for i, call := range e.Intercalls {
			if i > 0 && e.Intercalls[i-1].To >= call.To {
				t.Errorf("intercalls are not sorted by target: %+v", e.Intercalls)
			}
			for j, key := range call.ConfigKeys {
				if j > 0 && call.ConfigKeys[j-1] >= key {
					t.Errorf("evidence keys are not sorted: %v", call.ConfigKeys)
				}
			}
		}
	}
}

// renderEdges serializes the derived edges for a byte comparison.
func renderEdges(t *testing.T, entries []ProjectEntry) string {
	t.Helper()
	var b strings.Builder
	for _, e := range entries {
		for _, call := range e.Intercalls {
			b.WriteString(e.ID + " -> " + call.To + " " + strings.Join(call.ConfigKeys, ",") + "\n")
		}
		if len(e.CalledBy) > 0 {
			b.WriteString(e.ID + " <- " + strings.Join(e.CalledBy, ",") + "\n")
		}
	}
	return b.String()
}
