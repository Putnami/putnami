package http

import (
	"net/http/httptest"
	"testing"
)

func TestCache_SetsCacheControlHeader(t *testing.T) {
	mw := Cache(CacheOptions{MaxAge: 60})
	req := httptest.NewRequest("GET", "/api", nil)
	w := httptest.NewRecorder()
	ctx := NewContext(w, req)

	nextCalled := false
	resp := mw(ctx, func() *Response {
		nextCalled = true
		return JSON("ok")
	})
	if !nextCalled {
		t.Fatal("expected next() to be called")
	}
	if resp.Status != 200 {
		t.Errorf("status = %d, want 200", resp.Status)
	}
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Errorf("Cache-Control = %q, want %q", got, "public, max-age=60")
	}
}

func TestCacheControlValue(t *testing.T) {
	cases := []struct {
		name string
		opts CacheOptions
		want string
	}{
		{"public max-age", CacheOptions{MaxAge: 30}, "public, max-age=30"},
		{"private max-age", CacheOptions{MaxAge: 30, Private: true}, "private, max-age=30"},
		{"no-store wins over everything", CacheOptions{NoStore: true, MaxAge: 30, Private: true}, "no-store"},
		{"no-cache", CacheOptions{NoCache: true}, "public, no-cache"},
		{"no-cache with max-age", CacheOptions{NoCache: true, MaxAge: 10}, "public, no-cache, max-age=10"},
		{"zero options are public", CacheOptions{}, "public"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cacheControlValue(tc.opts); got != tc.want {
				t.Errorf("cacheControlValue(%+v) = %q, want %q", tc.opts, got, tc.want)
			}
		})
	}
}
