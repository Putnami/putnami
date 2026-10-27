package credentialprovider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	distribution "go.putnami.dev/protocol/distribution"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	registry "go.putnami.dev/protocol/registry"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/publicationoutbox"
	"go.putnami.dev/sdk/extension/publicationupload/publicationuploadtest"
	"go.putnami.dev/tooling/cli/internal/credentialprovider/providertest"
)

const (
	planRevision    = "8d5edb7513d93b9165ba2a7cb48466d022fc3f63"
	planOtherCommit = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	planArtifact    = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	planFingerprint = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	publishFixture  = "publish-bearer-fixture"
	publishHost     = "registry.example.test"
	runFixture      = "run-credential-fixture"
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

// npmRegistry starts a fake npm registry that answers publishFixture alone,
// and a provider that holds publishFixture for its host.
func npmRegistry(t *testing.T, config providertest.Config) (*publicationuploadtest.NPMRegistry, *providertest.Provider) {
	t.Helper()
	reg := publicationuploadtest.NewNPMRegistry(t, publishFixture)
	target, err := url.Parse(reg.Server.URL)
	if err != nil {
		t.Fatal(err)
	}
	config.Bearer, config.Hosts = publishFixture, []string{target.Host}
	return reg, providertest.New(config)
}

// testUpload packs the npm member of open's plan into a new outbox and
// returns its upload to registryURL, the registry the member names.
func testUpload(t *testing.T, open *registry.OpenParams, registryURL string) *registry.UploadParams {
	t.Helper()
	planned := open.Plan.Members[0]
	writer, err := publicationoutbox.NewWriter(filepath.Join(t.TempDir(), "outbox"))
	if err != nil {
		t.Fatal(err)
	}
	tarball, err := writer.WriteFile("npm/core.tgz", []byte("tarball of "+planned.Coordinate+"@"+planned.Version))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := writer.WriteFile("npm/core.json", []byte(fmt.Sprintf(`{"name":%q,"version":%q}`, planned.Coordinate, planned.Version)))
	if err != nil {
		t.Fatal(err)
	}
	member := extproto.OutboxMember{
		Ecosystem: extproto.OutboxEcosystemNPM, Coordinate: planned.Coordinate, Version: planned.Version, Project: "typescript/core",
		NPM: &extproto.OutboxNPM{Registry: registryURL, Tarball: tarball, Manifest: manifest},
	}
	if err := writer.Add(member); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	return &registry.UploadParams{PlanDigest: open.Plan.PlanDigest, Registry: registryURL, Outbox: writer.Root(), Member: member}
}

func refusalCode(err error) string {
	var refusal *PublicationRefusalError
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	return ""
}

// The engine offers credential-v1, publication-v1 and uploader-v1, and the
// provider's echo alone decides: a provider that echoes credential-v1 only
// serves a session without publication-v1, whose publication surface is
// absent and whose publication ops are never sent.
func TestSessionOffersPublicationV1AndAcceptsAV1OnlyEcho(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "registry/credential-seam-contract", "publication-v1-is-negotiated", "the-cli-offers-every-capability-and-follows-the-echo")
	offered := []string{registry.CapabilityCredentialV1, registry.CapabilityPublicationV1, registry.CapabilityUploaderV1}

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
// an upload is served only once a plan is open.
func TestSessionRefusesASecondDifferentPlan(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "registry/credential-seam-contract", "open-is-idempotent-and-release-final", "the-cli-surfaces-plan-already-open-for-a-second-plan")
	ctx := context.Background()
	reg, provider := npmRegistry(t, providertest.Config{})
	first := testOpenParams(t, "0.3.0", []string{"canary"}, nil)

	early, earlyPublication := publicationBroker(t, provider)
	if _, err := earlyPublication.Upload(ctx, testUpload(t, first, reg.Server.URL)); refusalCode(err) != registry.RefusalPlanNotOpen {
		t.Fatalf("upload before open: %v, want %s", err, registry.RefusalPlanNotOpen)
	}
	_ = early.Close()

	_, publication := publicationBroker(t, provider)
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
	upload := testUpload(t, first, reg.Server.URL)
	if result, err := publication.Upload(ctx, upload); err != nil || result.Digest != upload.Member.NPM.Tarball.Digest {
		t.Fatalf("upload after open: %+v, %v, want the packed tarball digest", result, err)
	}
}

