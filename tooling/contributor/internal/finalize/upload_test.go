package finalize

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"

	"go.putnami.dev/tooling/contributor/internal/screens"
)

const finalizeUploads = "finalize-uploads-screenshots"

// twoWebProjects touches one page of apps/admin and one of apps/web.
func twoWebProjects(context.Context, string, string, []string) ([]TouchedProject, error) {
	page := []screens.Page{{File: "src/app/tasks/page.tsx", Route: "/tasks", Before: true, After: true}}
	return []TouchedProject{{Project: "apps/admin", Pages: page}, {Project: "apps/web", Pages: page}}, nil
}

// uploadFixture is a ready publication of a web change with a manifest of
// the head for apps/web, against a provider that posts the verification
// comment.
func uploadFixture(t *testing.T) (Finalizer, *finalizerProvider, Options, string) {
	t.Helper()
	f, git, provider, opts := finalizerFixture(t)
	provider.reviewOffered = true
	f.Screens = webScreens
	opts.Screenshots = []string{"shots/screenshots.json"}
	manifest := writeManifest(t, filepath.Join(f.Root, "shots"), "apps/web", git.head)
	return f, provider, opts, manifest
}

// TestFinalizeUploadsEachAcceptedManifestBeforeTheVerificationComment: each
// accepted manifest is sent once, with the published proposal, the head and
// the manifest's absolute path, after the proposal is published and before
// the verification comment; a second run sends the manifests again and the
// provider reports no new file.
func TestFinalizeUploadsEachAcceptedManifestBeforeTheVerificationComment(t *testing.T) {
	f, git, provider, opts := finalizerFixture(t)
	provider.reviewOffered, provider.screenshotsOffered = true, true
	f.Screens = twoWebProjects
	opts.Screenshots = []string{"shots/web/screenshots.json", "shots/admin/screenshots.json"}
	web := writeManifest(t, filepath.Join(f.Root, "shots", "web"), "apps/web", git.head)
	admin := writeManifest(t, filepath.Join(f.Root, "shots", "admin"), "apps/admin", git.head)
	result, err := f.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	want := []collab.ProposalScreenshotsInput{
		{Ref: result.ProposalRef, Commit: git.head, Manifest: admin},
		{Ref: result.ProposalRef, Commit: git.head, Manifest: web},
	}
	if !slices.Equal(provider.screenshotUploads, want) {
		t.Fatalf("uploads = %+v, want one per manifest in project order: %+v", provider.screenshotUploads, want)
	}
	order := []string{}
	for _, call := range provider.log {
		if call == "proposals.upsert" || call == "proposals.screenshots" || call == "proposals.review" {
			order = append(order, call)
		}
	}
	if !slices.Equal(order, []string{"proposals.upsert", "proposals.screenshots", "proposals.screenshots", "proposals.review"}) {
		t.Fatalf("writes ran in the order %v, want the uploads between the publication and the verification comment", order)
	}
	// Both manifests hold the same eight images, so the second sends none.
	uploaded := &ScreenshotsUpload{Manifests: []UploadedManifest{{Project: "apps/admin", Captures: 8, Uploaded: 8}, {Project: "apps/web", Captures: 8, Uploaded: 0}}}
	if got := result.ScreenshotsUpload; got == nil || got.Skipped != "" || !slices.Equal(got.Manifests, uploaded.Manifests) {
		t.Fatalf("report = %+v, want %+v", got, uploaded)
	}
	comment := provider.reviewBodies[0]
	again, err := f.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.screenshotUploads) != 4 || again.ScreenshotsUpload.Manifests[0].Uploaded != 0 || provider.reviewBodies[1] != comment {
		t.Fatalf("a second run: %d uploads, report %+v", len(provider.screenshotUploads), again.ScreenshotsUpload)
	}
	var stderr bytes.Buffer
	reportScreenshotsUpload(&stderr, result.ScreenshotsUpload)
	if got := stderr.String(); got != "change finalize: uploaded the screenshots of apps/admin: 8 captures, 8 files sent\n"+
		"change finalize: uploaded the screenshots of apps/web: 8 captures, 0 files sent\n" {
		t.Fatalf("stderr = %q", got)
	}
	spectest.Proves(t, screensFeature, finalizeUploads, "finalize-uploads-each-accepted-manifest-before-the-verification-comment")
}

