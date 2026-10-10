package deliverycli

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	distribution "go.putnami.dev/protocol/distribution"
	registry "go.putnami.dev/protocol/registry"
)

const (
	pubPreview = "preview"
	// pubStable is a mutable channel a tag run names besides its own, as the
	// CI rule {"tags": "v*", "publish": ["stable"]} does.
	pubStable = "stable"
)

// pubNewerHead is the newer default-branch head Delivery names.
var pubNewerHead = strings.Repeat("d", 40)

// layout narrows the run to channels, with immutable as its immutable channel
// ("" for none) and forwardOnly as Delivery's forward-only channels, resolves
// them and opens the plan for them. It returns the plan's release, expecting
// the heads the session resolved. Call it right after start.
func (r *pubRig) layout(channels []string, immutable string, forwardOnly []string) pubRelease {
	r.t.Helper()
	r.delivery.set(func(d *fakeDelivery) { d.channels, d.immutable, d.forwardOnly = channels, immutable, forwardOnly })
	return r.openLayout(mustResolved(r.t, r.resolve(pubResolveRequest(pubNamespace, channels...))).Heads, channels, immutable)
}

// tagLayout narrows the run to channels as layout does, but opens the plan
// without a resolve, as the engine does for a tag publish that names no
// channel: the release expects no head, and its set is the planned members.
func (r *pubRig) tagLayout(channels []string, immutable string, forwardOnly []string) pubRelease {
	r.t.Helper()
	r.delivery.set(func(d *fakeDelivery) { d.channels, d.immutable, d.forwardOnly = channels, immutable, forwardOnly })
	release := r.openLayout(nil, channels, immutable)
	release.request.ReleaseSet.Members = release.request.ReleaseSet.Members[1:]
	return release
}

func (r *pubRig) openLayout(heads map[string]*distribution.ChannelHead, channels []string, immutable string) pubRelease {
	r.t.Helper()
	resolved := distribution.ResolveResponse{Heads: heads}
	plan := pubPlan(r.t)
	plan.Channels, plan.ImmutableChannel = channels, immutable
	plan = pubSeal(r.t, plan)
	ancestry := registry.PublicationAncestry{SourceRevision: pubSource, SnapshotCommits: 12}
	request := pubReleaseRequest(nil)
	request.Channels = nil
	for _, name := range channels {
		channel := distribution.ChannelRequest{Name: name, Visibility: distribution.VisibilityPrivate, Immutable: name == immutable}
		state := registry.PublicationChannelAncestry{Name: name}
		if head := resolved.Heads[name]; head != nil && name != immutable {
			ref := head.Ref
			channel.Expected = &ref
			state.HeadSourceRevision, state.Ancestor = pubParentSource, true
		}
		request.Channels = append(request.Channels, channel)
		ancestry.Channels = append(ancestry.Channels, state)
	}
	mustOpened(r.t, r.open(registry.OpenParams{Plan: plan, Ancestry: ancestry}), plan.PlanDigest)
	return pubRelease{planDigest: plan.PlanDigest, request: request, ancestry: ancestry}
}

// pubRestatedSet is a canary head another run released after the engine
// resolved the base: it differs from the base only in a member this run
// rebuilds, so this run's set restates it.
func pubRestatedSet(fill string) distribution.ReleaseSet {
	set := pubBaseSet()
	set.Members[1] = pubMember("npm", "@acme/web", "1.2.2", pubParentSource, fill, "4")
	return set
}

// pubOtherSet is a head of a channel that is not forward-only, released by
// another run.
func pubOtherSet() distribution.ReleaseSet {
	set := pubBaseSet()
	set.Members[2] = pubMember("put", "acme/app", "1.2.1", pubOtherSource, "e", "f")
	return set
}

// releaseChannels decodes the channel list of the nth body put-server
// released, and checks its set is the engine's.
func (r *pubRig) releaseChannels(n int, request distribution.ReleaseRequest) []distribution.ChannelRequest {
	r.t.Helper()
	var sent distribution.ReleaseRequest
	r.put.set(func(p *fakePutServer) {
		if len(p.releaseBodies) <= n {
			r.t.Fatalf("put-server release bodies = %d, want at least %d", len(p.releaseBodies), n+1)
		}
		if err := json.Unmarshal(p.releaseBodies[n], &sent); err != nil {
			r.t.Fatalf("put-server release body %d: %v", n, err)
		}
	})
	if !reflect.DeepEqual(sent.ReleaseSet, request.ReleaseSet) || sent.Namespace != request.Namespace || !reflect.DeepEqual(sent.Visibility, request.Visibility) {
		r.t.Fatalf("put-server release = %+v, want the engine's set, namespace and visibility", sent)
	}
	return sent.Channels
}

func countLines(text, needle string) int {
	count := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			count++
		}
	}
	return count
}

