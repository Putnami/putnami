package workspacejob

import (
	"regexp"
	"strings"
)

// outputExcerptMaxLines and outputExcerptMaxBytes bound the go command output
// a diagnostic quotes. go names the module it could not resolve last, so the
// end is kept.
const (
	outputExcerptMaxLines = 20
	outputExcerptMaxBytes = 2048
)

// credentialInURL matches the user information of a URL: everything between
// the scheme and the "@" before the host.
var credentialInURL = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/@\s"']+@`)

// queryInURL matches the query of a URL, which a private registry can use to
// carry a signed token.
var queryInURL = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^\s"'?]*)\?[^\s"']*`)

// redactedMarker replaces what RedactURLCredentials withholds.
const redactedMarker = "[redacted]"

// RedactURLCredentials replaces the user information and the query of every
// URL in text with a visible marker. A GOPROXY entry can carry a credential in
// either, and the go command quotes the URL it failed to read.
func RedactURLCredentials(text string) string {
	text = credentialInURL.ReplaceAllString(text, "${1}"+redactedMarker+"@")
	return queryInURL.ReplaceAllString(text, "${1}?"+redactedMarker)
}

// OutputExcerpt is the end of a go command's output, one line indented by
// four spaces per line, after a newline, or "" when the output is empty: at
// most the last 20 lines and 2048 bytes, with URL credentials redacted before
// the output is cut.
func OutputExcerpt(out string) string {
	text := strings.ReplaceAll(RedactURLCredentials(out), "\r\n", "\n")
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > outputExcerptMaxLines {
		lines = lines[len(lines)-outputExcerptMaxLines:]
	}
	text = strings.Join(lines, "\n")
	if len(text) > outputExcerptMaxBytes {
		text = strings.ToValidUTF8(text[len(text)-outputExcerptMaxBytes:], "")
	}
	if text == "" {
		return ""
	}
	return "\n    " + strings.ReplaceAll(text, "\n", "\n    ")
}