// The engine sends each upload to the session and asks no publish credential:
// the provider uploads with the credential it holds, answers the stored
// digest, and refuses a registry it does not serve without ending the
// session. A refusal is an answer and is not retried.
func TestAnUploadGoesToTheSessionAndTheEngineAsksNoCredential(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/provider-publication", "uploads-go-to-the-credential-provider", "the-engine-asks-no-publish-credential")
	ctx := context.Background()
	reg, provider := npmRegistry(t, providertest.Config{})
	_, publication := publicationBroker(t, provider)
	open := testOpenParams(t, "0.3.0", []string{"canary"}, nil)
	if err := publication.Open(ctx, open); err != nil {
		t.Fatal(err)
	}

	upload := testUpload(t, open, reg.Server.URL)
	result, err := publication.Upload(ctx, upload)
	if err != nil || result.Digest != upload.Member.NPM.Tarball.Digest || result.Platforms != nil {
		t.Fatalf("upload: %+v, %v, want the packed tarball digest and no platforms", result, err)
	}
	if stored, _ := reg.Stored("@putnami/core", "0.3.0"); string(stored) != "tarball of @putnami/core@0.3.0" {
		t.Fatalf("the registry stored %q, want the packed tarball", stored)
	}
	uploads := provider.Uploads()
	if len(uploads) != 1 || uploads[0].Params.PlanDigest != open.Plan.PlanDigest || uploads[0].Params.Registry != reg.Server.URL {
		t.Fatalf("the provider uploaded %+v, want one upload of the open plan to %s", uploads, reg.Server.URL)
	}

	elsewhere := testUpload(t, open, "https://elsewhere.example.test")
	_, err = publication.Upload(ctx, elsewhere)
	if refusalCode(err) != registry.RefusalRegistryNotServed || strings.Contains(err.Error(), publishFixture) {
		t.Fatalf("upload to a registry the provider does not serve: %v, want %s without the credential", err, registry.RefusalRegistryNotServed)
	}
	if calls := provider.CallsOf(registry.CredentialOpUpload); len(calls) != 2 {
		t.Fatalf("the provider read %d upload requests, want 2 and no retry of the refusal", len(calls))
	}
	if _, err := publication.Resolve(ctx, &distribution.ResolveRequest{
		ProtocolVersion: distribution.ProtocolVersion, Namespace: "putnami", Channels: []string{"canary"},
	}); err != nil {
		t.Fatalf("a refused upload ended the session: %v", err)
	}
	if calls := provider.CallsOf(registry.CredentialOpCredential); len(calls) != 0 {
		t.Fatalf("the engine asked the provider %d credentials, want none", len(calls))
	}
}

// An upload the provider applied and left unanswered may have reached the
// registry: it is not sent again, the session ends, and every later op fails.
func TestAnUnansweredUploadEndsTheSession(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/provider-publication", "uploads-go-to-the-credential-provider", "an-unanswered-upload-ends-the-session")
	ctx := context.Background()
	// The provider finishes applying the upload only once the engine gave up
	// on it, as a loaded machine can order them: the engine's bound fires
	// before the provider records the request.
	gaveUp := make(chan struct{})
	reg, provider := npmRegistry(t, providertest.Config{
		Unanswered: map[registry.CredentialOp]int{registry.CredentialOpUpload: 1},
		UploadAnswer: func(_ registry.UploadParams, answer registry.UploadResult) registry.UploadResult {
			<-gaveUp
			return answer
		},
	})
	const timeout = 300 * time.Millisecond
	_, publication := publicationBroker(t, provider, WithUploadTimeout(timeout))
	release := sync.OnceFunc(func() { close(gaveUp) })
	t.Cleanup(release)
	open := testOpenParams(t, "0.3.0", []string{"canary"}, nil)
	if err := publication.Open(ctx, open); err != nil {
		t.Fatal(err)
	}
	_, err := publication.Upload(ctx, testUpload(t, open, reg.Server.URL))
	if err == nil || !strings.Contains(err.Error(), "did not answer upload within "+timeout.String()) {
		t.Fatalf("unanswered upload: %v, want an error naming the bound", err)
	}
	if _, err := publication.Upload(ctx, testUpload(t, open, reg.Server.URL)); err == nil {
		t.Fatal("a session that left an upload unanswered serves another upload")
	}
	if _, err := publication.Release(ctx, testReleaseParams(open)); err == nil {
		t.Fatal("a session that left an upload unanswered serves a release")
	}

	release()
	recorded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if calls := provider.AwaitCalls(recorded, registry.CredentialOpUpload, 1); len(calls) != 1 || calls[0].Answered {
		t.Fatalf("upload requests %+v, want one unanswered request and no retry", calls)
	}
	if got := len(provider.CallsOf(registry.CredentialOpUpload)) + len(provider.CallsOf(registry.CredentialOpRelease)); got != 1 {
		t.Fatalf("the provider read %d upload and release requests, want only the first upload", got)
	}
}