// TestAProviderWithoutTheOperationGetsAWarningAndTheSameComment: a provider
// that does not offer proposals.screenshots receives no call, the run
// succeeds with the verification comment it posts without the upload, and
// the command warns on standard error.
func TestAProviderWithoutTheOperationGetsAWarningAndTheSameComment(t *testing.T) {
	f, provider, opts, _ := uploadFixture(t)
	result, err := f.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.screenshotUploads) != 0 || slices.Contains(provider.log, "proposals.screenshots") {
		t.Fatalf("a provider without the operation was called: %v", provider.log)
	}
	if got := result.ScreenshotsUpload; got == nil || got.Skipped != "the proposals provider does not offer screenshots" || len(got.Manifests) != 0 {
		t.Fatalf("report = %+v", got)
	}
	if len(provider.reviewBodies) != 1 || !strings.HasSuffix(provider.reviewBodies[0], "- screenshots: apps/web, 8 captures of 1 page") {
		t.Fatalf("verification comment = %q", provider.reviewBodies)
	}
	var stderr bytes.Buffer
	reportScreenshotsUpload(&stderr, result.ScreenshotsUpload)
	if got := stderr.String(); got != "change finalize: warning: the screenshots were not uploaded: the proposals provider does not offer screenshots; the verification comment is unchanged\n" {
		t.Fatalf("stderr = %q", got)
	}
	spectest.Proves(t, screensFeature, finalizeUploads, "a-provider-without-the-operation-gets-a-warning-and-the-same-comment")
}

// TestAnUnsupportedUploadWarnsAndKeepsTheVerificationComment: a provider
// that answers unsupported, such as one on a machine token or one whose
// backend does not serve the route, is asked once; the run goes on with the
// provider's message as the warning and the same verification comment.
func TestAnUnsupportedUploadWarnsAndKeepsTheVerificationComment(t *testing.T) {
	baseline, baselineProvider, baselineOpts, _ := uploadFixture(t)
	if _, err := baseline.Run(context.Background(), baselineOpts); err != nil {
		t.Fatal(err)
	}
	for name, failure := range map[string]*collab.Error{
		"a machine token":   {Reason: "screenshots.session_required", Message: "Putnami Delivery takes screenshots from a person's session"},
		"a route unserved":  {Reason: "screenshots.not_served", Message: "Putnami Delivery does not serve the screenshots of proposal pr"},
		"no error document": nil,
	} {
		t.Run(name, func(t *testing.T) {
			f, provider, opts, _ := uploadFixture(t)
			provider.screenshotsOffered = true
			provider.screenshotsAnswer = &collab.Envelope{Outcome: collab.OutcomeUnsupported, Error: failure}
			f.Screens = twoWebProjects
			opts.Screenshots = append(opts.Screenshots, "admin/screenshots.json")
			writeManifest(t, filepath.Join(f.Root, "admin"), "apps/admin", "0123456789012345678901234567890123456789")
			result, err := f.Run(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			want := "the proposals provider answered unsupported"
			if failure != nil {
				want = failure.Message
			}
			if got := result.ScreenshotsUpload; got == nil || got.Skipped != want || len(got.Manifests) != 0 {
				t.Fatalf("report = %+v, want skipped %q", got, want)
			}
			if len(provider.screenshotUploads) != 1 {
				t.Fatalf("the provider was asked %d times, want once", len(provider.screenshotUploads))
			}
			if len(provider.reviewBodies) != 1 || !strings.Contains(provider.reviewBodies[0], "- ordinary gate: ") ||
				strings.Contains(provider.reviewBodies[0], "upload") {
				t.Fatalf("verification comment = %q", provider.reviewBodies)
			}
		})
	}
	f, provider, opts, _ := uploadFixture(t)
	provider.screenshotsOffered = true
	provider.screenshotsAnswer = &collab.Envelope{Outcome: collab.OutcomeUnsupported, Error: &collab.Error{Message: "no session"}}
	if _, err := f.Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if provider.reviewBodies[0] != baselineProvider.reviewBodies[0] {
		t.Fatalf("the verification comment changed: %q, without the upload %q", provider.reviewBodies[0], baselineProvider.reviewBodies[0])
	}
	spectest.Proves(t, screensFeature, finalizeUploads, "an-unsupported-upload-warns-and-keeps-the-verification-comment")
}

// TestARefusedUploadFailsFinalizeWithTheProviderMessage: any other answer
// than ok or unsupported stops the run with the provider's message, after
// the publication and before the verification comment and the task
// transition; so does an answer for another proposal, commit or project.
func TestARefusedUploadFailsFinalizeWithTheProviderMessage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome collab.Outcome
		failure collab.Error
		want    string
	}{
		{"a head that is not a commit of the change", collab.OutcomeConflict, collab.Error{Reason: "revision.conflict", Message: "Putnami Delivery refused the screenshots (409 Conflict): the head is not a commit of the change"},
			"upload the screenshots of apps/web: proposals.screenshots answered conflict (revision.conflict): Putnami Delivery refused the screenshots (409 Conflict): the head is not a commit of the change"},
		{"a file above the bound", collab.OutcomeInvalid, collab.Error{Reason: "screenshots.invalid", Message: "Putnami Delivery refused the screenshots (413 Request Entity Too Large): a file exceeds 16 MiB"},
			"answered invalid (screenshots.invalid): Putnami Delivery refused the screenshots (413 Request Entity Too Large): a file exceeds 16 MiB"},
		{"bytes that are not the PNG the digest names", collab.OutcomeInvalid, collab.Error{Reason: "screenshots.invalid", Message: "Putnami Delivery refused the screenshots (422 Unprocessable Entity): not a PNG"},
			"(422 Unprocessable Entity): not a PNG"},
		{"a person who may not submit", collab.OutcomeDenied, collab.Error{Reason: "screenshots.refused", Message: "forbidden"}, "answered denied (screenshots.refused): forbidden"},
		{"an unknown outcome", collab.OutcomeUnresolved, collab.Error{Message: "no answer", Reconcile: "repeat proposals.screenshots"},
			"proposals.screenshots unresolved: no answer; reconcile with repeat proposals.screenshots before resuming"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, provider, opts, _ := uploadFixture(t)
			provider.screenshotsOffered = true
			provider.screenshotsAnswer = &collab.Envelope{Outcome: tc.outcome, Error: &tc.failure}
			opts.TaskSource, opts.TaskID = provider.task.Ref.Source, provider.task.Ref.ID
			_, err := f.Run(context.Background(), opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if provider.upserts != 1 || len(provider.screenshotUploads) != 1 || provider.reviews != 0 || provider.transitions != 0 {
				t.Fatalf("upserts=%d uploads=%d reviews=%d transitions=%d, want the run to stop at the one refused upload",
					provider.upserts, len(provider.screenshotUploads), provider.reviews, provider.transitions)
			}
		})
	}
	for name, answer := range map[string]collab.ProposalScreenshotsResult{
		"another proposal": {Ref: collab.Ref{Source: "local:proposal", ID: "other"}, Commit: "0123456789012345678901234567890123456789", Project: "apps/web"},
		"another commit":   {Ref: collab.Ref{Source: "local:proposal", ID: "pr"}, Commit: "ffff", Project: "apps/web"},
		"another project":  {Ref: collab.Ref{Source: "local:proposal", ID: "pr"}, Commit: "0123456789012345678901234567890123456789", Project: "apps/admin"},
	} {
		t.Run(name, func(t *testing.T) {
			f, provider, opts, _ := uploadFixture(t)
			provider.screenshotsOffered = true
			provider.screenshotsAnswer = &collab.Envelope{Outcome: collab.OutcomeOK, Result: mustJSON(t, answer)}
			if _, err := f.Run(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "proposals.screenshots answered") || provider.reviews != 0 {
				t.Fatalf("err = %v, reviews = %d", err, provider.reviews)
			}
		})
	}
	spectest.Proves(t, screensFeature, finalizeUploads, "a-refused-upload-fails-finalize-with-the-provider-message")
}