// TestPublicationReleaseLeavesTheForwardOnlyChannelsOfARunThatIsNotTheHead
// pins the first half of D18: a run Delivery says is not the head of its
// default branch, superseded or a rerun of an older commit, leaves its
// forward-only channels where they are and ends green. Its other channels are
// released, and the engine reads the set as the head of every forward-only
// channel, at the generation the session resolved.
func TestPublicationReleaseLeavesTheForwardOnlyChannelsOfARunThatIsNotTheHead(t *testing.T) {
	released := func(t *testing.T) distribution.ReleaseSetRef { return pubRef(t, pubReleasedSet()) }

	t.Run("superseded with forward-only channels only", func(t *testing.T) {
		r := newPubRig(t, nil).start()
		release := r.layout([]string{pubCanary}, "", []string{pubCanary})
		r.delivery.set(func(d *fakeDelivery) { d.advance, d.advanceHead = false, pubNewerHead })
		payload, requestBytes := release.payload(t)
		answer := mustReleased(t, r.release(payload), requestBytes)
		want := map[string]*distribution.ChannelHead{pubCanary: {Ref: released(t), Generation: 3}}
		if answer.Outcome != distribution.ReleaseOutcomeAlreadyCurrent || !reflect.DeepEqual(answer.Current, want) {
			t.Fatalf("release = %+v, want already-current at the set on the canary's generation", answer)
		}
		if _, releases := r.put.calls(); releases != 0 {
			t.Fatalf("put-server releases = %d, want none", releases)
		}
		if head := r.put.head(pubCanary); head.Ref != r.baseRef {
			t.Fatalf("canary head = %+v, want the base left in place", head)
		}
		if _, publish := r.delivery.calls(); publish != 1 {
			t.Fatalf("Delivery publish calls = %d, want only the open's: nothing is released", publish)
		}
		stderr := r.h.stderr.String()
		if countLines(stderr, "is not the head of its default branch") != 1 || !strings.Contains(stderr, pubNewerHead) || !strings.Contains(stderr, "[canary]") {
			t.Fatalf("stderr = %q, want one line naming the skipped channel and Delivery's head", stderr)
		}
		// The answer is the plan's: the same release gets it again.
		if again := mustReleased(t, r.release(payload), requestBytes); !reflect.DeepEqual(again, answer) || r.delivery.advances() != 1 {
			t.Fatalf("repeated release = %+v after %d advance calls, want the stored answer", again, r.delivery.advances())
		}
		r.noSecret()
	})

	t.Run("superseded with the immutable channel", func(t *testing.T) {
		r := newPubRig(t, nil).start().opened()
		r.delivery.set(func(d *fakeDelivery) { d.advance, d.advanceHead = false, pubNewerHead })
		release := r.defaultRelease()
		payload, requestBytes := release.payload(t)
		answer := mustReleased(t, r.release(payload), requestBytes)
		ref := released(t)
		want := map[string]*distribution.ChannelHead{pubCanary: {Ref: ref, Generation: 3}, pubImmutable: {Ref: ref, Generation: 1}}
		if answer.Outcome != distribution.ReleaseOutcomeReleased || !reflect.DeepEqual(answer.Current, want) {
			t.Fatalf("release = %+v, want released with the immutable channel moved", answer)
		}
		if channels := r.releaseChannels(0, release.request); !reflect.DeepEqual(channels, release.request.Channels[1:]) {
			t.Fatalf("put-server released channels %+v, want the immutable channel alone", channels)
		}
		if head := r.put.head(pubCanary); head.Ref != r.baseRef {
			t.Fatalf("canary head = %+v, want the base left in place", head)
		}
		if head := r.put.head(pubImmutable); head == nil || head.Ref != ref {
			t.Fatalf("immutable head = %+v, want the set", head)
		}
		r.noSecret()
	})

	t.Run("rerun of an older commit", func(t *testing.T) {
		r := newPubRig(t, nil).start()
		newer := r.put.move(pubCanary, pubRestatedSet("b"))
		release := r.layout([]string{pubCanary}, "", []string{pubCanary})
		if *release.request.Channels[0].Expected != newer {
			t.Fatalf("expected head = %+v, want the newer head the session resolved", release.request.Channels[0].Expected)
		}
		r.delivery.set(func(d *fakeDelivery) { d.advance = false })
		payload, requestBytes := release.payload(t)
		answer := mustReleased(t, r.release(payload), requestBytes)
		want := map[string]*distribution.ChannelHead{pubCanary: {Ref: released(t), Generation: 4}}
		if answer.Outcome != distribution.ReleaseOutcomeAlreadyCurrent || !reflect.DeepEqual(answer.Current, want) {
			t.Fatalf("release = %+v, want already-current at the newer head's generation", answer)
		}
		if head := r.put.head(pubCanary); head.Ref != newer {
			t.Fatalf("canary head = %+v, want the newer head left in place", head)
		}
		if !strings.Contains(r.h.stderr.String(), "Delivery names no newer head") {
			t.Fatalf("stderr = %q, want the missing head named", r.h.stderr.String())
		}
	})

	t.Run("superseded while another channel moved", func(t *testing.T) {
		r := newPubRig(t, nil).start()
		release := r.layout([]string{pubCanary, pubPreview}, "", []string{pubCanary})
		moved := r.put.move(pubPreview, pubOtherSet())
		r.delivery.set(func(d *fakeDelivery) { d.advance, d.advanceHead = false, pubNewerHead })
		payload, requestBytes := release.payload(t)
		answer := mustReleased(t, r.release(payload), requestBytes)
		want := map[string]*distribution.ChannelHead{pubCanary: {Ref: r.baseRef, Generation: 3}, pubPreview: {Ref: moved, Generation: 1}}
		if answer.Outcome != distribution.ReleaseOutcomeConflict || !reflect.DeepEqual(answer.Current, want) {
			t.Fatalf("release = %+v, want a conflict naming the moved channel and the canary as expected", answer)
		}
		if !strings.Contains(r.h.stderr.String(), "(conflict)") {
			t.Fatalf("stderr = %q, want the conflict reported", r.h.stderr.String())
		}
	})

	t.Run("superseded while put-server does not answer", func(t *testing.T) {
		r := newPubRig(t, nil).start().opened()
		r.delivery.set(func(d *fakeDelivery) { d.advance, d.advanceHead = false, pubNewerHead })
		r.put.set(func(p *fakePutServer) {
			p.onRelease = func(call int, w http.ResponseWriter, _ *http.Request) bool {
				if call <= 2 {
					w.WriteHeader(http.StatusBadGateway)
					return true
				}
				return false
			}
		})
		payload, requestBytes := r.defaultRelease().payload(t)
		refusal := mustRefusal(t, r.release(payload), refusalPublicationUnavailable)
		if !strings.Contains(refusal.Message, "outcome of plan") {
			t.Fatalf("refusal = %q, want an unknown outcome", refusal.Message)
		}
		// An unknown outcome stores nothing: the release is decided again.
		if answer := mustReleased(t, r.release(payload), requestBytes); answer.Outcome != distribution.ReleaseOutcomeReleased || r.delivery.advances() != 2 {
			t.Fatalf("release = %+v after %d advance calls, want released on the second decision", answer, r.delivery.advances())
		}
	})
}

