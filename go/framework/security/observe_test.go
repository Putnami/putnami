package security

import (
	"expvar"
	"net/http/httptest"
	"strconv"
	"testing"

	phttp "go.putnami.dev/http"
	"go.putnami.dev/protocol/features/spectest"
	identity "go.putnami.dev/protocol/identity/schema"
)

// counterValue reads the current value of one auth-decision counter. Counters
// are process-global (expvar), so tests assert on the delta around an action
// rather than an absolute value.
func counterValue(t *testing.T, d decision) int64 {
	t.Helper()
	v := decisionCounters().Get(string(d))
	if v == nil {
		return 0
	}
	n, err := strconv.ParseInt(v.String(), 10, 64)
	if err != nil {
		t.Fatalf("counter %q value %q not an int: %v", d, v.String(), err)
	}
	return n
}

func TestRecordDecision_PublishesCounter(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "secret-non-disclosure", "decision-metrics-expose-only-counters")
	// The counters must be published under a stable, scrapable name so an
	// operator (or /debug/vars) can read auth outcomes.
	decisionCounters() // ensure published
	if expvar.Get("security.auth_decisions") == nil {
		t.Fatal("expected security.auth_decisions to be published via expvar")
	}
}

func TestMiddleware_CountsAllow(t *testing.T) {
	before := counterValue(t, decisionAllow)

	mw := Middleware(Options{Roles: []string{"admin"}})
	ctx := newCtx()
	ctx.User = &phttp.Claims{Subject: "u-1", Roles: []string{"admin"}}

	resp := mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if resp.Status != 200 {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	if got := counterValue(t, decisionAllow); got != before+1 {
		t.Errorf("allow counter = %d, want %d", got, before+1)
	}
}

func TestMiddleware_CountsUnauthenticated(t *testing.T) {
	before := counterValue(t, decisionDenyUnauthenticated)

	mw := Middleware(Options{Roles: []string{"admin"}})
	ctx := newCtx() // no ctx.User

	resp := mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if resp.Status != 401 {
		t.Fatalf("status = %d, want 401", resp.Status)
	}
	if got := counterValue(t, decisionDenyUnauthenticated); got != before+1 {
		t.Errorf("deny_unauthenticated counter = %d, want %d", got, before+1)
	}
}

func TestMiddleware_CountsRoleDenial(t *testing.T) {
	before := counterValue(t, decisionDenyRole)

	mw := Middleware(Options{Roles: []string{"admin"}})
	ctx := newCtx()
	ctx.User = &phttp.Claims{Subject: "u-1", Roles: []string{"viewer"}}

	resp := mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if resp.Status != 403 {
		t.Fatalf("status = %d, want 403", resp.Status)
	}
	if got := counterValue(t, decisionDenyRole); got != before+1 {
		t.Errorf("deny_role counter = %d, want %d", got, before+1)
	}
}

func TestMiddleware_CountsScopeDenial(t *testing.T) {
	before := counterValue(t, decisionDenyScope)

	mw := Middleware(Options{Scopes: []string{"write"}})
	ctx := newCtx()
	ctx.User = &phttp.Claims{Subject: "u-1", Scopes: []string{"read"}}

	resp := mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if resp.Status != 403 {
		t.Fatalf("status = %d, want 403", resp.Status)
	}
	if got := counterValue(t, decisionDenyScope); got != before+1 {
		t.Errorf("deny_scope counter = %d, want %d", got, before+1)
	}
}

func TestMiddleware_CountsClientDenial(t *testing.T) {
	before := counterValue(t, decisionDenyClient)

	mw := Middleware(Options{Client: []string{"web-app"}})
	ctx := newCtx()
	ctx.User = &phttp.Claims{Subject: "u-1", ClientID: "rogue-app"}

	resp := mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if resp.Status != 403 {
		t.Fatalf("status = %d, want 403", resp.Status)
	}
	if got := counterValue(t, decisionDenyClient); got != before+1 {
		t.Errorf("deny_client counter = %d, want %d", got, before+1)
	}
}

func TestGuardMiddleware_CountsGuardDenialAndAllow(t *testing.T) {
	beforeDeny := counterValue(t, decisionDenyGuard)
	beforeAllow := counterValue(t, decisionAllow)

	mw := Middleware(Guard(func(user *phttp.Claims, _ *phttp.Context) bool {
		return user.Subject == "allowed"
	}))

	// Denied path.
	ctx := newCtx()
	ctx.User = &phttp.Claims{Subject: "blocked"}
	if resp := mw(ctx, func() *phttp.Response { return phttp.JSON("ok") }); resp.Status != 403 {
		t.Fatalf("denied status = %d, want 403", resp.Status)
	}
	if got := counterValue(t, decisionDenyGuard); got != beforeDeny+1 {
		t.Errorf("deny_guard counter = %d, want %d", got, beforeDeny+1)
	}

	// Allowed path.
	ctx = newCtx()
	ctx.User = &phttp.Claims{Subject: "allowed"}
	if resp := mw(ctx, func() *phttp.Response { return phttp.JSON("ok") }); resp.Status != 200 {
		t.Fatalf("allowed status = %d, want 200", resp.Status)
	}
	if got := counterValue(t, decisionAllow); got != beforeAllow+1 {
		t.Errorf("allow counter = %d, want %d", got, beforeAllow+1)
	}
}

func TestMiddleware_ExcludedPathNotCounted(t *testing.T) {
	beforeAllow := counterValue(t, decisionAllow)
	beforeDeny := counterValue(t, decisionDenyUnauthenticated)

	mw := Middleware(Options{Roles: []string{"admin"}, ExcludePaths: []string{"/_/"}})

	req := httptest.NewRequest("GET", "/_/health", nil)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)
	if resp := mw(ctx, func() *phttp.Response { return phttp.JSON("ok") }); resp.Status != 200 {
		t.Fatalf("excluded status = %d, want 200", resp.Status)
	}

	// An excluded path bypasses the rule entirely, so it must not move any
	// decision counter.
	if got := counterValue(t, decisionAllow); got != beforeAllow {
		t.Errorf("allow counter moved on excluded path: %d, want %d", got, beforeAllow)
	}
	if got := counterValue(t, decisionDenyUnauthenticated); got != beforeDeny {
		t.Errorf("deny counter moved on excluded path: %d, want %d", got, beforeDeny)
	}
}

