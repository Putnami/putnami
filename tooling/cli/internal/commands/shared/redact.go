package shared

import "regexp"

// credentialInURLPattern matches the userinfo component of a URL — everything
// between the scheme and the `@` that precedes the host.
var credentialInURLPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/@\s"']+@`)

// queryInURLPattern matches a URL's query string, which private registries use
// to carry signed tokens.
var queryInURLPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^\s"'?]*)\?[^\s"']*`)

// RedactURLCredentials removes both credential carriers from arbitrary error
// text or command output, leaving a visible marker so a reader can tell
// something was withheld rather than wondering why their URL looks wrong.
//
// It scrubs the rendered message rather than the URL value because the leak is
// not one call site: net/http masks the password but keeps the username, the
// registry validator quotes the raw PUTNAMI_REGISTRY_URL in full, and archive
// errors quote the download URL with its query. Scrubbing the text covers all
// three without depending on which one produced this error.
func RedactURLCredentials(message string) string {
	message = credentialInURLPattern.ReplaceAllString(message, "${1}"+RedactedMarker+"@")
	return queryInURLPattern.ReplaceAllString(message, "${1}?"+RedactedMarker)
}

// RedactedMarker replaces what RedactURLCredentials withholds.
const RedactedMarker = "[redacted]"
