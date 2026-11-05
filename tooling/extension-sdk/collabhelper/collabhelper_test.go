package collabhelper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBoundedCollapsesWhitespaceAndCutsOnCharacters(t *testing.T) {
	if got := Bounded("  a \n\t b  ", 400); got != "a b" {
		t.Fatalf("Bounded = %q", got)
	}
	if got := Bounded(strings.Repeat("é", 5), 3); got != "ééé…" {
		t.Fatalf("Bounded = %q, want three characters and an ellipsis", got)
	}
	if got := Bounded("abc", 3); got != "abc" {
		t.Fatalf("Bounded = %q, want the text at the limit unchanged", got)
	}
}

func TestOneLineFitsTheLimitAndIsNeverEmpty(t *testing.T) {
	if got := OneLine(" \n ", 10, "#7"); got != "#7" {
		t.Fatalf("OneLine = %q, want the fallback", got)
	}
	if got := OneLine("one\ntwo", 10, "x"); got != "one two" {
		t.Fatalf("OneLine = %q", got)
	}
	if got := OneLine("abcdef", 4, "x"); got != "abc…" {
		t.Fatalf("OneLine = %q, want four characters ending in an ellipsis", got)
	}
}

func TestBoundedBodyDropsNULAndCutsOnACharacterBoundary(t *testing.T) {
	if got := BoundedBody("a\x00b", 10); got != "ab" {
		t.Fatalf("BoundedBody = %q", got)
	}
	if got := BoundedBody("aé", 2); got != "a" {
		t.Fatalf("BoundedBody = %q, want the cut before the two-byte character", got)
	}
	if got := BoundedBody("abc", 3); got != "abc" {
		t.Fatalf("BoundedBody = %q", got)
	}
}

func TestBoundedLabelsKeepsDistinctBoundedNamesAndIsNeverNil(t *testing.T) {
	if got := BoundedLabels(nil, 2, 3); got == nil || len(got) != 0 {
		t.Fatalf("BoundedLabels(nil) = %#v, want an empty list", got)
	}
	got := BoundedLabels([]string{"a", "", "a", "long", "b", "c"}, 2, 3)
	if !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("BoundedLabels = %q", got)
	}
}

func TestSameSetIgnoresOrderAndRepetition(t *testing.T) {
	a := []string{"b", "a", "a"}
	if !SameSet(a, []string{"a", "b"}) || SameSet(a, []string{"a"}) {
		t.Fatal("SameSet compares the wrong sets")
	}
	if !slices.Equal(a, []string{"b", "a", "a"}) {
		t.Fatalf("SameSet reordered its argument: %q", a)
	}
}

func TestDigestIsTheHexSHA256OfTheJSONEncoding(t *testing.T) {
	sum := sha256.Sum256([]byte(`["a"]`))
	if got := Digest([]string{"a"}); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("Digest = %q, want the SHA-256 of the JSON encoding", got)
	}
	if got := Digest(func() {}); len(got) != 64 {
		t.Fatalf("Digest of a value without a JSON encoding = %q", got)
	}
}

func TestTimestampKeepsRFC3339Only(t *testing.T) {
	for _, value := range []string{"2026-09-24T08:00:00Z", "2026-09-24T08:00:00.123+02:00"} {
		if Timestamp(value) != value {
			t.Errorf("Timestamp(%q) dropped a valid timestamp", value)
		}
	}
	for _, value := range []string{"", "2026-09-24", "2026-09-24T08:00:00.000+0000"} {
		if got := Timestamp(value); got != "" {
			t.Errorf("Timestamp(%q) = %q, want empty", value, got)
		}
	}
}

func TestDisplayURLKeepsOnlyCredentialFreeHTTPLinks(t *testing.T) {
	if got := DisplayURL("https://example.com/x", 100); got != "https://example.com/x" {
		t.Fatalf("DisplayURL dropped a valid link: %q", got)
	}
	for _, value := range []string{"ftp://example.com", "https://user:pw@example.com", "/relative", "https://example.com/" + strings.Repeat("a", 100)} {
		if got := DisplayURL(value, 100); got != "" {
			t.Errorf("DisplayURL(%q) = %q, want empty", value, got)
		}
	}
}

func TestPausesWaitTheirScheduleAndStopWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Backoff(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("Backoff on a canceled context = %v", err)
	}
	if err := ReconcilePause(ctx, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReconcilePause on a canceled context = %v", err)
	}
	start := time.Now()
	if err := Backoff(context.Background(), 1); err != nil || time.Since(start) < time.Second {
		t.Fatalf("Backoff(1) = %v after %s, want a one-second wait", err, time.Since(start))
	}
}

func TestRedactReplacesSecretsAndThePattern(t *testing.T) {
	pattern := regexp.MustCompile(`\btok_[a-z]{4}\b`)
	got := Redact("mine s3cret, short abc, theirs tok_abcd", []string{"s3cret", "abc"}, pattern)
	if got != "mine [redacted], short abc, theirs [redacted]" {
		t.Fatalf("Redact = %q", got)
	}
}

func TestLoopbackOverrideRefusesAnyOtherURLWithOneMessage(t *testing.T) {
	for _, value := range []string{"https://example.com", "ftp://127.0.0.1", "http://u:p@127.0.0.1", "http://127.0.0.1/?q=1", "http://127.0.0.1/#f", "://"} {
		parsed, err := LoopbackOverride("VAR", value)
		if err == nil || parsed != nil || err.Error() != "VAR must be an http or https URL on the loopback interface, without credentials or query" {
			t.Errorf("LoopbackOverride(%q) = %v, %v", value, parsed, err)
		}
	}
}

func TestFollowReadsOnSameHostFollowsAHeadAndStopsAtTheFifthHop(t *testing.T) {
	request := func(method, raw string) *http.Request {
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Request{Method: method, URL: parsed}
	}
	next := request(http.MethodGet, "https://api.example.com/b")
	if err := FollowReadsOnSameHost(next, []*http.Request{request(http.MethodHead, "https://api.example.com/a")}); err != nil {
		t.Errorf("a HEAD on its host was not followed: %v", err)
	}
	fifth := slices.Repeat([]*http.Request{request(http.MethodGet, "https://api.example.com/a")}, 5)
	if err := FollowReadsOnSameHost(next, fifth); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("a fifth hop was followed: %v", err)
	}
}
