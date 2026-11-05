// Package collabhelper holds what the collaboration providers of
// go.putnami.dev/protocol/collaboration share: the bounds the text of an
// answer meets, the pauses between attempts, the digest a revision token is
// built from, the redaction of credentials, and the checks an API override
// and a redirect pass.
//
// It imports no protocol. A caller passes the contract's limits, so one
// helper serves every provider and every contract version.
package collabhelper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Bounded collapses every run of whitespace in text to one space and cuts it
// to limit characters, marking a cut with an ellipsis.
func Bounded(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}

// OneLine fits a title into one line of at most limit characters, never
// empty: whitespace runs collapse to one space, an empty title becomes
// fallback, and a longer one ends in an ellipsis within the limit.
func OneLine(value string, limit int, fallback string) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		value = fallback
	}
	if utf8.RuneCountInString(value) > limit {
		runes := []rune(value)
		value = string(runes[:limit-1]) + "…"
	}
	return value
}

// BoundedBody drops every NUL byte of a body and cuts it to at most limit
// bytes on a character boundary.
func BoundedBody(value string, limit int) string {
	value = strings.ReplaceAll(value, "\x00", "")
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

// BoundedLabels keeps, in order, at most maxLabels distinct non-empty names
// of at most maxLength characters. The list is never nil, so an answer
// reports a label-less item as [].
func BoundedLabels(names []string, maxLabels, maxLength int) []string {
	labels := []string{}
	for _, label := range names {
		if len(labels) < maxLabels && label != "" && utf8.RuneCountInString(label) <= maxLength && !slices.Contains(labels, label) {
			labels = append(labels, label)
		}
	}
	return labels
}

// SameSet reports whether two lists hold the same values, ignoring order and
// repetition.
func SameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}

// Digest is the hex SHA-256 of a value's JSON encoding, or of its default
// formatting when it has no JSON encoding.
func Digest(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		encoded = []byte(fmt.Sprint(value))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

var timestampPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$`)

// Timestamp keeps value when it is an RFC 3339 date and time, and returns the
// empty string otherwise.
func Timestamp(value string) string {
	if timestampPattern.MatchString(value) {
		return value
	}
	return ""
}

// DisplayURL keeps a link of at most maxLength bytes only when it is an
// absolute http or https URL without credentials, and returns the empty
// string otherwise.
func DisplayURL(value string, maxLength int) string {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil ||
		len(value) > maxLength {
		return ""
	}
	return value
}

// Backoff waits before repeat attempt n (1-based) of a read: 1s, then 3s. It
// returns the context's error when the context ends first.
func Backoff(ctx context.Context, attempt int) error {
	delay := time.Second
	if attempt > 1 {
		delay = 3 * time.Second
	}
	return wait(ctx, delay)
}

// ReconcilePause waits before read-back attempt n (1-based) of a write whose
// outcome is unknown: n seconds. It returns the context's error when the
// context ends first.
func ReconcilePause(ctx context.Context, attempt int) error {
	return wait(ctx, time.Duration(attempt)*time.Second)
}

func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// RedactedMarker replaces every redacted credential.
const RedactedMarker = "[redacted]"

// Redact replaces in text every secret of at least four bytes, and every
// match of pattern, the shape of the provider's published credentials, with
// RedactedMarker.
func Redact(text string, secrets []string, pattern *regexp.Regexp) string {
	for _, secret := range secrets {
		if len(secret) >= 4 {
			text = strings.ReplaceAll(text, secret, RedactedMarker)
		}
	}
	return pattern.ReplaceAllString(text, RedactedMarker)
}

var hostPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?(?::[0-9]{1,5})?$`)

// ValidHost reports whether host is a bare host name, optionally with a port:
// no scheme, path, query or user information.
func ValidHost(host string) bool {
	return hostPattern.MatchString(host)
}

// LoopbackOverride parses the value of the environment variable named
// variable that points a client at another API root. Only an http or https
// URL on the loopback interface, without credentials, query or fragment, is
// accepted, so the variable reaches a local stand-in and nothing else.
func LoopbackOverride(variable, value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || !loopback(parsed.Hostname()) {
		return nil, fmt.Errorf("%s must be an http or https URL on the loopback interface, without credentials or query", variable)
	}
	return parsed, nil
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// FollowReadsOnSameHost is an http.Client CheckRedirect that follows at most
// four redirects of a GET or HEAD that stay on the host it started on. A
// write is never replayed at another location: its 3xx answer is the answer.
func FollowReadsOnSameHost(request *http.Request, via []*http.Request) error {
	first := via[0]
	if len(via) >= 5 || (first.Method != http.MethodGet && first.Method != http.MethodHead) || request.URL.Host != first.URL.Host {
		return http.ErrUseLastResponse
	}
	return nil
}
