package deliverycli

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	distribution "go.putnami.dev/protocol/distribution"
)

// pubStaging is the staging channel Delivery names to a staged run.
const pubStaging = "ci-candidate"

// stagedLayout opens a staged default-branch run: canary, which has a head,
// and preview, which has none, both forward-only, and Delivery's staging
// channel.
func (r *pubRig) stagedLayout() pubRelease {
	r.t.Helper()
	r.delivery.set(func(d *fakeDelivery) { d.staging = pubStaging })
	return r.layout([]string{pubCanary, pubPreview}, "", []string{pubCanary, pubPreview})
}

// TestStagedRunReleasesOntoTheStagingChannelOnly: a staged run's release puts
// its set on the staging channel and moves none of the run's channels, which
// Delivery moves in the deploy phase after a green gate. Delivery hears the
// set and the head each channel had when the set was cut; the engine reads the
// set as the head of every channel, as for a skipped forward-only channel.
func TestStagedRunReleasesOntoTheStagingChannelOnly(t *testing.T) {
	r := newPubRig(t, nil).start()
	release := r.stagedLayout()
	// The staging channel confers the widest level of the run's channels.
	release.request.Channels[1].Visibility = distribution.VisibilityPublic
	payload, requestBytes := release.payload(t)
	answer := mustReleased(t, r.release(payload), requestBytes)

	ref := pubRef(t, pubReleasedSet())
	want := map[string]*distribution.ChannelHead{pubCanary: {Ref: ref, Generation: 3}, pubPreview: {Ref: ref, Generation: 1}}
	if answer.Outcome != distribution.ReleaseOutcomeReleased || !reflect.DeepEqual(answer.Current, want) {
		t.Fatalf("release = %+v, want released with the set as every channel's head", answer)
	}
	channels := r.releaseChannels(0, release.request)
	wantChannels := []distribution.ChannelRequest{{Name: pubStaging, Visibility: distribution.VisibilityPublic}}
	if !reflect.DeepEqual(channels, wantChannels) {
		t.Fatalf("put-server released channels %+v, want the staging channel alone", channels)
	}
	if head := r.put.head(pubCanary); head.Ref != r.baseRef || head.Generation != 3 {
		t.Fatalf("canary head = %+v, want the base left in place", head)
	}
	if head := r.put.head(pubPreview); head != nil {
		t.Fatalf("preview head = %+v, want none", head)
	}
	if head := r.put.head(pubStaging); head == nil || head.Ref != ref {
		t.Fatalf("staging head = %+v, want the run's set", head)
	}
	var question []stagedAdvanceRequest
	r.delivery.set(func(d *fakeDelivery) { question = d.advanceBodies })
	wantQuestion := []stagedAdvanceRequest{{ReleaseSet: ref.ID, Expected: map[string]string{pubCanary: r.baseRef.ID, pubPreview: ""}}}
	if !reflect.DeepEqual(question, wantQuestion) {
		t.Fatalf("advance questions = %+v, want the set and the heads it was cut from", question)
	}
	if !strings.Contains(r.h.stderr.String(), "is on staging channel ci-candidate; channels [canary preview] advance in the deploy phase after a green gate") {
		t.Fatalf("stderr = %q, want the staged release line", r.h.stderr.String())
	}
	// The answer is the plan's: the same release gets it again.
	if again := mustReleased(t, r.release(payload), requestBytes); !reflect.DeepEqual(again, answer) || r.delivery.advances() != 1 {
		t.Fatalf("repeated release = %+v after %d advance calls, want the stored answer", again, r.delivery.advances())
	}
	if _, releases := r.put.calls(); releases != 1 {
		t.Fatalf("put-server releases = %d, want 1", releases)
	}
	r.noSecret()
}

// TestOvertakenStagedRunReleasesNothing: a staged run that is no longer the
// head of its default branch leaves the staging channel to the newer run and
// ends green, as an unstaged superseded run does.
func TestOvertakenStagedRunReleasesNothing(t *testing.T) {
	r := newPubRig(t, nil).start()
	release := r.stagedLayout()
	r.delivery.set(func(d *fakeDelivery) { d.advance, d.advanceHead = false, pubNewerHead })
	payload, requestBytes := release.payload(t)
	answer := mustReleased(t, r.release(payload), requestBytes)
	if answer.Outcome != distribution.ReleaseOutcomeAlreadyCurrent {
		t.Fatalf("release = %+v, want already-current", answer)
	}
	if _, releases := r.put.calls(); releases != 0 {
		t.Fatalf("put-server releases = %d, want none", releases)
	}
	if head := r.put.head(pubStaging); head != nil {
		t.Fatalf("staging head = %+v, want none", head)
	}
	stderr := r.h.stderr.String()
	if countLines(stderr, "is not the head of its default branch") != 1 || !strings.Contains(stderr, "[canary preview]") {
		t.Fatalf("stderr = %q, want one line naming the channels left in place", stderr)
	}
}

