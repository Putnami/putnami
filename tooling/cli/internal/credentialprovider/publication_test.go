package credentialprovider

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
	registry "go.putnami.dev/protocol/registry"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/credentialprovider/providertest"
)

const (
	planRevision    = "8d5edb7513d93b9165ba2a7cb48466d022fc3f63"
	planOtherCommit = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	planArtifact    = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	planFingerprint = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	publishFixture  = "publish-bearer-fixture"
	publishHost     = "registry.example.test"
)

// providerOpener serves provider over in-process pipes, one session per open.
func providerOpener(provider *providertest.Provider) Opener {
	return func(ctx context.Context) (*Session, error) {
		session := Connect(provider.Pipes())
		if _, err := session.Initialize(ctx, ""); err != nil {
			_ = session.Close()
			return nil, err
		}
		return session, nil
	}
}

// publicationBroker returns a publish broker over provider and its
// publication surface, which is nil when the provider did not echo
// publication-v1.
func publicationBroker(t *testing.T, provider *providertest.Provider, options ...Option) (*Broker, *Publication) {
	t.Helper()
	broker := NewBroker([]string{registry.PurposePublish}, providerOpener(provider), options...)
	t.Cleanup(func() { _ = broker.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	publication, err := broker.Publication(ctx)
	if err != nil {
		t.Fatalf("publication: %v", err)
	}
	return broker, publication
}

// testOpenParams is one plan of one npm member advancing channels, whose heads
// heads names: a channel absent from heads has no head.
func testOpenParams(t *testing.T, version string, channels []string, heads map[string]string) *registry.OpenParams {
	t.Helper()
	plan := registry.PublicationPlan{
		ProtocolVersion: registry.PublicationPlanProtocolVersion,
		Namespace:       "putnami",
		SourceRevision:  planRevision,
		Channels:        channels,
		Members: []registry.PublicationPlanMember{{
			Ecosystem:            distribution.Ecosystem("npm"),
			Coordinate:           "@putnami/core",
			Version:              version,
			SourceRevision:       planRevision,
			SelectionFingerprint: planFingerprint,
		}},
	}
	digest, err := registry.PlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	plan.PlanDigest = digest
	if err := registry.ValidatePublicationPlan(plan); err != nil {
		t.Fatal(err)
	}
	return &registry.OpenParams{Plan: plan, Ancestry: testAncestry(channels, heads)}
}

func testAncestry(channels []string, heads map[string]string) registry.PublicationAncestry {
	ancestry := registry.PublicationAncestry{SourceRevision: planRevision, SnapshotCommits: 12}
	for _, channel := range channels {
		head := heads[channel]
		ancestry.Channels = append(ancestry.Channels, registry.PublicationChannelAncestry{
			Name:               channel,
			HeadSourceRevision: head,
			Ancestor:           head == planRevision,
		})
	}
	return ancestry
}

// testReleaseParams releases open's plan onto channels without a head.
func testReleaseParams(open *registry.OpenParams) *registry.ReleaseParams {
	member := open.Plan.Members[0]
	request := distribution.ReleaseRequest{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       open.Plan.Namespace,
		ReleaseSet: distribution.ReleaseSet{
			ProtocolVersion: distribution.ProtocolVersion,
			Namespace:       open.Plan.Namespace,
			Members: []distribution.ReleaseSetMember{{
				Ecosystem:            member.Ecosystem,
				Coordinate:           member.Coordinate,
				Version:              member.Version,
				ArtifactDigest:       planArtifact,
				Dependencies:         []distribution.ReleaseSetDependency{},
				SourceRevision:       member.SourceRevision,
				SelectionFingerprint: member.SelectionFingerprint,
			}},
		},
		Visibility: distribution.VisibilityChain{Repo: distribution.VisibilityPrivate},
	}
	for _, channel := range open.Plan.Channels {
		request.Channels = append(request.Channels, distribution.ChannelRequest{Name: channel, Visibility: distribution.VisibilityPublic})
	}
	return &registry.ReleaseParams{
		PlanDigest: open.Plan.PlanDigest,
		Request:    request,
		Ancestry:   open.Ancestry,
		Evidence: registry.PublicationEvidence{
			Images: []runtimeproto.ReleaseSetPublishedImage{},
			Members: []registry.PublicationMemberEvidence{{
				Project: "typescript/core", Ecosystem: string(member.Ecosystem), Coordinate: member.Coordinate,
				Version: member.Version, Digest: planArtifact,
				Publisher: "@putnami/typescript", Command: "publish", Step: "publish",
			}},
		},
	}
}

func refusalCode(err error) string {
	var refusal *PublicationRefusalError
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	return ""
}

// The engine offers credential-v1 and publication-v1, and the provider's echo
// alone decides: a provider that echoes credential-v1 only serves a session
// without publication-v1, whose publication surface is absent and whose
// publication ops are never sent.
func TestSessionOffersPublicationV1AndAcceptsAV1OnlyEcho(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/provider-publication", "open-credential-release-through-one-provider", "the-session-offers-publication-v1-and-the-echo-decides")
	offered := []string{registry.CapabilityCredentialV1, registry.CapabilityPublicationV1}

	both := providertest.New(providertest.Config{})
	if _, publication := publicationBroker(t, both); publication == nil {
		t.Fatal("a provider that echoes publication-v1 serves no publication surface")
	}
	if got := both.Offered(); len(got) != 1 || !slices.Equal(got[0], offered) {
		t.Fatalf("offered %v, want one initialize offering %v", got, offered)
	}

	v1Only := providertest.New(providertest.Config{Echo: []string{registry.CapabilityCredentialV1}})
	_, publication := publicationBroker(t, v1Only)
	if publication != nil {
		t.Fatal("a credential-v1-only echo serves a publication surface")
	}
	if got := v1Only.Offered(); len(got) != 1 || !slices.Equal(got[0], offered) {
		t.Fatalf("offered %v, want one initialize offering %v", got, offered)
	}
	session, err := providerOpener(v1Only)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	if session.Publication() {
		t.Fatal("a credential-v1-only echo negotiated publication-v1")
	}
	if err := session.Open(context.Background(), testOpenParams(t, "0.3.0", []string{"canary"}, nil)); err == nil || !strings.Contains(err.Error(), "did not negotiate") {
		t.Fatalf("open without publication-v1: %v, want a not-negotiated error", err)
	}
	if calls := v1Only.CallsOf(registry.CredentialOpOpen); len(calls) != 0 {
		t.Fatalf("the provider read %d open requests from a session without publication-v1", len(calls))
	}
}

// One session holds one plan: the same plan opens again with the first
// answer, another plan is refused with a bounded error naming the code, and
// the publish credential is issued only once a plan is open.
func TestSessionRefusesASecondDifferentPlan(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/provider-publication", "open-credential-release-through-one-provider", "a-second-plan-is-refused-in-one-session")
	ctx := context.Background()
	provider := providertest.New(providertest.Config{Bearer: publishFixture, Hosts: []string{publishHost}})
	target := mustURL(t, "https://"+publishHost+"/@putnami/core")

	early, earlyPublication := publicationBroker(t, provider)
	if _, err := earlyPublication.PublishBearer(ctx, target); refusalCodeOf(err) != registry.RefusalPlanNotOpen {
		t.Fatalf("publish credential before open: %v, want %s", err, registry.RefusalPlanNotOpen)
	}
	_ = early.Close()

	_, publication := publicationBroker(t, provider)
	first := testOpenParams(t, "0.3.0", []string{"canary"}, nil)
	if err := publication.Open(ctx, first); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := publication.Open(ctx, first); err != nil {
		t.Fatalf("the same plan again: %v", err)
	}
	second := testOpenParams(t, "0.4.0", []string{"canary"}, nil)
	err := publication.Open(ctx, second)
	if refusalCode(err) != registry.RefusalPlanAlreadyOpen {
		t.Fatalf("another plan: %v, want %s", err, registry.RefusalPlanAlreadyOpen)
	}
	if !strings.Contains(err.Error(), second.Plan.PlanDigest) || len(err.Error()) > 1024 {
		t.Fatalf("the refusal %q does not name the refused plan in a bounded message", err)
	}
	if opens := provider.Opens(); len(opens) != 1 || opens[0].Plan.PlanDigest != first.Plan.PlanDigest {
		t.Fatalf("opened %d plans, want only the first", len(opens))
	}
	bearer, err := publication.PublishBearer(ctx, target)
	if err != nil || bearer != publishFixture {
		t.Fatalf("publish credential after open: served %t, %v", bearer == publishFixture, err)
	}
	if _, err := publication.PublishBearer(ctx, mustURL(t, "https://elsewhere.example.test/x")); err == nil {
		t.Fatal("the publish credential was handed to a host it does not serve")
	}
}

// refusalCodeOf names the code of a credential or publication refusal.
func refusalCodeOf(err error) string {
	var credential *RefusalError
	if errors.As(err, &credential) {
		return credential.Code
	}
	return refusalCode(err)
}

// A release whose answer is lost is sent once more on the same session; the
// provider applies the plan digest once and answers the repeat with its first
// answer. When the repeat is lost too, the outcome is unknown: the error names
// the plan digest and the session ends.
func TestReleaseAfterAnUncertainSubmitIsResolvedByPlanDigest(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/provider-publication", "open-credential-release-through-one-provider", "an-uncertain-release-is-resolved-by-plan-digest")
	ctx := context.Background()

	provider := providertest.New(providertest.Config{Unanswered: map[registry.CredentialOp]int{registry.CredentialOpRelease: 1}})
	_, publication := publicationBroker(t, provider, WithReleaseTimeout(300*time.Millisecond))
	open := testOpenParams(t, "0.3.0", []string{"canary"}, nil)
	if err := publication.Open(ctx, open); err != nil {
		t.Fatal(err)
	}
	response, err := publication.Release(ctx, testReleaseParams(open))
	if err != nil || response.Outcome != distribution.ReleaseOutcomeReleased {
		t.Fatalf("release after a lost answer: %+v, %v", response, err)
	}
	if calls := provider.CallsOf(registry.CredentialOpRelease); len(calls) != 2 || provider.Releases() != 1 {
		t.Fatalf("%d release requests applied %d times, want 2 requests applied once", len(calls), provider.Releases())
	}
	if _, moved := provider.Head("putnami", "canary"); !moved {
		t.Fatal("the released channel has no head")
	}

	lost := providertest.New(providertest.Config{Unanswered: map[registry.CredentialOp]int{registry.CredentialOpRelease: 2}})
	_, publication = publicationBroker(t, lost, WithReleaseTimeout(300*time.Millisecond))
	open = testOpenParams(t, "0.3.0", []string{"canary"}, nil)
	if err := publication.Open(ctx, open); err != nil {
		t.Fatal(err)
	}
	_, err = publication.Release(ctx, testReleaseParams(open))
	if err == nil || !strings.Contains(err.Error(), "the outcome is unknown") || !strings.Contains(err.Error(), open.Plan.PlanDigest) {
		t.Fatalf("release with both answers lost: %v, want an unknown outcome naming %s", err, open.Plan.PlanDigest)
	}
	if lost.Releases() != 1 {
		t.Fatalf("applied %d releases, want 1", lost.Releases())
	}
	if _, err := publication.PublishBearer(ctx, mustURL(t, "https://"+publishHost+"/x")); err == nil {
		t.Fatal("a session that left a release unanswered still serves the publish credential")
	}
}

// A plan whose channel head is not the source revision or one of its
// ancestors is refused at open, before any credential is issued.
func TestSessionRefusesANotForwardOpen(t *testing.T) {
	t.Parallel()
	provider := providertest.New(providertest.Config{Bearer: publishFixture, Hosts: []string{publishHost}})
	_, publication := publicationBroker(t, provider)
	open := testOpenParams(t, "0.3.0", []string{"canary"}, map[string]string{"canary": planOtherCommit})
	err := publication.Open(context.Background(), open)
	if refusalCode(err) != registry.RefusalNotForward || !strings.Contains(err.Error(), open.Plan.PlanDigest) {
		t.Fatalf("open of a plan behind its channel: %v, want %s naming the plan", err, registry.RefusalNotForward)
	}
	if len(provider.Opens()) != 0 {
		t.Fatal("the provider opened a plan that is not forward")
	}
	if _, err := publication.PublishBearer(context.Background(), mustURL(t, "https://"+publishHost+"/x")); err == nil {
		t.Fatal("a refused open still served the publish credential")
	}
}

// A refused release is an answer: it is not retried, and it names the code.
func TestARefusedReleaseIsNotRetried(t *testing.T) {
	t.Parallel()
	provider := providertest.New(providertest.Config{Stored: func(string, string, string) (string, bool) { return "", false }})
	_, publication := publicationBroker(t, provider)
	open := testOpenParams(t, "0.3.0", []string{"canary"}, nil)
	if err := publication.Open(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	_, err := publication.Release(context.Background(), testReleaseParams(open))
	if refusalCode(err) != registry.RefusalArtifactMissing || !strings.Contains(err.Error(), open.Plan.PlanDigest) {
		t.Fatalf("release of a member with no artifact: %v, want %s", err, registry.RefusalArtifactMissing)
	}
	if calls := provider.CallsOf(registry.CredentialOpRelease); len(calls) != 1 {
		t.Fatalf("a refused release was sent %d times", len(calls))
	}
	if _, moved := provider.Head("putnami", "canary"); moved {
		t.Fatal("a refused release moved a channel")
	}
}

// Resolve goes to the session and answers each channel's head.
func TestPublicationResolveAnswersTheSessionHeads(t *testing.T) {
	t.Parallel()
	provider := providertest.New(providertest.Config{})
	_, publication := publicationBroker(t, provider)
	response, err := publication.Resolve(context.Background(), &distribution.ResolveRequest{
		ProtocolVersion: distribution.ProtocolVersion, Namespace: "putnami", Channels: []string{"canary"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if head, listed := response.Heads["canary"]; !listed || head != nil {
		t.Fatalf("heads %v, want canary listed without a head", response.Heads)
	}
	if len(provider.CallsOf(registry.CredentialOpResolve)) != 1 {
		t.Fatal("resolve did not reach the provider session")
	}
}