// TestPublicationReleaseMovesTheForwardOnlyChannelsOfTheHeadRun pins the
// second half of D18: a run Delivery says is the head of its default branch
// moves its forward-only channels from the heads put-server holds now. A
// head that moves again before the move is read once more; a second miss is
// the conflict.
func TestPublicationReleaseMovesTheForwardOnlyChannelsOfTheHeadRun(t *testing.T) {
	t.Run("from the head the channel moved to", func(t *testing.T) {
		r := newPubRig(t, nil).start().opened()
		restated := r.put.move(pubCanary, pubRestatedSet("b"))
		release := r.defaultRelease()
		payload, requestBytes := release.payload(t)
		answer := mustReleased(t, r.release(payload), requestBytes)
		ref := pubRef(t, pubReleasedSet())
		want := map[string]*distribution.ChannelHead{pubCanary: {Ref: ref, Generation: 5}, pubImmutable: {Ref: ref, Generation: 1}}
		if answer.Outcome != distribution.ReleaseOutcomeReleased || !reflect.DeepEqual(answer.Current, want) {
			t.Fatalf("release = %+v, want released from the moved head", answer)
		}
		channels := r.releaseChannels(0, release.request)
		if len(channels) != 2 || channels[0].Expected == nil || *channels[0].Expected != restated || !reflect.DeepEqual(channels[1], release.request.Channels[1]) {
			t.Fatalf("put-server released channels %+v, want the canary expected at the moved head and the immutable channel as sent", channels)
		}
	})

	t.Run("a miss then the move", func(t *testing.T) {
		r := newPubRig(t, nil).start().opened()
		restated := pubRef(t, pubRestatedSet("b"))
		r.put.set(func(p *fakePutServer) {
			p.onRelease = func(call int, _ http.ResponseWriter, _ *http.Request) bool {
				if call == 1 {
					r.put.move(pubCanary, pubRestatedSet("b"))
				}
				return false
			}
		})
		release := r.defaultRelease()
		payload, requestBytes := release.payload(t)
		if answer := mustReleased(t, r.release(payload), requestBytes); answer.Outcome != distribution.ReleaseOutcomeReleased {
			t.Fatalf("release = %+v, want released on the second attempt", answer)
		}
		if _, releases := r.put.calls(); releases != 2 || r.delivery.advances() != 2 {
			t.Fatalf("put-server releases = %d, advance calls = %d, want 2 and 2", releases, r.delivery.advances())
		}
		if channels := r.releaseChannels(1, release.request); channels[0].Expected == nil || *channels[0].Expected != restated {
			t.Fatalf("second put-server release channels %+v, want the canary expected at the head read again", channels)
		}
		if !strings.Contains(r.h.stderr.String(), "reading it again") {
			t.Fatalf("stderr = %q, want the second read reported", r.h.stderr.String())
		}
	})

	t.Run("a second miss is a conflict", func(t *testing.T) {
		r := newPubRig(t, nil).start().opened()
		fills := []string{"b", "c", "d"}
		r.put.set(func(p *fakePutServer) {
			p.onRelease = func(call int, _ http.ResponseWriter, _ *http.Request) bool {
				r.put.move(pubCanary, pubRestatedSet(fills[call-1]))
				return false
			}
		})
		payload, requestBytes := r.defaultRelease().payload(t)
		answer := mustReleased(t, r.release(payload), requestBytes)
		if answer.Outcome != distribution.ReleaseOutcomeConflict || answer.Current[pubCanary].Ref != pubRef(t, pubRestatedSet("c")) {
			t.Fatalf("release = %+v, want a conflict naming the head of the second miss", answer)
		}
		resolves, releases := r.put.calls()
		if resolves != 3 || releases != 2 || r.delivery.advances() != 2 {
			t.Fatalf("put-server resolves = %d, releases = %d, advance calls = %d, want 3, 2 and 2", resolves, releases, r.delivery.advances())
		}
		if stderr := r.h.stderr.String(); !strings.Contains(stderr, "moved again") || !strings.Contains(stderr, "(conflict)") {
			t.Fatalf("stderr = %q, want the second miss reported as the conflict", stderr)
		}
		if again := mustReleased(t, r.release(payload), requestBytes); !reflect.DeepEqual(again, answer) {
			t.Fatalf("repeated release = %+v, want the stored conflict", again)
		}
		if _, releases := r.put.calls(); releases != 2 {
			t.Fatalf("put-server releases = %d, want the stored conflict", releases)
		}
	})

	t.Run("a miss on another channel is not read again", func(t *testing.T) {
		r := newPubRig(t, nil).start()
		release := r.layout([]string{pubCanary, pubPreview}, "", []string{pubCanary})
		moved := r.put.move(pubPreview, pubOtherSet())
		payload, requestBytes := release.payload(t)
		answer := mustReleased(t, r.release(payload), requestBytes)
		if answer.Outcome != distribution.ReleaseOutcomeConflict || answer.Current[pubPreview].Ref != moved {
			t.Fatalf("release = %+v, want a conflict naming the moved channel", answer)
		}
		if _, releases := r.put.calls(); releases != 1 || r.delivery.advances() != 1 {
			t.Fatalf("put-server releases = %d, advance calls = %d, want 1 and 1", releases, r.delivery.advances())
		}
	})

	t.Run("superseded between the attempts", func(t *testing.T) {
		r := newPubRig(t, nil).start().opened()
		var restated distribution.ReleaseSetRef
		r.put.set(func(p *fakePutServer) {
			p.onRelease = func(call int, _ http.ResponseWriter, _ *http.Request) bool {
				if call == 1 {
					restated = r.put.move(pubCanary, pubRestatedSet("b"))
				}
				return false
			}
		})
		r.delivery.set(func(d *fakeDelivery) {
			d.onAdvance = func(call int, w http.ResponseWriter) bool {
				if call == 1 {
					return false
				}
				writeJSON(w, http.StatusOK, map[string]any{"protocolVersion": 1, "advance": false, "head": pubNewerHead})
				return true
			}
		})
		payload, requestBytes := r.defaultRelease().payload(t)
		answer := mustReleased(t, r.release(payload), requestBytes)
		ref := pubRef(t, pubReleasedSet())
		want := map[string]*distribution.ChannelHead{pubCanary: {Ref: ref, Generation: 3}, pubImmutable: {Ref: ref, Generation: 1}}
		if answer.Outcome != distribution.ReleaseOutcomeReleased || !reflect.DeepEqual(answer.Current, want) {
			t.Fatalf("release = %+v, want the immutable channel released and the canary left", answer)
		}
		if head := r.put.head(pubCanary); head.Ref != restated {
			t.Fatalf("canary head = %+v, want the newer run's head left in place", head)
		}
	})
}

