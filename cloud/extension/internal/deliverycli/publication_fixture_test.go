package deliverycli

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
)

// providerFixturePath is the copy, in this project, of the CI runner image's
// fixture tests/fixtures/provider-publication-stderr.txt.
// The runner reads these provider lines for its channel note
// (ci-run-native.sh native_note_publication_channel). A test that read the
// runner's file directly would depend on a file outside this project's inputs,
// so its cached verdict would survive a change to that file.
const providerFixturePath = "testdata/provider-publication-stderr.txt"

// providerFixtureSHA256 is the SHA-256 of both copies of the fixture. Each
// project reads only its own copy and pins this digest:
// TestProviderFixtureMatchesTheSharedDigest here, and native_phase_facts_test.sh
// in the CI runner image. A change to one copy fails that project's test
// until both copies and both pins move together.
const providerFixtureSHA256 = "9a6adc55405d5f04ec3bdede51c86c85f8a8fa94d8a3024a27ba8e6d21a98fa4"

// TestProviderFixtureMatchesTheSharedDigest pins this project's copy of the
// runner fixture to the digest the runner's suite pins for its own copy.
func TestProviderFixtureMatchesTheSharedDigest(t *testing.T) {
	data, err := os.ReadFile(providerFixturePath)
	if err != nil {
		t.Fatalf("read %s: %v", providerFixturePath, err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != providerFixtureSHA256 {
		t.Fatalf("%s sha256 = %s, want %s: update both copies of the fixture and both pins together "+
			"(the CI runner image's tests/fixtures/provider-publication-stderr.txt, native_phase_facts_test.sh)",
			providerFixturePath, got, providerFixtureSHA256)
	}
}

// providerFixtureReproduced reports whether line is one of the kinds this test
// has the provider write: a release that leaves its forward-only channels
// (publication.go releaseForwardOnly), with or without a newer head, and the
// retry after a forward-only channel moved (publication.go releaseChannels).
func providerFixtureReproduced(line string) bool {
	return strings.Contains(line, " is not the head of its default branch (") ||
		strings.HasSuffix(line, "release: a forward-only channel moved after it was read; reading it again")
}

// TestPublicationDiagnosticsMatchTheRunnerFixture pins the provider side of
// the channel note contract. The provider writes each line through
// credentialServer.diagnose, which adds the prefix and bounds the line, and the
// runner parses the result. Every reproducible line of the fixture must be one
// the provider writes, byte for byte, and every such line the provider writes
// here must be in the fixture.
func TestPublicationDiagnosticsMatchTheRunnerFixture(t *testing.T) {
	data, err := os.ReadFile(providerFixturePath)
	if err != nil {
		t.Fatalf("read %s: %v", providerFixturePath, err)
	}
	want := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "putnami-cloud credential-provider: ") && providerFixtureReproduced(line) {
			want[line] = true
		}
	}
	if len(want) != 4 {
		t.Fatalf("fixture reproducible lines = %d, want 4: two superseded releases, one with no newer head, one retry", len(want))
	}

	got := map[string]bool{}
	collect := func(r *pubRig) {
		for _, line := range strings.Split(r.h.stderr.String(), "\n") {
			if providerFixtureReproduced(line) {
				got[line] = true
			}
		}
	}
	// A superseded run: Delivery names the newer head of the default branch.
	{
		r := newPubRig(t, nil).start()
		release := r.layout([]string{pubCanary}, "", []string{pubCanary})
		r.delivery.set(func(d *fakeDelivery) { d.advance, d.advanceHead = false, pubNewerHead })
		payload, requestBytes := release.payload(t)
		mustReleased(t, r.release(payload), requestBytes)
		collect(r)
	}
	// A rerun of an older commit: Delivery names no newer head.
	{
		r := newPubRig(t, nil).start()
		r.put.move(pubCanary, pubRestatedSet("b"))
		release := r.layout([]string{pubCanary}, "", []string{pubCanary})
		r.delivery.set(func(d *fakeDelivery) { d.advance = false })
		payload, requestBytes := release.payload(t)
		mustReleased(t, r.release(payload), requestBytes)
		collect(r)
	}
	// A forward-only channel moves between the read and the release.
	{
		r := newPubRig(t, nil).start().opened()
		r.put.set(func(p *fakePutServer) {
			p.onRelease = func(call int, _ http.ResponseWriter, _ *http.Request) bool {
				if call == 1 {
					r.put.move(pubCanary, pubRestatedSet("b"))
				}
				return false
			}
		})
		payload, requestBytes := r.defaultRelease().payload(t)
		mustReleased(t, r.release(payload), requestBytes)
		collect(r)
	}
	// A superseded run that leaves two forward-only channels, one of them
	// without a head.
	{
		r := newPubRig(t, nil).start()
		release := r.layout([]string{pubCanary, pubPreview}, "", []string{pubCanary, pubPreview})
		r.delivery.set(func(d *fakeDelivery) { d.advance, d.advanceHead = false, strings.Repeat("e", 40) })
		payload, _ := release.payload(t)
		_ = r.release(payload)
		collect(r)
	}

	var missing, extra []string
	for line := range want {
		if !got[line] {
			missing = append(missing, line)
		}
	}
	for line := range got {
		if !want[line] {
			extra = append(extra, line)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("fixture lines the provider did not write:\n%s\nprovider lines the fixture lacks:\n%s",
			strings.Join(missing, "\n"), strings.Join(extra, "\n"))
	}
}