func TestRecordDecision_AnonymousSubjectDoesNotPanic(t *testing.T) {
	// A nil user (unauthenticated) must record cleanly as "anonymous" without
	// dereferencing ctx.User.
	ctx := newCtx()
	recordDecision(ctx, decisionDenyUnauthenticated, "no identity")
}

// TestDecisionConstants_AliasGeneratedVocabulary pins the decision labels to the
// generated identity.AuthDecision* constants. The decision vocabulary is
// single-sourced from protocols/identity, so a manifest rename or wire-value
// change fails here (and in the contract twin drift tests) rather than silently
// diverging from the TypeScript framework's adopted constants.
func TestDecisionConstants_AliasGeneratedVocabulary(t *testing.T) {
	cases := []struct {
		got  decision
		want identity.AuthDecision
	}{
		{decisionAllow, identity.AuthDecisionAllow},
		{decisionDenyUnauthenticated, identity.AuthDecisionDenyUnauthenticated},
		{decisionDenyClient, identity.AuthDecisionDenyClient},
		{decisionDenyScope, identity.AuthDecisionDenyScope},
		{decisionDenyRole, identity.AuthDecisionDenyRole},
		{decisionDenyGuard, identity.AuthDecisionDenyGuard},
	}
	for _, c := range cases {
		if string(c.got) != string(c.want) {
			t.Errorf("decision %q is not aliased to generated AuthDecision %q", string(c.got), string(c.want))
		}
	}
}