// TestPublicationReleaseMovesNoChannelWithoutDeliveryAdvance pins the failure
// answers before a forward-only move: without Delivery's advance answer, or
// without a fresh read of the heads, no channel moves. Only a refusal Delivery
// or put-server decided is the plan's answer; anything else lets the engine
// send the release again.
func TestPublicationReleaseMovesNoChannelWithoutDeliveryAdvance(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(r *pubRig)
		refusal    string
		definitive bool
		advances   int
	}{
		{name: "Delivery unavailable", refusal: refusalPublicationUnavailable, advances: 2, setup: func(r *pubRig) {
			r.delivery.set(func(d *fakeDelivery) {
				d.onAdvance = func(_ int, w http.ResponseWriter) bool { w.WriteHeader(http.StatusServiceUnavailable); return true }
			})
		}},
		{name: "Delivery answers outside its contract", refusal: refusalPublicationUnavailable, advances: 1, setup: func(r *pubRig) {
			r.delivery.set(func(d *fakeDelivery) {
				d.onAdvance = func(_ int, w http.ResponseWriter) bool {
					writeJSON(w, http.StatusOK, map[string]any{"protocolVersion": 1})
					return true
				}
			})
		}},
		{name: "Delivery answers no advance", refusal: refusalPublicationUnavailable, advances: 1, setup: func(r *pubRig) {
			r.delivery.set(func(d *fakeDelivery) {
				d.onAdvance = func(_ int, w http.ResponseWriter) bool { w.WriteHeader(http.StatusNoContent); return true }
			})
		}},
		{name: "the run ended", refusal: refusalPublicationRefused, definitive: true, advances: 1, setup: func(r *pubRig) {
			r.delivery.set(func(d *fakeDelivery) {
				d.onAdvance = func(_ int, w http.ResponseWriter) bool {
					writeJSON(w, http.StatusConflict, map[string]string{"error": "the run has reached a terminal state", "code": "run_terminal"})
					return true
				}
			})
		}},
		{name: "Delivery without the advance route", refusal: refusalPublicationRefused, definitive: true, advances: 1, setup: func(r *pubRig) {
			r.delivery.set(func(d *fakeDelivery) {
				d.onAdvance = func(_ int, w http.ResponseWriter) bool { w.WriteHeader(http.StatusNotFound); return true }
			})
		}},
		{name: "put-server does not answer the read", refusal: refusalPublicationUnavailable, advances: 1, setup: func(r *pubRig) {
			r.put.set(func(p *fakePutServer) {
				p.onResolve = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
					w.WriteHeader(http.StatusBadGateway)
					return true
				}
			})
		}},
		{name: "Delivery refuses the planning renewal", refusal: refusalPublicationRefused, definitive: true, advances: 1, setup: func(r *pubRig) {
			r.clock.Advance(275 * time.Second)
			r.delivery.set(func(d *fakeDelivery) {
				d.onPlanning = func(_ int, w http.ResponseWriter) bool {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "no publication for this run"})
					return true
				}
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newPubRig(t, nil).start().opened()
			tc.setup(r)
			payload, requestBytes := r.defaultRelease().payload(t)
			refusal := mustRefusal(t, r.release(payload), tc.refusal)
			if !strings.Contains(refusal.Message, "no channel moved") {
				t.Fatalf("refusal = %q, want it to say no channel moved", refusal.Message)
			}
			if _, releases := r.put.calls(); releases != 0 || r.delivery.advances() != tc.advances {
				t.Fatalf("put-server releases = %d, advance calls = %d, want none and %d", releases, r.delivery.advances(), tc.advances)
			}
			r.noSecret(refusal.Message)
			r.delivery.set(func(d *fakeDelivery) { d.onAdvance, d.onPlanning = nil, nil })
			r.put.set(func(p *fakePutServer) { p.onResolve = nil })
			if tc.definitive {
				mustRefusal(t, r.release(payload), tc.refusal)
				if r.delivery.advances() != tc.advances {
					t.Fatalf("advance calls = %d, want the stored refusal", r.delivery.advances())
				}
				return
			}
			mustReleased(t, r.release(payload), requestBytes)
		})
	}
}

