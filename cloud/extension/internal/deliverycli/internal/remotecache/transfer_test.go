package remotecache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	cache "go.putnami.dev/protocol/cache"
)

// storageToken is the credential a transfer carries in its headers.
const storageToken = "Bearer storage-token-of-one-namespace"

// redirectFixture is two servers behind made-up host names. The storage
// serves the transfer's URL and redirects it to the URL under test; the other
// server stands for every host that is not the storage. Both record the
// Authorization header of the request that follows the redirect.
type redirectFixture struct {
	storage, other *httptest.Server
	client         *http.Client

	mu sync.Mutex
	// landed is the Authorization header the redirect's target received, by
	// the Host header of that request.
	landed map[string]string
	// target is where the storage redirects /blob.
	target string
	// status is the redirect's status.
	status int
}

func newRedirectFixture(t *testing.T) *redirectFixture {
	t.Helper()
	content := []byte("blob bytes")
	fx := &redirectFixture{landed: map[string]string{}}
	land := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		fx.mu.Lock()
		fx.landed[r.Host] = r.Header.Get("Authorization")
		fx.mu.Unlock()
		_, _ = w.Write(content)
	}
	fx.storage = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/blob" {
			land(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		fx.mu.Lock()
		target, status := fx.target, fx.status
		fx.mu.Unlock()
		http.Redirect(w, r, target, status)
	}))
	t.Cleanup(fx.storage.Close)
	fx.other = httptest.NewServer(http.HandlerFunc(land))
	t.Cleanup(fx.other.Close)

	// Every made-up address resolves to one of the two servers: the storage's
	// own origin to the storage, anything else to the other server.
	dialer := &net.Dialer{}
	fx.client = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			to := fx.other.Listener.Addr().String()
			if addr == "storage.test:80" {
				to = fx.storage.Listener.Addr().String()
			}
			return dialer.DialContext(ctx, network, to)
		},
	}}
	t.Cleanup(fx.client.CloseIdleConnections)
	return fx
}

// TestTransfers_KeepTheCredentialInsideItsOrigin pins where a transfer's
// credential travels. A storage token rides in an Authorization header, and the
// transfer's URL can answer with a redirect. net/http drops the header when
// the redirect leaves the first host, but keeps it for a subdomain of that
// host and for another port of it. The transfers use a stricter rule: the
// header follows a redirect inside the origin, and no other.
func TestTransfers_KeepTheCredentialInsideItsOrigin(t *testing.T) {
	tests := []struct {
		name   string
		target string
		host   string // the Host header the target receives
		keep   bool
	}{
		{"another host", "http://attacker.test/landing", "attacker.test", false},
		{"a subdomain of the storage", "http://cdn.storage.test/landing", "cdn.storage.test", false},
		{"another port of the storage", "http://storage.test:8080/landing", "storage.test:8080", false},
		{"a parent domain", "http://test/landing", "test", false},
		{"the same origin", "http://storage.test/landing", "storage.test", true},
		{"the same origin, default port spelled out", "http://storage.test:80/landing", "storage.test:80", true},
	}
	content := []byte("blob bytes")
	transfer := func(method string) cache.BlobTransfer {
		return cache.BlobTransfer{
			Digest: cache.DigestOf(content), URL: "http://storage.test/blob", Method: method,
			SizeBytes: int64(len(content)), Headers: map[string]string{"Authorization": storageToken},
		}
	}
	for _, tt := range tests {
		t.Run("download: "+tt.name, func(t *testing.T) {
			fx := newRedirectFixture(t)
			fx.target, fx.status = tt.target, http.StatusFound
			c := NewClient("http://cache.test", "user-bearer", WithHTTPClient(fx.client))

			var got bytes.Buffer
			if _, err := c.DownloadBlob(context.Background(), transfer(cache.TransferGet), &got); err != nil {
				t.Fatalf("DownloadBlob: %v", err)
			}
			fx.assertLanded(t, tt.host, tt.keep)
		})
		t.Run("upload: "+tt.name, func(t *testing.T) {
			fx := newRedirectFixture(t)
			// A 307 keeps the method and the entity, so the PUT is sent again.
			fx.target, fx.status = tt.target, http.StatusTemporaryRedirect
			c := NewClient("http://cache.test", "user-bearer", WithHTTPClient(fx.client))

			if err := c.UploadBlob(context.Background(), transfer(cache.TransferPut), bytes.NewReader(content), int64(len(content))); err != nil {
				t.Fatalf("UploadBlob: %v", err)
			}
			fx.assertLanded(t, tt.host, tt.keep)
		})
	}
}

