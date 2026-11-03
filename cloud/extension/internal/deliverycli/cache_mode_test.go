package deliverycli

import (
	"encoding/json"
	"testing"
)

// TestBuildCacheConfigRecordsOnlyARequestedMode pins what setup and install
// write: a mode only when the flag or PUTNAMI_CACHE_MODE requests one, the
// flag first. Without one the file holds no mode key, so the cache provider
// picks the mode per run. An unknown mode is still a usage error.
func TestBuildCacheConfigRecordsOnlyARequestedMode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		params  map[string]any
		env     map[string]string
		want    string
		invalid bool
	}{
		{name: "none requested"},
		{name: "flag", params: map[string]any{"cache-mode": "minimal"}, want: "minimal"},
		{name: "camel-case flag", params: map[string]any{"cacheMode": "toplevel"}, want: "toplevel"},
		{name: "environment", env: map[string]string{"PUTNAMI_CACHE_MODE": "full"}, want: "full"},
		{name: "flag over environment", params: map[string]any{"cache-mode": "minimal"}, env: map[string]string{"PUTNAMI_CACHE_MODE": "full"}, want: "minimal"},
		{name: "unknown flag", params: map[string]any{"cache-mode": "turbo"}, invalid: true},
		{name: "unknown environment", env: map[string]string{"PUTNAMI_CACHE_MODE": "turbo"}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"PUTNAMI_CACHE_MODE": ""}
			for key, value := range tc.env {
				env[key] = value
			}
			cfg, err := BuildCacheConfig(tc.params, env)
			if tc.invalid {
				if err == nil {
					t.Fatalf("BuildCacheConfig = %+v, want a usage error", cfg)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Mode != tc.want {
				t.Fatalf("mode = %q, want %q", cfg.Mode, tc.want)
			}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var written map[string]any
			if err := json.Unmarshal(data, &written); err != nil {
				t.Fatal(err)
			}
			if mode, recorded := written["mode"]; recorded != (tc.want != "") {
				t.Fatalf("written mode = %v (recorded %t), want recorded only when requested", mode, recorded)
			}
			if _, shown := CacheSummary(cfg, "")["mode"]; shown != (tc.want != "") {
				t.Fatalf("summary shows a mode = %t, want only when one is recorded", shown)
			}
		})
	}
}