func TestParseAdvanceAnswer(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		advance bool
		head    string
		valid   bool
	}{
		{name: "head", body: `{"protocolVersion":1,"advance":true}`, advance: true, valid: true},
		{name: "superseded", body: `{"protocolVersion":1,"advance":false,"head":"` + pubNewerHead + `"}`, head: pubNewerHead, valid: true},
		{name: "no newer head", body: `{"protocolVersion":1,"advance":false}`, valid: true},
		{name: "SHA-256 head", body: `{"protocolVersion":1,"advance":false,"head":"` + strings.Repeat("e", 64) + `"}`, head: strings.Repeat("e", 64), valid: true},
		{name: "unknown member", body: `{"protocolVersion":1,"advance":true,"later":1}`, advance: true, valid: true},
		{name: "another version", body: `{"protocolVersion":2,"advance":true}`},
		{name: "no advance", body: `{"protocolVersion":1}`},
		{name: "advance not a boolean", body: `{"protocolVersion":1,"advance":"yes"}`},
		{name: "short head", body: `{"protocolVersion":1,"advance":false,"head":"abc"}`},
		{name: "uppercase head", body: `{"protocolVersion":1,"advance":false,"head":"` + strings.Repeat("D", 40) + `"}`},
		{name: "a head with advance", body: `{"protocolVersion":1,"advance":true,"head":"` + pubNewerHead + `"}`},
		{name: "not an object", body: `[]`},
	}
	for _, tc := range cases {
		advance, head, err := parseAdvanceAnswer([]byte(tc.body))
		if (err == nil) != tc.valid || (tc.valid && (advance != tc.advance || head != tc.head)) {
			t.Errorf("%s: parseAdvanceAnswer = %v, %q, %v", tc.name, advance, head, err)
		}
	}
}