// TestAWaiverOrADraftUploadsNothing: a waiver, a draft and a change that
// touches no page send nothing and report no upload.
func TestAWaiverOrADraftUploadsNothing(t *testing.T) {
	for name, adjust := range map[string]func(*Finalizer, *Options){
		"a waiver": func(_ *Finalizer, opts *Options) {
			opts.Screenshots, opts.WaiveScreenshots = nil, "a class rename that renders the same"
		},
		"a draft": func(_ *Finalizer, opts *Options) {
			opts.Screenshots, opts.Draft, opts.VerificationFile = nil, true, ""
		},
		"no page touched": func(f *Finalizer, _ *Options) { f.Screens = noScreens },
	} {
		t.Run(name, func(t *testing.T) {
			f, provider, opts, _ := uploadFixture(t)
			provider.screenshotsOffered = true
			adjust(&f, &opts)
			result, err := f.Run(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(provider.screenshotUploads) != 0 || result.ScreenshotsUpload != nil {
				t.Fatalf("uploads = %+v, report = %+v, want none", provider.screenshotUploads, result.ScreenshotsUpload)
			}
		})
	}
	var stderr bytes.Buffer
	reportScreenshotsUpload(&stderr, nil)
	if stderr.Len() != 0 {
		t.Fatalf("a run without an upload wrote %q", stderr.String())
	}
	spectest.Proves(t, screensFeature, finalizeUploads, "a-waiver-or-a-draft-uploads-nothing")
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
