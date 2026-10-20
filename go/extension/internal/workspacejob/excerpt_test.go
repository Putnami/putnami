package workspacejob

import (
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// The user information and the query of every URL in a text are withheld and
// marked; the rest of the text, and a URL without either, stays as it was.
func TestRedactURLCredentialsWithholdsUserinfoAndQuery(t *testing.T) {
	t.Parallel()
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

// The excerpt keeps the bounded end of the output, indented, and cuts a long
// line on a character boundary.
func TestOutputExcerptKeepsTheBoundedEnd(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/project-setup", "create-fails-when-go-mod-tidy-fails",
		"the-quoted-go-output-is-bounded-and-redacts-url-credentials")
	if got := OutputExcerpt(" \r\n\n"); got != "" {
		t.Errorf("excerpt of blank output = %q, want none", got)
	}
	if got, want := OutputExcerpt("go: first\r\ngo: second\r\n"), "\n    go: first\n    go: second"; got != want {
		t.Errorf("excerpt = %q, want %q", got, want)
	}
	many := make([]string, 0, 30)
	for i := range 30 {
		many = append(many, "line"+strings.Repeat("x", i))
	}
	if got := OutputExcerpt(strings.Join(many, "\n")); strings.Count(got, "\n    ") != outputExcerptMaxLines ||
		!strings.HasPrefix(got, "\n    "+many[10]+"\n") || !strings.HasSuffix(got, many[29]) {
		t.Errorf("excerpt of 30 lines = %q, want the last %d", got, outputExcerptMaxLines)
	}
	long := strings.Repeat("a", outputExcerptMaxBytes) + "é" + strings.Repeat("z", 10)
	got := OutputExcerpt(long)
	if !strings.HasSuffix(got, "é"+strings.Repeat("z", 10)) || len(strings.TrimPrefix(got, "\n    ")) > outputExcerptMaxBytes {
		t.Errorf("excerpt of a %d-byte line = %d bytes, want the last %d bytes", len(long), len(got), outputExcerptMaxBytes)
	}
	if got := OutputExcerpt("go: reading https://u:p@proxy.example/x: 401"); strings.Contains(got, "u:p") {
		t.Errorf("excerpt = %q, want the URL credentials redacted", got)
	}
}
