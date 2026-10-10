package remotecache

import (
	"testing"
)

// A hosted run's cache token comes from Delivery for the run credential, which
// the provider exchanged once and dropped. It replaces the configured source
// and any bearer that source already resolved, and a refusal never re-mints or
// replays it: the source that could replace it is gone.
func TestUseRunBearer_ReplacesTheConfiguredSourceAndIsNeverRefreshed(t *testing.T) {
	server := &cacheServer{accept: func(bearer string) bool { return bearer == "configured-1" }}
	srv := server.start(t)
	source := &mintingSource{fixed: "configured-1"}
	rec := &authRecorder{}
	c := NewClient(srv.URL, "", WithBearerFunc(source.resolve), WithAuthObserver(rec.observe))

	// The configured source already served the session (as the object-cache
	// probe at initialize does) before the run token arrives.
	if err := negotiateOnce(c); err != nil {
		t.Fatalf("negotiate with the configured source: %v", err)
	}
	c.UseRunBearer("run-cache-token")

	if err := negotiateOnce(c); err == nil {
		t.Fatal("negotiate succeeded against a server that refuses the run token")
	}
	sent := server.seen()
	if len(sent) != 2 || sent[1] != "run-cache-token" {
		t.Fatalf("bearers sent = %v, want the configured bearer then the run token once", sent)
	}
	if source.mints() != 1 {
		t.Fatalf("the configured source ran %d times, want 1: the run token replaces it", source.mints())
	}
	failures := rec.all()
	if len(failures) != 1 || failures[0].Source != TokenClassRun || failures[0].Refreshed {
		t.Fatalf("refusals = %+v, want one unrefreshed refusal of class run", failures)
	}
	if TokenClassRun.Renewable() {
		t.Fatal("TokenClassRun is renewable; the provider cannot mint another run token")
	}
}
