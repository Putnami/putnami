package main

import (
	"go.putnami.dev/protocol/features/spectest"

	"net/http/httptest"
	"testing"

	phttp "go.putnami.dev/http"
)

func ctxWith(remoteAddr, xff string) *phttp.Context {
	r := httptest.NewRequest("POST", "/v1/logs", nil)
	r.RemoteAddr = remoteAddr
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return phttp.NewContext(httptest.NewRecorder(), r)
}

func TestClientIPDerivationMatrix(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "caller-address-is-not-retained", "the-caller-address-is-derived-only-for-request-limits")
	cases := []struct {
		name       string
		trusted    []string
		remoteAddr string
		xff        string
		want       string
	}{
		{
			name:       "empty trusted list keys on RemoteAddr (fail-safe)",
			trusted:    nil,
			remoteAddr: "198.51.100.7:5555",
			xff:        "203.0.113.9",
			want:       "198.51.100.7",
		},
		{
			name:       "trusted peer honors rightmost XFF hop",
			trusted:    []string{"10.0.0.1"},
			remoteAddr: "10.0.0.1:5555",
			xff:        "203.0.113.9",
			want:       "203.0.113.9",
		},
		{
			name:       "untrusted direct peer ignores XFF",
			trusted:    []string{"10.0.0.1"},
			remoteAddr: "192.0.2.5:5555",
			xff:        "203.0.113.9",
			want:       "192.0.2.5",
		},
		{
			name:       "rightmost-non-trusted skips trusted hops",
			trusted:    []string{"10.0.0.1", "10.0.0.2"},
			remoteAddr: "10.0.0.1:5555",
			xff:        "203.0.113.9, 10.0.0.2",
			want:       "203.0.113.9",
		},
		{
			name:       "forged prepended hop is ignored (takes real rightmost client)",
			trusted:    []string{"10.0.0.1"},
			remoteAddr: "10.0.0.1:5555",
			xff:        "1.2.3.4, 203.0.113.9",
			want:       "203.0.113.9",
		},
		{
			name:       "trusted CIDR range",
			trusted:    []string{"10.0.0.0/8"},
			remoteAddr: "10.9.9.9:5555",
			xff:        "203.0.113.9",
			want:       "203.0.113.9",
		},
		{
			name:       "all XFF hops trusted falls back to peer",
			trusted:    []string{"10.0.0.0/8"},
			remoteAddr: "10.0.0.1:5555",
			xff:        "10.0.0.2, 10.0.0.3",
			want:       "10.0.0.1",
		},
		{
			name:       "trusted peer with no XFF uses peer",
			trusted:    []string{"10.0.0.1"},
			remoteAddr: "10.0.0.1:5555",
			xff:        "",
			want:       "10.0.0.1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clientIP(ctxWith(tc.remoteAddr, tc.xff), parseTrustSet(tc.trusted))
			if got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}