func (fx *redirectFixture) assertLanded(t *testing.T, host string, keep bool) {
	t.Helper()
	fx.mu.Lock()
	defer fx.mu.Unlock()
	got, reached := fx.landed[host]
	if !reached {
		t.Fatalf("the redirect did not reach %s; requests landed on %v", host, fx.landed)
	}
	switch {
	case keep && got != storageToken:
		t.Errorf("Authorization at %s = %q, want the storage token: the redirect stays inside the origin", host, got)
	case !keep && got != "":
		t.Errorf("Authorization at %s = %q, want none: the redirect leaves the origin", host, got)
	}
}

// TestTransfers_NeverCarryTheUserBearer keeps the older rule next to the new
// one: a transfer is authenticated by what the server issued for it, never by
// the user's cache bearer, before or after a redirect.
func TestTransfers_NeverCarryTheUserBearer(t *testing.T) {
	fx := newRedirectFixture(t)
	fx.target, fx.status = "http://storage.test/landing", http.StatusFound
	c := NewClient("http://cache.test", "user-bearer", WithHTTPClient(fx.client))

	content := []byte("blob bytes")
	var got bytes.Buffer
	if _, err := c.DownloadBlob(context.Background(), cache.BlobTransfer{
		Digest: cache.DigestOf(content), URL: "http://storage.test/blob", Method: cache.TransferGet, SizeBytes: int64(len(content)),
	}, &got); err != nil {
		t.Fatalf("DownloadBlob: %v", err)
	}
	fx.assertLanded(t, "storage.test", false)
}

// TestTransferClient_RemovesEveryCredentialHeader covers the headers a
// transfer could carry besides Authorization.
func TestTransferClient_RemovesEveryCredentialHeader(t *testing.T) {
	check := transferClient(&http.Client{}).CheckRedirect
	first, _ := http.NewRequest(http.MethodGet, "https://storage.test/blob", nil)
	next, _ := http.NewRequest(http.MethodGet, "https://attacker.test/blob", nil)
	for _, name := range credentialHeaders {
		next.Header.Set(name, "secret")
	}
	next.Header.Set("Content-Type", "application/octet-stream")
	if err := check(next, []*http.Request{first}); err != nil {
		t.Fatalf("CheckRedirect: %v", err)
	}
	for _, name := range credentialHeaders {
		if got := next.Header.Get(name); got != "" {
			t.Errorf("%s = %q after a redirect to another origin, want it removed", name, got)
		}
	}
	if next.Header.Get("Content-Type") == "" {
		t.Error("a header that is no credential must stay")
	}
}

// TestTransferClient_KeepsTheCallersRedirectPolicy proves the stricter rule
// adds to the policy of the client it copies and does not replace it, and
// that the default bound on redirects stays.
func TestTransferClient_KeepsTheCallersRedirectPolicy(t *testing.T) {
	loop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path+"x", http.StatusFound)
	}))
	defer loop.Close()

	t.Run("the caller's policy decides", func(t *testing.T) {
		calls := 0
		hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			calls++
			return http.ErrUseLastResponse
		}}
		resp, err := transferClient(hc).Get(loop.URL + "/a")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusFound || calls != 1 {
			t.Errorf("status = %d after %d policy calls, want the redirect itself after 1", resp.StatusCode, calls)
		}
	})

	t.Run("the default bound stays", func(t *testing.T) {
		resp, err := transferClient(nil).Get(loop.URL + "/a")
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
			t.Fatalf("err = %v, want the bound on redirects", err)
		}
	})
}