// One upload is bounded by the base timeout plus one minute per whole 256 MiB
// its member declares. An OCI member declares no file and gets the base.
func TestAnUploadIsBoundedByItsDeclaredSize(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/provider-publication", "uploads-go-to-the-credential-provider", "an-upload-is-bounded-by-its-declared-size")
	const mib = int64(1) << 20
	file := func(size int64) extproto.OutboxFile { return extproto.OutboxFile{Size: size} }
	blobs := make([]extproto.OutboxPutBlob, 32)
	for index := range blobs {
		blobs[index].Size = 512 * mib
	}
	for _, tc := range []struct {
		name   string
		member extproto.OutboxMember
		want   time.Duration
	}{
		{"an OCI layout", extproto.OutboxMember{OCI: &extproto.OutboxOCI{}}, DefaultUploadTimeout},
		{"a small npm tarball", extproto.OutboxMember{NPM: &extproto.OutboxNPM{Tarball: file(4 << 10), Manifest: file(512)}}, DefaultUploadTimeout},
		{"npm files just under 256 MiB", extproto.OutboxMember{NPM: &extproto.OutboxNPM{Tarball: file(256*mib - 513), Manifest: file(512)}}, DefaultUploadTimeout},
		{"npm files of 256 MiB", extproto.OutboxMember{NPM: &extproto.OutboxNPM{Tarball: file(256*mib - 512), Manifest: file(512)}}, DefaultUploadTimeout + time.Minute},
		{"a 500 MiB Go module", extproto.OutboxMember{Go: &extproto.OutboxGo{Zip: file(500 * mib), Mod: file(64), Info: file(64)}}, DefaultUploadTimeout + time.Minute},
		{"32 put blobs of 512 MiB", extproto.OutboxMember{Put: &extproto.OutboxPut{Manifest: file(4 << 10), Blobs: blobs}}, DefaultUploadTimeout + 64*time.Minute},
	} {
		if got := uploadTimeout(DefaultUploadTimeout, tc.member); got != tc.want {
			t.Errorf("%s: bound %s, want %s", tc.name, got, tc.want)
		}
	}
	if DefaultUploadTimeout != 10*time.Minute {
		t.Errorf("DefaultUploadTimeout = %s, want 10m", DefaultUploadTimeout)
	}
}