// TestStagedReleaseFollowsTheStagingHead: any run of the namespace may move
// the staging channel. The release expects the head it reads just before, and
// reads it once more when that head moved before the release; a second miss
// moves nothing.
func TestStagedReleaseFollowsTheStagingHead(t *testing.T) {
	t.Run("an earlier run's head", func(t *testing.T) {
		r := newPubRig(t, nil).start()
		earlier := r.put.move(pubStaging, pubOtherSet())
		release := r.stagedLayout()
		payload, requestBytes := release.payload(t)
		mustReleased(t, r.release(payload), requestBytes)
		channels := r.releaseChannels(0, release.request)
		if len(channels) != 1 || channels[0].Expected == nil || *channels[0].Expected != earlier {
			t.Fatalf("put-server released channels %+v, want the staging channel from %s", channels, earlier.ID)
		}
	})

	t.Run("moved once", func(t *testing.T) {
		r := newPubRig(t, nil).start()
		release := r.stagedLayout()
		var moved distribution.ReleaseSetRef
		r.put.set(func(p *fakePutServer) {
			p.onRelease = func(call int, _ http.ResponseWriter, _ *http.Request) bool {
				if call == 1 {
					moved = r.put.move(pubStaging, pubOtherSet())
				}
				return false
			}
		})
		payload, requestBytes := release.payload(t)
		mustReleased(t, r.release(payload), requestBytes)
		channels := r.releaseChannels(1, release.request)
		if channels[0].Expected == nil || *channels[0].Expected != moved {
			t.Fatalf("second release channels %+v, want the staging channel from the moved head", channels)
		}
		if head := r.put.head(pubStaging); head.Ref != pubRef(t, pubReleasedSet()) {
			t.Fatalf("staging head = %+v, want the run's set", head)
		}
		if countLines(r.h.stderr.String(), "staging channel ci-candidate moved after it was read; reading it again") != 1 {
			t.Fatalf("stderr = %q, want the retry line", r.h.stderr.String())
		}
	})

	t.Run("moved twice", func(t *testing.T) {
		r := newPubRig(t, nil).start()
		release := r.stagedLayout()
		r.put.set(func(p *fakePutServer) {
			p.onRelease = func(call int, _ http.ResponseWriter, _ *http.Request) bool {
				set := pubOtherSet()
				set.Members[0].Version = "v1.0." + string(rune('0'+call))
				r.put.move(pubStaging, set)
				return false
			}
		})
		payload, _ := release.payload(t)
		response := r.release(payload)
		if response.OK || response.Error == nil || response.Error.Code != refusalPublicationUnavailable ||
			!strings.Contains(response.Error.Message, "no channel moved") {
			t.Fatalf("release = %+v, want publication_unavailable with no channel moved", response)
		}
		if _, releases := r.put.calls(); releases != 2 {
			t.Fatalf("put-server releases = %d, want 2", releases)
		}
		if head := r.put.head(pubCanary); head.Ref != r.baseRef {
			t.Fatalf("canary head = %+v, want the base left in place", head)
		}
	})
}

// TestUnstagedRunSendsAnEmptyAdvanceQuestion: a run Delivery does not stage
// moves its channels itself and asks the advance with {}, as before.
func TestUnstagedRunSendsAnEmptyAdvanceQuestion(t *testing.T) {
	r := newPubRig(t, nil).start()
	release := r.layout([]string{pubCanary}, "", []string{pubCanary})
	payload, requestBytes := release.payload(t)
	mustReleased(t, r.release(payload), requestBytes)
	if head := r.put.head(pubCanary); head.Ref != pubRef(t, pubReleasedSet()) {
		t.Fatalf("canary head = %+v, want the run's set", head)
	}
	if head := r.put.head(pubStaging); head != nil {
		t.Fatalf("staging head = %+v, want none", head)
	}
}

func TestParsePlanningGrantStagingChannel(t *testing.T) {
	issuedAt := time.Unix(1_800_000_000, 0)
	valid := func() map[string]any {
		return map[string]any{
			"protocolVersion": 1, "workspaceId": "ws", "runId": "run", "sourceRevision": pubSource, "namespace": pubNamespace,
			"channels": []string{pubCanary, pubPreview}, "forwardOnlyChannels": []string{pubCanary, pubPreview},
			"stagingChannel": pubStaging, "expiresUnix": issuedAt.Unix() + 3600, "access_token": "planning-bearer",
			"token_type": "Bearer", "expires_in": 120, "scope": "read", "jti": "jti",
		}
	}
	body, _ := json.Marshal(valid())
	authority, _, err := parsePlanningGrant(body, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	if authority.StagingChannel != pubStaging || authority.equal(publicationAuthority{
		WorkspaceID: "ws", RunID: "run", SourceRevision: pubSource, Namespace: pubNamespace,
		Channels: []string{pubCanary, pubPreview}, ForwardOnly: []string{pubCanary, pubPreview},
	}) {
		t.Fatalf("authority = %+v, want the staging channel kept and compared", authority)
	}
	for name, edit := range map[string]func(map[string]any){
		"invalid name":        func(m map[string]any) { m["stagingChannel"] = "Ci Candidate" },
		"one of the channels": func(m map[string]any) { m["stagingChannel"] = pubCanary },
		"not all forward-only": func(m map[string]any) {
			m["forwardOnlyChannels"] = []string{pubCanary}
		},
		"a tag release": func(m map[string]any) {
			m["channels"] = []string{pubCanary, pubImmutable}
			m["immutableChannel"] = pubImmutable
			m["forwardOnlyChannels"] = []string{pubCanary}
		},
	} {
		grant := valid()
		edit(grant)
		body, _ := json.Marshal(grant)
		if _, _, err := parsePlanningGrant(body, issuedAt); err == nil {
			t.Errorf("%s: parsePlanningGrant accepted %s", name, body)
		}
	}
}
