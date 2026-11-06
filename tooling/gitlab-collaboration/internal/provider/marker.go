package provider

import (
	"regexp"
	"strings"

	"go.putnami.dev/sdk/extension/collabhelper"
)

// An idempotency marker records, inside the body of the issue a
// keyed create produced, the key that produced it and a digest of what the
// request asked for. It is an HTML comment, so GitLab renders nothing, and it
// is the only record of the key: finding it is how a repeated create returns
// the first result instead of a second item.
//
// Each hash is written as two words of 32 hex digits: GitLab's search index
// drops words of 50 characters or more from a description.
//
//	<!-- putnami-idempotency k=<sha256 of operation and key, in two halves> d=<sha256 of the request, in two halves> -->

var markerPattern = regexp.MustCompile(`(?:\r?\n)*<!-- putnami-idempotency k=([0-9a-f]{32}) ([0-9a-f]{32}) d=([0-9a-f]{32}) ([0-9a-f]{32}) -->\s*$`)

// idempotency is one key and the request it was used for.
type idempotency struct {
	key    string
	digest string
}

// newIdempotency hashes an operation's key and the request it carries.
func newIdempotency(operation, key string, request any) idempotency {
	return idempotency{key: collabhelper.Digest([]string{operation, key}), digest: collabhelper.Digest(request)}
}

func (m idempotency) comment() string {
	return "<!-- putnami-idempotency k=" + m.key[:32] + " " + m.key[32:] + " d=" + m.digest[:32] + " " + m.digest[32:] + " -->"
}

// searchWord is the word of the marker GitLab's search finds it by.
func (m idempotency) searchWord() string { return m.key[:32] }

// withMarker appends a marker to a body.
func withMarker(body string, marker idempotency) string {
	if strings.TrimSpace(body) == "" {
		return marker.comment()
	}
	return strings.TrimRight(body, "\r\n") + "\n\n" + marker.comment()
}

// splitMarker separates a body from the marker at its end.
func splitMarker(body string) (string, *idempotency) {
	match := markerPattern.FindStringSubmatchIndex(body)
	if match == nil {
		return body, nil
	}
	return body[:match[0]], &idempotency{key: body[match[2]:match[3]] + body[match[4]:match[5]], digest: body[match[6]:match[7]] + body[match[8]:match[9]]}
}

// rejoin replaces the text of a body and keeps the marker it carried.
func rejoin(raw, text string) string {
	_, marker := splitMarker(raw)
	text, _ = splitMarker(text)
	if marker == nil {
		return text
	}
	return withMarker(text, *marker)
}
