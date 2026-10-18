package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestReleasePlanCallbackWithoutAfterIsRefusedAtCapture(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "private-selected-publication-handoff", "callback-without-after-refused")
	for _, after := range []string{"", " "} {
		t.Run("after="+after, func(t *testing.T) {
			t.Setenv(InternalReleasePlanCallbackEnv, "http://127.0.0.1:123/v1/release-plan")
			t.Setenv(extensionproto.CloudTokenEnv, "private-token-never-log")
			t.Setenv(extensionproto.CloudCapabilityAfterEnv, after)
			output, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatal(err)
			}
			previous := os.Stderr
			os.Stderr = output
			defer func() { os.Stderr = previous; _ = output.Close() }()
			ctx := CaptureProcessCapabilities(t.Context())
			capabilities := processCapabilitiesFromContext(ctx)
			if capabilities == nil || capabilities.planCallback != "" || capabilities.cloudToken != "" {
				t.Fatal("callback retained without AFTER")
			}
			if ReleasePlanCallbackFrom(ctx) != nil {
				t.Fatal("a callback refused at capture is still offered to the handoff")
			}
			if _, present := os.LookupEnv(extensionproto.CloudTokenEnv); present {
				t.Fatal("capability remained ambient")
			}
			if _, present := os.LookupEnv(InternalReleasePlanCallbackEnv); present {
				t.Fatal("callback remained ambient")
			}
			if _, err := output.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			diagnostic, err := io.ReadAll(output)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(diagnostic), "PUTNAMI_CLOUD_CAPABILITY_AFTER is required") || strings.Contains(string(diagnostic), "private-token-never-log") {
				t.Fatalf("unsafe or missing capture diagnostic: %s", diagnostic)
			}
		})
	}
}

// A captured callback reaches the handoff alone: a job never inherits it,
// even when its own environment carries one. Not parallel: it sets the
// environment.
func TestReleasePlanCallbackNeverReachesAJob(t *testing.T) {
	t.Setenv(InternalReleasePlanCallbackEnv, "http://127.0.0.1:123/v1/release-plan")
	t.Setenv(extensionproto.CloudTokenEnv, "opaque-local-client-token-keep-private")
	t.Setenv(extensionproto.CloudCapabilityAfterEnv, "test,lint")
	ctx := CaptureProcessCapabilities(t.Context())
	if ReleasePlanCallbackFrom(ctx) == nil {
		t.Fatal("the captured callback is not offered to the handoff")
	}
	publish := capabilityTestJob(t.TempDir(), &workspace.Project{ID: "/api", Name: "api"}, "publish", "publish", "/unused", extensionproto.SideEffectsRegistry)
	for _, entry := range scopeProcessCapabilities(ctx, []string{InternalReleasePlanCallbackEnv + "=hostile"}, publish) {
		if strings.HasPrefix(entry, InternalReleasePlanCallbackEnv+"=") {
			t.Fatal("job inherited callback")
		}
	}
}

// releasePlanCallbackFixture is a callback as a capture with an AFTER
// contract leaves it.
func releasePlanCallbackFixture(token string) *ReleasePlanCallback {
	return &ReleasePlanCallback{capabilities: &processCapabilities{
		planCallback: "http://127.0.0.1:123/v1/release-plan",
		cloudToken:   token,
		after:        "test",
	}}
}

// The handoff finds a callback only in a context whose capture kept one.
func TestReleasePlanCallbackFromNeedsACapturedCallback(t *testing.T) {
	t.Parallel()
	if ReleasePlanCallbackFrom(context.Background()) != nil {
		t.Fatal("a context with no capture offered a callback")
	}
	captured := context.WithValue(context.Background(), processCapabilityContextKey{}, &processCapabilities{cloudToken: "token", after: "test"})
	if ReleasePlanCallbackFrom(captured) != nil {
		t.Fatal("a capture with no callback offered one")
	}
	callback := releasePlanCallbackFixture("token")
	withCallback := context.WithValue(context.Background(), processCapabilityContextKey{}, callback.capabilities)
	if got := ReleasePlanCallbackFrom(withCallback); got == nil || got.Endpoint() != callback.capabilities.planCallback {
		t.Fatalf("the captured callback = %v, want its endpoint", got)
	}
}

// The callback carries a bearer only when the capture holds an AFTER contract
// and a token of at most 64 KiB with no space, tab, line break or NUL. It
// puts the bearer on a request and never renders it.
func TestReleasePlanCallbackAuthorizesOnlyAWellFormedCapability(t *testing.T) {
	t.Parallel()
	for name, token := range map[string]string{
		"empty":      "",
		"space":      "opaque token",
		"tab":        "opaque\ttoken",
		"line break": "opaque\ntoken",
		"return":     "opaque\rtoken",
		"nul":        "opaque\x00token",
		"oversize":   strings.Repeat("x", 64<<10+1),
	} {
		if releasePlanCallbackFixture(token).Authorized() {
			t.Errorf("a %s bearer authorized the callback", name)
		}
	}
	withoutAfter := releasePlanCallbackFixture("opaque-token")
	withoutAfter.capabilities.after = ""
	if withoutAfter.Authorized() {
		t.Error("a capture without AFTER authorized the callback")
	}
	callback := releasePlanCallbackFixture(strings.Repeat("x", 64<<10))
	if !callback.Authorized() {
		t.Fatal("a 64 KiB bearer did not authorize the callback")
	}

	callback = releasePlanCallbackFixture("opaque-local-client-token-keep-private")
	request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:123/v1/release-plan", nil)
	if err != nil {
		t.Fatal(err)
	}
	callback.SetAuthHeader(request)
	if got := request.Header.Get("Authorization"); got != "Bearer opaque-local-client-token-keep-private" {
		t.Fatalf("Authorization = %q", got)
	}
	for _, rendered := range []string{fmt.Sprint(callback), fmt.Sprintf("%+v", callback), fmt.Sprintf("%#v", *callback), fmt.Sprintf("<%s>", *callback), fmt.Sprintf("%q", *callback)} {
		if strings.Contains(rendered, "opaque-local") {
			t.Fatalf("the callback rendered its bearer: %s", rendered)
		}
	}
}

// One process binds one plan digest: the same digest again posts nothing, a
// different one is refused, and a failed handoff binds nothing.
func TestReleasePlanCallbackBindsOneDigest(t *testing.T) {
	t.Parallel()
	callback := releasePlanCallbackFixture("token")
	posts := 0
	post := func() error { posts++; return nil }
	failure := errors.New("broker refused")
	if err := callback.Bind("sha256:a", func() error { posts++; return failure }); !errors.Is(err, failure) {
		t.Fatalf("a failed handoff returned %v", err)
	}
	if err := callback.Bind("sha256:a", post); err != nil || posts != 2 {
		t.Fatalf("a retry after a failed handoff: err=%v posts=%d, want a second post", err, posts)
	}
	if err := callback.Bind("sha256:a", post); err != nil || posts != 2 {
		t.Fatalf("the bound digest again: err=%v posts=%d, want no post", err, posts)
	}
	if err := callback.Bind("sha256:b", post); err == nil || posts != 2 {
		t.Fatalf("another digest: err=%v posts=%d, want a refusal and no post", err, posts)
	}
}
