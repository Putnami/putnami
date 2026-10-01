package shared

import "testing"

// The userinfo and the query of every URL in a text are withheld and marked;
// the rest of the text, and a URL without either, stays as it was.
func TestRedactURLCredentialsWithholdsUserinfoAndQuery(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{
			"reading https://ci:s3cret@proxy.example.com/app/@v/list: 404",
			"reading https://[redacted]@proxy.example.com/app/@v/list: 404",
		},
		{
			"GET https://registry.example.com/a.tgz?token=abc failed",
			"GET https://registry.example.com/a.tgz?[redacted] failed",
		},
		{"no URL here", "no URL here"},
		{"https://proxy.golang.org/app/@latest", "https://proxy.golang.org/app/@latest"},
	} {
		if got := RedactURLCredentials(tc.in); got != tc.want {
			t.Errorf("RedactURLCredentials(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