// TestSameOrigin pins the origin rule: scheme, host and port, with the
// scheme's default port.
func TestSameOrigin(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"https://storage.test/a", "https://storage.test/b?x=1", true},
		{"https://storage.test/a", "https://STORAGE.test:443/b", true},
		{"http://storage.test/a", "http://storage.test:80/b", true},
		{"https://storage.test/a", "http://storage.test/a", false},
		{"https://storage.test/a", "https://storage.test:8443/a", false},
		{"https://storage.test/a", "https://cdn.storage.test/a", false},
		{"https://cdn.storage.test/a", "https://storage.test/a", false},
		{"https://storage.test/a", "https://attacker.test/a", false},
		{"http://127.0.0.1:9000/a", "http://127.0.0.1:9001/a", false},
		{"http://[::1]:9000/a", "http://[::1]:9000/b", true},
	}
	for _, tt := range tests {
		a, errA := url.Parse(tt.a)
		b, errB := url.Parse(tt.b)
		if errA != nil || errB != nil {
			t.Fatalf("parse %q, %q: %v, %v", tt.a, tt.b, errA, errB)
		}
		if got := sameOrigin(a, b); got != tt.want {
			t.Errorf("sameOrigin(%s, %s) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

// TestDownloadRefused pins which download failures say the transfer itself is
// no longer good, so that a restore negotiates the key again: the answers the
// storage gives for an expired signature, an expired token, a withdrawn grant
// or a missing object. A failing storage, a transport failure and bad bytes
// say nothing about the transfer.
func TestDownloadRefused(t *testing.T) {
	content := []byte("blob bytes")
	digest := cache.DigestOf(content)
	for status, want := range map[int]bool{
		http.StatusBadRequest:          true,
		http.StatusUnauthorized:        true,
		http.StatusForbidden:           true,
		http.StatusNotFound:            true,
		http.StatusGone:                true,
		http.StatusTooManyRequests:     false,
		http.StatusInternalServerError: false,
		http.StatusServiceUnavailable:  false,
	} {
		t.Run(fmt.Sprintf("HTTP %d", status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()
			c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
			_, err := c.DownloadBlob(context.Background(), cache.BlobTransfer{
				Digest: digest, URL: srv.URL + "/blob", Method: cache.TransferGet, SizeBytes: int64(len(content)),
			}, io.Discard)
			if !IsStatus(err, status) {
				t.Fatalf("err = %v, want HTTP %d", err, status)
			}
			if got := DownloadRefused(err); got != want {
				t.Errorf("DownloadRefused(%v) = %v, want %v", err, got, want)
			}
			if got := DownloadRefused(fmt.Errorf("restore: %w", err)); got != want {
				t.Errorf("DownloadRefused of the wrapped error = %v, want %v", got, want)
			}
		})
	}

	t.Run("the hit fetcher surfaces the refusal to its reader", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
		fetch := c.HitFetcher(context.Background(), cache.KeyResult{Hit: true, Downloads: []cache.BlobTransfer{
			{Digest: digest, URL: srv.URL + "/blob", Method: cache.TransferGet, SizeBytes: int64(len(content))},
		}})
		rc, err := fetch(digest)
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		_, err = io.ReadAll(rc)
		_ = rc.Close()
		if !DownloadRefused(err) {
			t.Fatalf("the reader's error = %v, want a refused download", err)
		}
	})

	t.Run("other failures", func(t *testing.T) {
		for name, err := range map[string]error{
			"nil":                        nil,
			"a refused control exchange": &StatusError{Op: "negotiate", Status: http.StatusForbidden},
			"a transport failure":        &url.Error{Op: "Get", URL: "http://storage.test", Err: io.EOF},
			"bad bytes":                  errors.New("blob sha256:00 content-addresses to sha256:11"),
		} {
			if DownloadRefused(err) {
				t.Errorf("DownloadRefused(%s) = true, want false", name)
			}
		}
	})
}
