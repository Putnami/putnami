package http

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestRateLimit_Concurrent(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	mw := RateLimit(RateLimitOptions{
		WindowMs: 60_000,
		Max:      1000,
		KeyFunc:  func(_ *Context) string { return "shared" },
		StopCh:   stopCh,
	})

	next := func() *Response { return JSON("ok") }

	const goroutines = 50
	const requestsPerGoroutine = 20

	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines*requestsPerGoroutine)

	for range goroutines {
		go func() {
			defer wg.Done()
			for range requestsPerGoroutine {
				w := httptest.NewRecorder()
				r := httptest.NewRequest("GET", "/test", nil)
				r.RemoteAddr = "127.0.0.1:12345"
				ctx := NewContext(w, r)
				resp := mw(ctx, next)
				if resp == nil {
					errs <- fmt.Errorf("nil response")
				}
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
}

func TestRateLimit_ConcurrentDifferentKeys(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	mw := RateLimit(RateLimitOptions{
		WindowMs: 60_000,
		Max:      5,
		KeyFunc: func(ctx *Context) string {
			return ctx.Header("X-Key")
		},
		StopCh: stopCh,
	})

	next := func() *Response { return JSON("ok") }

	const goroutines = 30
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/test", nil)
			r.Header.Set("X-Key", fmt.Sprintf("key-%d", i))
			ctx := NewContext(w, r)
			resp := mw(ctx, next)
			if resp == nil {
				t.Error("nil response")
			}
		}()
	}

	wg.Wait()
}

func TestServerPlugin_ConcurrentRequests(t *testing.T) {
	server := NewServerPlugin(ServerConfig{Port: 0})
	server.GET("/data", func(_ *Context) *Response {
		return JSON(map[string]string{"status": "ok"})
	})
	server.GET("/users/{id}", func(ctx *Context) *Response {
		return JSON(map[string]string{"id": ctx.Param("id")})
	})

	ts := server.TestServer()
	defer ts.Close()

	const goroutines = 30
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines)

	for i := range goroutines {
		go func() {
			defer wg.Done()
			url := ts.URL + "/data"
			if i%2 == 0 {
				url = fmt.Sprintf("%s/users/%d", ts.URL, i)
			}
			resp, err := http.Get(url)
			if err != nil {
				errs <- err
				return
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				errs <- fmt.Errorf("GET %s: status %d, want 200", url, resp.StatusCode)
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
}
