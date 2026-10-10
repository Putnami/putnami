package remotecache

import "testing"

func TestValidateURL(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		allowEnv  bool
		wantError bool
	}{
		{name: "https accepted", url: "https://cache.putnami.cloud", wantError: false},
		{name: "https with path", url: "https://cache.example/v1", wantError: false},
		{name: "http loopback localhost", url: "http://localhost:8080", wantError: false},
		{name: "http loopback 127.0.0.1", url: "http://127.0.0.1:9000/cache", wantError: false},
		{name: "http loopback ipv6", url: "http://[::1]:9000", wantError: false},
		{name: "http non-loopback rejected", url: "http://cache.example", wantError: true},
		{name: "http non-loopback allowed via env", url: "http://cache.example", allowEnv: true, wantError: false},
		{name: "ftp scheme rejected", url: "ftp://cache.example", wantError: true},
		{name: "ftp scheme rejected even with env", url: "ftp://cache.example", allowEnv: true, wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.allowEnv {
				t.Setenv(AllowInsecureCacheEnv, "1")
			}
			err := ValidateURL(tt.url)
			if tt.wantError && err == nil {
				t.Errorf("ValidateURL(%q) = nil, want error", tt.url)
			}
			if !tt.wantError && err != nil {
				t.Errorf("ValidateURL(%q) = %v, want nil", tt.url, err)
			}
		})
	}
}