// A session that echoes publication-v1 without uploader-v1 cannot publish:
// this CLI sends every upload to the session. The broker refuses it at start,
// before the first hook, when the run is hosted or enables the publish
// purpose, with an invalid-configuration error that names the extension to
// upgrade. Any other broker, and a session that echoes uploader-v1, start.
func TestASessionWithoutUploaderV1FailsBeforeTheFirstHook(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/provider-publication", "uploads-go-to-the-credential-provider", "a-session-without-uploader-v1-fails-before-the-first-hook")
	ctx := context.Background()
	noUploader := []string{registry.CapabilityCredentialV1, registry.CapabilityPublicationV1}
	start := func(echo []string, purposes []string, options ...Option) (*Broker, error) {
		provider := providertest.New(providertest.Config{Echo: echo})
		broker := NewBroker(purposes, providerOpener(provider), options...)
		broker.extensionName = "@putnami/cloud"
		t.Cleanup(func() { _ = broker.Close() })
		return broker, broker.Start(ctx)
	}

	for _, tc := range []struct {
		name     string
		purposes []string
		options  []Option
	}{
		{"a publish broker", []string{registry.PurposePublish}, nil},
		{"a hosted install broker", []string{registry.PurposeRead}, []Option{WithRunCredential(runFixture)}},
	} {
		broker, err := start(noUploader, tc.purposes, tc.options...)
		var required *UploaderRequiredError
		if !errors.As(err, &required) || !errors.Is(err, protocolcli.ErrInvalidConfig) {
			t.Fatalf("%s: start = %v, want an invalid-configuration *UploaderRequiredError", tc.name, err)
		}
		want := "the credential provider of @putnami/cloud serves publication-v1 without uploader-v1, and this CLI sends every publication upload to its credential provider: upgrade @putnami/cloud to a version that serves uploader-v1"
		if err.Error() != want || strings.Contains(err.Error(), runFixture) {
			t.Fatalf("%s: error %q, want %q", tc.name, err, want)
		}
		if surface, err := broker.Publication(ctx); surface != nil || (slices.Contains(tc.purposes, registry.PurposePublish) && !errors.As(err, &required)) {
			t.Fatalf("%s: publication = %v, %v, want no surface and the same refusal", tc.name, surface, err)
		}
	}

	if _, err := start(noUploader, []string{registry.PurposeRead}); err != nil {
		t.Fatalf("a local install broker: start = %v, want a session it never publishes through", err)
	}
	all := []string{registry.CapabilityCredentialV1, registry.CapabilityPublicationV1, registry.CapabilityUploaderV1}
	if _, err := start(all, []string{registry.PurposePublish}, WithRunCredential(runFixture)); err != nil {
		t.Fatalf("a hosted publish broker over uploader-v1: start = %v", err)
	}
	if _, err := start([]string{registry.CapabilityCredentialV1}, []string{registry.PurposePublish}, WithRunCredential(runFixture)); err != nil {
		t.Fatalf("a hosted publish broker over credential-v1 alone: start = %v, want the legacy path", err)
	}
	unnamed := (&UploaderRequiredError{}).Error()
	if !strings.Contains(unnamed, "upgrade the extension that declares it") {
		t.Fatalf("an unnamed provider's refusal %q does not say what to upgrade", unnamed)
	}
}

// lostAnswerTimeout bounds each release exchange in the lost-answer tests. The
// same bound times out the lost answer and waits for the answered repeat, so it
// must cover the provider's real answer on a loaded machine: a 300ms bound
// failed the answered repeat under load.
const lostAnswerTimeout = 5 * time.Second

// A release whose answer is lost is sent once more on the same session; the
// provider applies the plan digest once and answers the repeat with its first
// answer. When the repeat is lost too, the outcome is unknown: the error names
// the plan digest and the session ends.
func TestReleaseAfterAnUncertainSubmitIsResolvedByPlanDigest(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "registry/credential-seam-contract", "open-is-idempotent-and-release-final", "the-cli-sends-a-lost-release-once-more")
	ctx := context.Background()

	provider := providertest.New(providertest.Config{Unanswered: map[registry.CredentialOp]int{registry.CredentialOpRelease: 1}})
	_, publication := publicationBroker(t, provider, WithReleaseTimeout(lostAnswerTimeout))
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
	_, publication = publicationBroker(t, lost, WithReleaseTimeout(lostAnswerTimeout))
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
	if _, err := publication.Upload(ctx, testUpload(t, open, "https://"+publishHost)); err == nil || len(lost.CallsOf(registry.CredentialOpUpload)) != 0 {
		t.Fatal("a session that left a release unanswered still serves an upload")
	}
}

// A plan whose channel head is not the source revision or one of its
// ancestors is refused at open, before any member is uploaded.
func TestSessionRefusesANotForwardOpen(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "registry/credential-seam-contract", "nothing-moves-before-an-open", "the-cli-refuses-uploads-after-a-not-forward-open")
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
	if _, err := publication.Upload(context.Background(), testUpload(t, open, "https://"+publishHost)); refusalCode(err) != registry.RefusalPlanNotOpen {
		t.Fatalf("upload after a refused open: %v, want %s", err, registry.RefusalPlanNotOpen)
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
