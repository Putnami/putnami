package provider

import (
	"regexp"
	"strings"
)

// An idempotency marker records, inside the body of the issue or review a
// keyed create produced, the key that produced it and a digest of what the
// request asked for. It is an HTML comment, so GitHub renders nothing, and it
// is the only record of the key: finding it is how a repeated create returns
// the first result instead of a second item.
//
//	<!-- putnami-idempotency k=<sha256 of operation and key> d=<sha256 of the request> -->

var markerPattern = regexp.MustCompile(`(?:\r?\n)*<!-- putnami-idempotency k=([0-9a-f]{64}) d=([0-9a-f]{64}) -->\s*$`)

// idempotency is one key and the request it was used for.
type idempotency struct {
	key    string
	digest string
}

// newIdempotency hashes an operation's key and the request it carries.
func newIdempotency(operation, key string, request any) idempotency {
	return idempotency{key: digest([]string{operation, key}), digest: digest(request)}
}

func (m idempotency) comment() string {
	return "<!-- putnami-idempotency k=" + m.key + " d=" + m.digest + " -->"
}

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
	return body[:match[0]], &idempotency{key: body[match[2]:match[3]], digest: body[match[4]:match[5]]}
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
