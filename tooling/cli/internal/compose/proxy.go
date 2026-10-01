package compose

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.putnami.dev/tooling/cli/internal/workspace"
)

const (
	// defaultServePort is the proxy port of a target whose project states none,
	// the same default `putnami serve` uses.
	defaultServePort = 3000
	// proxyReadHeaderTimeout bounds a client that never finishes its headers.
	proxyReadHeaderTimeout = 10 * time.Second
	// proxyShutdownTimeout bounds the graceful half of closing a proxy; open
	// streams are cut after it.
	proxyShutdownTimeout = 2 * time.Second
)

// Bodies the proxy answers with while its member cannot serve. They follow the
// platform readiness envelope, so a readiness poller reads "unavailable" rather
// than a transport error it would have to classify.
const (
	backendNotReadyBody    = `{"status":"unavailable","checks":{"compose":"backend not ready"}}`
	backendUnreachableBody = `{"status":"unavailable","checks":{"compose":"backend unreachable"}}`
)

// DefaultTargetProxyPort is the proxy port of a composition's target when the
// caller names none: the project's options.serve.port, then 3000.
func DefaultTargetProxyPort(project *workspace.Project) int {
	if project != nil && project.Config != nil {
		if port, ok := portOption(project.Config.Options["serve"]["port"]); ok {
			return port
		}
	}
	return defaultServePort
}

// portOption reads a configured port in any of the shapes a JSON decode or a
// hand-built config produces.
func portOption(raw any) (int, bool) {
	var port int
	switch v := raw.(type) {
	case int:
		port = v
	case int64:
		port = int(v)
	case float64:
		if v != math.Trunc(v) {
			return 0, false
		}
		port = int(v)
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false
		}
		port = parsed
	default:
		return 0, false
	}
	if port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}

// backendPortKey carries the backend port the handler checked into the
// rewrite, so the two can never disagree about a port that changed between
// them.
type backendPortKey struct{}

// proxy is one member's stable listener. Its URL is what every dependant holds;
// the backend port behind it changes with every restart of the member.
type proxy struct {
	member   string
	port     int
	backend  atomic.Int64
	listener net.Listener
	server   *http.Server
	reverse  *httputil.ReverseProxy
	done     chan struct{}
}

// startProxy listens on 127.0.0.1:<port> (0 picks an ephemeral port) and
// serves until closed.
func startProxy(member string, port int) (*proxy, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, newError(CodeProxyFailed, member, PhaseProxies,
			fmt.Sprintf("listen on 127.0.0.1:%d: %v", port, err))
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return nil, newError(CodeProxyFailed, member, PhaseProxies, "the listener has no TCP address")
	}
	p := &proxy{
		member:   member,
		port:     address.Port,
		listener: listener,
		done:     make(chan struct{}),
	}
	p.reverse = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			backend, _ := r.In.Context().Value(backendPortKey{}).(int)
			r.SetURL(&url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(backend))})
			// SetURL points Host at the backend; the member sees the host its
			// caller addressed, exactly as without a proxy.
			r.Out.Host = r.In.Host
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			writeUnavailable(w, backendUnreachableBody)
		},
	}
	p.server = &http.Server{Handler: p, ReadHeaderTimeout: proxyReadHeaderTimeout}
	go func() {
		defer close(p.done)
		_ = p.server.Serve(listener)
	}()
	return p, nil
}

// URL is the proxy's stable address.
func (p *proxy) URL() string {
	return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(p.port))
}

// setBackend records the port the member bound, or 0 while it has none.
func (p *proxy) setBackend(port int) {
	p.backend.Store(int64(port))
}

func (p *proxy) backendPort() int {
	return int(p.backend.Load())
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	backend := p.backendPort()
	if backend == 0 {
		writeUnavailable(w, backendNotReadyBody)
		return
	}
	//nolint:gosec // G704: the destination is 127.0.0.1 at the port the member announced; nothing in the request selects it.
	p.reverse.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), backendPortKey{}, backend)))
}

func writeUnavailable(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(body))
}

// close stops accepting, lets in-flight requests finish briefly, then cuts the
// rest. It reports whether the listener is released.
func (p *proxy) close(ctx context.Context) error {
	shutdown, cancel := context.WithTimeout(ctx, proxyShutdownTimeout)
	defer cancel()
	if err := p.server.Shutdown(shutdown); err != nil {
		_ = p.server.Close()
	}
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("proxy of %s on port %d did not stop: %w", p.member, p.port, ctx.Err())
	}
}