func TestPreservesHead(t *testing.T) {
	plan := pubPlan(t)
	set := pubReleasedSet()
	head := func(edit func(set *distribution.ReleaseSet)) *distribution.ChannelHead {
		base := pubBaseSet()
		if edit != nil {
			edit(&base)
		}
		return &distribution.ChannelHead{Ref: pubRef(t, base), Generation: 2, ReleaseSet: &base}
	}
	cases := []struct {
		name string
		head *distribution.ChannelHead
		want bool
	}{
		{name: "no head", want: true},
		{name: "the base", head: head(nil), want: true},
		{name: "the set itself", head: &distribution.ChannelHead{Ref: pubRef(t, set), Generation: 2, ReleaseSet: &set}, want: true},
		{name: "a rebuilt member at another artifact", head: head(func(s *distribution.ReleaseSet) {
			s.Members[2] = pubMember("put", "acme/app", "1.2.9", pubOtherSource, "e", "f")
		}), want: true},
		{name: "a carried member stamped with a project", head: head(func(s *distribution.ReleaseSet) {
			s.Members[0].Project, s.Members[0].Kind = "libs/lib", distribution.KindLibrary
		}), want: true},
		{name: "a carried member at another version", head: head(func(s *distribution.ReleaseSet) { s.Members[0].Version = "v1.0.9" })},
		{name: "a member the set does not carry", head: head(func(s *distribution.ReleaseSet) {
			s.Members = append(s.Members, pubMember("go", "go.acme.dev/tool", "v0.1.0", pubOtherSource, "e", "f"))
		})},
		{name: "a carried member the head dropped", head: head(func(s *distribution.ReleaseSet) { s.Members = s.Members[1:] })},
		{name: "a head without its members", head: &distribution.ChannelHead{Ref: pubRef(t, pubBaseSet()), Generation: 2}},
	}
	for _, tc := range cases {
		if got := preservesHead(tc.head, set, plan); got != tc.want {
			t.Errorf("%s: preservesHead = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWithChannelsKeepsEveryOtherMember(t *testing.T) {
	request := pubReleaseRequest(nil)
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := withChannels(body, request.Channels[1:])
	if err != nil {
		t.Fatal(err)
	}
	var got distribution.ReleaseRequest
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	want := request
	want.Channels = request.Channels[1:]
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("withChannels = %+v, want %+v", got, want)
	}
	if _, err := withChannels([]byte(`[]`), nil); err == nil {
		t.Fatal("withChannels accepted a body that is not an object")
	}
}

// TestPublicationReleaseOfChannelsThatAreNotForwardOnlyAsksNoAdvance pins the
// scope of D18: a run Delivery gives no forward-only channel, a pull-request
// or a tag run, releases exactly as the engine sent it, without asking
// Delivery or reading the heads again, and a moved head is put-server's
// conflict.
func TestPublicationReleaseOfChannelsThatAreNotForwardOnlyAsksNoAdvance(t *testing.T) {
	t.Run("released verbatim", func(t *testing.T) {
		r := newPubRig(t, nil).start()
		release := r.layout([]string{pubCanary, pubImmutable}, pubImmutable, nil)
		payload, requestBytes := release.payload(t)
		if answer := mustReleased(t, r.release(payload), requestBytes); answer.Outcome != distribution.ReleaseOutcomeReleased {
			t.Fatalf("release = %+v, want released", answer)
		}
		r.put.set(func(p *fakePutServer) {
			if len(p.releaseBodies) != 1 || string(p.releaseBodies[0]) != string(requestBytes) || len(p.resolveBodies) != 1 {
				t.Fatalf("put-server resolves %d, releases %s, want the session's resolve and the engine's bytes", len(p.resolveBodies), p.releaseBodies)
			}
		})
		if r.delivery.advances() != 0 {
			t.Fatalf("advance calls = %d, want none", r.delivery.advances())
		}
	})

	t.Run("a moved head is a conflict", func(t *testing.T) {
		r := newPubRig(t, nil).start()
		release := r.layout([]string{pubCanary, pubImmutable}, pubImmutable, nil)
		moved := r.put.move(pubCanary, pubRestatedSet("b"))
		payload, requestBytes := release.payload(t)
		answer := mustReleased(t, r.release(payload), requestBytes)
		if answer.Outcome != distribution.ReleaseOutcomeConflict || answer.Current[pubCanary].Ref != moved {
			t.Fatalf("release = %+v, want a conflict naming the moved head", answer)
		}
		if _, releases := r.put.calls(); releases != 1 || r.delivery.advances() != 0 {
			t.Fatalf("put-server releases = %d, advance calls = %d, want 1 and none", releases, r.delivery.advances())
		}
		if !strings.Contains(r.h.stderr.String(), "(conflict)") {
			t.Fatalf("stderr = %q, want the conflict reported", r.h.stderr.String())
		}
	})
}

// TestPublicationReleaseAnswersTheEngineInItsOwnTerms pins the binding of a
// forward-only answer to the engine's request, which compared against older
// heads than the ones the provider moved from.
func TestPublicationReleaseAnswersTheEngineInItsOwnTerms(t *testing.T) {
	t.Run("superseded on a channel without a head", func(t *testing.T) {
		r := newPubRig(t, nil).start()
		release := r.layout([]string{pubPreview}, "", []string{pubPreview})
		// No head to carry a member from: the set is the planned members.
		release.request.ReleaseSet.Members = release.request.ReleaseSet.Members[1:]
		r.delivery.set(func(d *fakeDelivery) { d.advance = false })
		payload, requestBytes := release.payload(t)
		answer := mustReleased(t, r.release(payload), requestBytes)
		want := map[string]*distribution.ChannelHead{pubPreview: {Ref: pubRef(t, release.request.ReleaseSet), Generation: 1}}
		if answer.Outcome != distribution.ReleaseOutcomeAlreadyCurrent || !reflect.DeepEqual(answer.Current, want) {
			t.Fatalf("release = %+v, want already-current at generation 1", answer)
		}
		if head := r.put.head(pubPreview); head != nil {
			t.Fatalf("preview head = %+v, want none", head)
		}
	})

	t.Run("released where the engine expected the set", func(t *testing.T) {
		r := newPubRig(t, nil).start()
		r.put.move(pubCanary, pubReleasedSet())
		release := r.layout([]string{pubCanary}, "", []string{pubCanary})
		// Another run moved the canary off the set; this run moves it back.
		r.put.move(pubCanary, pubRestatedSet("b"))
		payload, requestBytes := release.payload(t)
		answer := mustReleased(t, r.release(payload), requestBytes)
		if answer.Outcome != distribution.ReleaseOutcomeAlreadyCurrent || answer.Current[pubCanary].Generation != 6 {
			t.Fatalf("release = %+v, want already-current: the engine expected the set on every channel", answer)
		}
	})

	t.Run("a conflict the engine cannot read is an unknown outcome", func(t *testing.T) {
		r := newPubRig(t, nil).start().opened()
		r.put.set(func(p *fakePutServer) {
			p.onRelease = func(call int, _ http.ResponseWriter, _ *http.Request) bool {
				switch call {
				case 1:
					r.put.move(pubCanary, pubRestatedSet("b"))
				case 2:
					// Back at the head the engine expected: a conflict
					// against the head this provider read is none against
					// the engine's.
					r.put.move(pubCanary, pubBaseSet())
				}
				return false
			}
		})
		payload, requestBytes := r.defaultRelease().payload(t)
		refusal := mustRefusal(t, r.release(payload), refusalPublicationUnavailable)
		if !strings.Contains(refusal.Message, "outcome of plan") {
			t.Fatalf("refusal = %q, want an unknown outcome", refusal.Message)
		}
		if answer := mustReleased(t, r.release(payload), requestBytes); answer.Outcome != distribution.ReleaseOutcomeReleased {
			t.Fatalf("release = %+v, want released once the heads hold", answer)
		}
	})
}

// TestPublicationHoldsAnImmutableChannelThatAlreadyHasAHead pins D19: a run
// whose immutable channel an earlier run already released is a rerun. It opens
// its plan, uploads its artifacts and ends green, and its release moves no
// channel, mutable ones included: put-server and Delivery's advance route are
// not asked, and the engine reads already-current with the set as the head of
// every channel, the held one at the generation it holds and every other one
// at the generation the session resolved.
func TestPublicationHoldsAnImmutableChannelThatAlreadyHasAHead(t *testing.T) {
	released := func(t *testing.T) distribution.ReleaseSetRef { return pubRef(t, pubReleasedSet()) }
	// tagged is a run whose tag's channel an earlier run released.
	tagged := func(t *testing.T) (*pubRig, distribution.ReleaseSetRef) {
		r := newPubRig(t, nil).start()
		held := r.put.move(pubImmutable, pubOtherSet())
		r.put.set(func(p *fakePutServer) { p.immutable[pubImmutable] = true })
		return r, held
	}
	stillHeld := func(t *testing.T, r *pubRig, held distribution.ReleaseSetRef) {
		t.Helper()
		if head := r.put.head(pubImmutable); head == nil || head.Ref != held || head.Generation != 1 {
			t.Fatalf("immutable head = %+v, want %s left in place", head, held.ID)
		}
		stderr := r.h.stderr.String()
		if countLines(stderr, "immutable channel v1.2.3 already has head "+held.ID) != 2 {
			t.Fatalf("stderr = %q, want the held head named at open and at release", stderr)
		}
		r.noSecret()
	}

	for _, tc := range []struct {
		name    string
		advance bool
	}{
		{name: "a rerun that is not the head leaves every channel"},
		{name: "a rerun of the head commit leaves every channel too", advance: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, held := tagged(t)
			r.opened()
			// Another run moved the canary since; Delivery's answer does not
			// matter to a rerun.
			restated := r.put.move(pubCanary, pubRestatedSet("b"))
			r.delivery.set(func(d *fakeDelivery) { d.advance = tc.advance })
			payload, requestBytes := r.defaultRelease().payload(t)
			answer := mustReleased(t, r.release(payload), requestBytes)
			ref := released(t)
			want := map[string]*distribution.ChannelHead{pubCanary: {Ref: ref, Generation: 3}, pubImmutable: {Ref: ref, Generation: 1}}
			if answer.Outcome != distribution.ReleaseOutcomeAlreadyCurrent || !reflect.DeepEqual(answer.Current, want) {
				t.Fatalf("release = %+v, want already-current at the set on both channels", answer)
			}
			if _, releases := r.put.calls(); releases != 0 || r.delivery.advances() != 0 {
				t.Fatalf("put-server releases = %d, advance calls = %d, want none", releases, r.delivery.advances())
			}
			if head := r.put.head(pubCanary); head.Ref != restated {
				t.Fatalf("canary head = %+v, want the other run's head left in place", head)
			}
			if !strings.Contains(r.h.stderr.String(), "this run is a rerun and channels [canary v1.2.3] stay where they are") {
				t.Fatalf("stderr = %q, want every channel named as staying", r.h.stderr.String())
			}
			// The answer is the plan's: the same release gets it again.
			if again := mustReleased(t, r.release(payload), requestBytes); !reflect.DeepEqual(again, answer) {
				t.Fatalf("repeated release = %+v, want the stored answer", again)
			}
			stillHeld(t, r, held)
		})
	}

	t.Run("a tag run the engine resolved nothing for", func(t *testing.T) {
		r, held := tagged(t)
		release := r.tagLayout([]string{pubImmutable}, pubImmutable, nil)
		resolves, _ := r.put.calls()
		var read distribution.ResolveRequest
		r.put.set(func(p *fakePutServer) {
			if err := json.Unmarshal(p.resolveBodies[0], &read); err != nil {
				t.Fatalf("resolve body: %v", err)
			}
		})
		if resolves != 1 || !reflect.DeepEqual(read.Channels, []string{pubImmutable}) {
			t.Fatalf("put-server resolves = %d of %v, want the open's one read of the immutable channel", resolves, read.Channels)
		}
		payload, requestBytes := release.payload(t)
		answer := mustReleased(t, r.release(payload), requestBytes)
		want := map[string]*distribution.ChannelHead{pubImmutable: {Ref: pubRef(t, release.request.ReleaseSet), Generation: 1}}
		if answer.Outcome != distribution.ReleaseOutcomeAlreadyCurrent || !reflect.DeepEqual(answer.Current, want) {
			t.Fatalf("release = %+v, want already-current at the set", answer)
		}
		if _, releases := r.put.calls(); releases != 0 || r.delivery.advances() != 0 {
			t.Fatalf("put-server releases = %d, advance calls = %d, want none", releases, r.delivery.advances())
		}
		stillHeld(t, r, held)
	})

	// The v1.0 tag rerun after the v1.1 tag run moved stable: stable is not
	// forward-only on a tag run, and the engine expects the v1.1 head it
	// resolved, so a compare-and-set would move stable back to v1.0.
	t.Run("a tag rerun leaves its other channels where they are", func(t *testing.T) {
		r, held := tagged(t)
		newer := r.put.move(pubStable, pubRestatedSet("b"))
		release := r.layout([]string{pubStable, pubImmutable}, pubImmutable, nil)
		if expected := release.request.Channels[0].Expected; expected == nil || *expected != newer {
			t.Fatalf("stable expected head = %+v, want the newer tag's set the session resolved", expected)
		}
		payload, requestBytes := release.payload(t)
		answer := mustReleased(t, r.release(payload), requestBytes)
		ref := released(t)
		want := map[string]*distribution.ChannelHead{pubStable: {Ref: ref, Generation: 1}, pubImmutable: {Ref: ref, Generation: 1}}
		if answer.Outcome != distribution.ReleaseOutcomeAlreadyCurrent || !reflect.DeepEqual(answer.Current, want) {
			t.Fatalf("release = %+v, want already-current at the set on both channels", answer)
		}
		if _, releases := r.put.calls(); releases != 0 || r.delivery.advances() != 0 {
			t.Fatalf("put-server releases = %d, advance calls = %d, want none", releases, r.delivery.advances())
		}
		if head := r.put.head(pubStable); head == nil || head.Ref != newer || head.Generation != 1 {
			t.Fatalf("stable head = %+v, want the newer tag's set left in place", head)
		}
		stillHeld(t, r, held)
	})

	t.Run("a plan that names an immutable channel as mutable is refused", func(t *testing.T) {
		r, held := tagged(t)
		release := r.layout([]string{pubCanary, pubImmutable}, "", []string{pubCanary})
		if release.request.Channels[1].Immutable || release.request.Channels[1].Expected == nil {
			t.Fatalf("release channels = %+v, want the tag's channel named mutable at its head", release.request.Channels)
		}
		payload, _ := release.payload(t)
		mustRefusal(t, r.release(payload), registry.RefusalChannelImmutable)
		if head := r.put.head(pubImmutable); head == nil || head.Ref != held {
			t.Fatalf("immutable head = %+v, want %s left in place", head, held.ID)
		}
		if head := r.put.head(pubCanary); head.Ref != r.baseRef {
			t.Fatalf("canary head = %+v, want the base left in place", head)
		}
		if strings.Contains(r.h.stderr.String(), "already has head") {
			t.Fatalf("stderr = %q, want no held channel: the plan names no immutable channel", r.h.stderr.String())
		}
	})

	t.Run("a channel the run created is not held", func(t *testing.T) {
		r := newPubRig(t, nil).start().opened()
		payload, requestBytes := r.defaultRelease().payload(t)
		if answer := mustReleased(t, r.release(payload), requestBytes); answer.Outcome != distribution.ReleaseOutcomeReleased {
			t.Fatalf("release = %+v, want released", answer)
		}
		if head := r.put.head(pubImmutable); head == nil || head.Ref != released(t) {
			t.Fatalf("immutable head = %+v, want the set", head)
		}
		if strings.Contains(r.h.stderr.String(), "already has head") {
			t.Fatalf("stderr = %q, want no held channel", r.h.stderr.String())
		}
	})
}

// TestPublicationReleaseRaceOnTheImmutableChannelIsARerun pins D19 under a
// race: a concurrent run of the same tag creates the immutable channel between
// this run's open and its release, and put-server refuses the release with
// channel_immutable. The provider reads the channel once more; when it now has
// a head, the run is a rerun and ends green with no channel moved. Otherwise
// the refusal stands.
func TestPublicationReleaseRaceOnTheImmutableChannelIsARerun(t *testing.T) {
	// raced makes put-server's first release find the immutable channel
	// another run created with pubOtherSet.
	raced := func(r *pubRig) {
		r.put.set(func(p *fakePutServer) {
			p.onRelease = func(call int, _ http.ResponseWriter, _ *http.Request) bool {
				if call == 1 {
					r.put.move(pubImmutable, pubOtherSet())
					r.put.set(func(p *fakePutServer) { p.immutable[pubImmutable] = true })
				}
				return false
			}
		})
	}

	t.Run("the channel now has a head", func(t *testing.T) {
		r := newPubRig(t, nil).start().opened()
		raced(r)
		other := pubRef(t, pubOtherSet())
		payload, requestBytes := r.defaultRelease().payload(t)
		answer := mustReleased(t, r.release(payload), requestBytes)
		ref := pubRef(t, pubReleasedSet())
		want := map[string]*distribution.ChannelHead{pubCanary: {Ref: ref, Generation: 3}, pubImmutable: {Ref: ref, Generation: 1}}
		if answer.Outcome != distribution.ReleaseOutcomeAlreadyCurrent || !reflect.DeepEqual(answer.Current, want) {
			t.Fatalf("release = %+v, want already-current at the set on both channels", answer)
		}
		if head := r.put.head(pubCanary); head.Ref != r.baseRef {
			t.Fatalf("canary head = %+v, want the base left in place", head)
		}
		if head := r.put.head(pubImmutable); head == nil || head.Ref != other {
			t.Fatalf("immutable head = %+v, want the other run's set", head)
		}
		resolves, releases := r.put.calls()
		if resolves != 3 || releases != 1 {
			t.Fatalf("put-server resolves = %d, releases = %d, want 3 (the session's, the forward-only read, the immutable read) and 1", resolves, releases)
		}
		if stderr := r.h.stderr.String(); !strings.Contains(stderr, "immutable channel v1.2.3 now has head "+other.ID+"; this run is a rerun") {
			t.Fatalf("stderr = %q, want the concurrent head named", stderr)
		}
		if again := mustReleased(t, r.release(payload), requestBytes); !reflect.DeepEqual(again, answer) {
			t.Fatalf("repeated release = %+v, want the stored answer", again)
		}
		if _, releases := r.put.calls(); releases != 1 {
			t.Fatalf("put-server releases = %d, want the stored answer", releases)
		}
		r.noSecret()
	})

	t.Run("a tag run whose channel now has a head", func(t *testing.T) {
		r := newPubRig(t, nil).start()
		release := r.tagLayout([]string{pubPreview, pubImmutable}, pubImmutable, nil)
		raced(r)
		payload, requestBytes := release.payload(t)
		answer := mustReleased(t, r.release(payload), requestBytes)
		ref := pubRef(t, release.request.ReleaseSet)
		want := map[string]*distribution.ChannelHead{pubPreview: {Ref: ref, Generation: 1}, pubImmutable: {Ref: ref, Generation: 1}}
		if answer.Outcome != distribution.ReleaseOutcomeAlreadyCurrent || !reflect.DeepEqual(answer.Current, want) {
			t.Fatalf("release = %+v, want already-current at the set on both channels", answer)
		}
		if head := r.put.head(pubPreview); head != nil {
			t.Fatalf("preview head = %+v, want none", head)
		}
	})

	t.Run("the channel still has no head", func(t *testing.T) {
		r := newPubRig(t, nil).start().opened()
		r.put.set(func(p *fakePutServer) {
			p.onRelease = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
				putError(w, http.StatusConflict, "registry.release_set.channel_immutable", "immutable channel already has a head")
				return true
			}
		})
		payload, _ := r.defaultRelease().payload(t)
		mustRefusal(t, r.release(payload), registry.RefusalChannelImmutable)
		if resolves, releases := r.put.calls(); resolves != 3 || releases != 1 {
			t.Fatalf("put-server resolves = %d, releases = %d, want the immutable channel read once more and 1", resolves, releases)
		}
		if strings.Contains(r.h.stderr.String(), "this run is a rerun") {
			t.Fatalf("stderr = %q, want no rerun", r.h.stderr.String())
		}
	})

	t.Run("the channel cannot be read", func(t *testing.T) {
		r := newPubRig(t, nil).start().opened()
		raced(r)
		r.put.set(func(p *fakePutServer) {
			p.onResolve = func(call int, w http.ResponseWriter, _ *http.Request) bool {
				if call >= 3 {
					w.WriteHeader(http.StatusBadGateway)
					return true
				}
				return false
			}
		})
		payload, _ := r.defaultRelease().payload(t)
		mustRefusal(t, r.release(payload), registry.RefusalChannelImmutable)
		if !strings.Contains(r.h.stderr.String(), "immutable channel v1.2.3 cannot be read again") {
			t.Fatalf("stderr = %q, want the failed read reported", r.h.stderr.String())
		}
	})

	t.Run("a plan without an immutable channel is not read again", func(t *testing.T) {
		r := newPubRig(t, nil).start()
		release := r.layout([]string{pubCanary}, "", []string{pubCanary})
		r.put.set(func(p *fakePutServer) {
			p.onRelease = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
				putError(w, http.StatusConflict, "registry.release_set.channel_immutable", "the channel is immutable")
				return true
			}
		})
		payload, _ := release.payload(t)
		mustRefusal(t, r.release(payload), registry.RefusalChannelImmutable)
		if resolves, _ := r.put.calls(); resolves != 2 {
			t.Fatalf("put-server resolves = %d, want the session's and the forward-only read", resolves)
		}
	})
}
