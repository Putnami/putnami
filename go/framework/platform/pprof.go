package platform

import (
	"fmt"
	nethttp "net/http"
	"net/http/pprof"
	"strings"

	"go.putnami.dev/http"
	protocol "go.putnami.dev/protocol/platform"
)

// registerPprof wires the net/http/pprof handlers onto the server. It
// is intentionally kept off the default surface because pprof exposes
// runtime internals and adds non-trivial CPU overhead during profile
// collection.
//
// The mount layout mirrors net/http/pprof's defaults under DefaultMux:
//
//	GET  {prefix}/debug/pprof          — HTML index
//	GET  {prefix}/debug/pprof/cmdline  — process argv
//	GET  {prefix}/debug/pprof/profile  — CPU profile
//	GET  {prefix}/debug/pprof/symbol   — symbol lookup
//	POST {prefix}/debug/pprof/symbol   — symbol lookup
//	GET  {prefix}/debug/pprof/trace    — execution trace
//	GET  {prefix}/debug/pprof/{name}   — registered profiles (heap,
//	                                     goroutine, allocs, block, …)
func (p *Plugin) registerPprof(server *http.ServerPlugin) {
	base := p.prefix + protocol.PathPprofPrefix

	server.GET(base, p.pprofIndex())
	server.GET(base+"/cmdline", wrapStd(pprof.Cmdline))
	server.GET(base+"/profile", wrapStd(pprof.Profile))
	server.GET(base+"/symbol", wrapStd(pprof.Symbol))
	server.Route("POST", base+"/symbol", wrapStd(pprof.Symbol))
	server.GET(base+"/trace", wrapStd(pprof.Trace))
	server.GET(base+"/{name}", p.pprofNamedProfile())
}

// wrapStd adapts a net/http handler function to the framework's
// Handler shape: write directly to the underlying ResponseWriter and
// return nil so the server doesn't try to re-serialize.
func wrapStd(h nethttp.HandlerFunc) http.Handler {
	return func(ctx *http.Context) *http.Response {
		h(ctx.Writer, ctx.Request)
		return nil
	}
}

// pprofNamedProfile dispatches /debug/pprof/{name} to the registered
// profile of that name (heap, goroutine, allocs, block, mutex,
// threadcreate, …). pprof.Handler returns a handler that ignores
// URL.Path, so the framework's path semantics are irrelevant here.
func (p *Plugin) pprofNamedProfile() http.Handler {
	return func(ctx *http.Context) *http.Response {
		name := ctx.Param("name")
		pprof.Handler(name).ServeHTTP(ctx.Writer, ctx.Request)
		return nil
	}
}

// pprofIndex serves a minimal HTML index linking to each available
// profile. We can't reuse pprof.Index directly because it hard-codes
// the "/debug/pprof/" prefix in the URLs it renders; with a
// configurable Prefix the links wouldn't resolve.
func (p *Plugin) pprofIndex() http.Handler {
	base := p.prefix + protocol.PathPprofPrefix
	return func(_ *http.Context) *http.Response {
		var b strings.Builder
		b.WriteString("<html><head><title>pprof</title></head><body>\n")
		b.WriteString("<h1>pprof</h1>\n<ul>\n")
		for _, name := range pprofProfileNames() {
			fmt.Fprintf(&b, "<li><a href=\"%s/%s\">%s</a></li>\n", base, name, name)
		}
		for _, name := range []string{"cmdline", "profile", "symbol", "trace"} {
			fmt.Fprintf(&b, "<li><a href=\"%s/%s\">%s</a></li>\n", base, name, name)
		}
		b.WriteString("</ul>\n</body></html>\n")
		// http.Text builds a text/plain response — override the
		// Content-Type so the index renders as HTML. (There is no
		// http.HTML helper today; if one lands, switch to it.)
		return http.Text(b.String()).WithHeader("Content-Type", "text/html; charset=utf-8")
	}
}

// pprofProfileNames returns the names of every registered profile.
// Hard-coded to the stdlib's defaults (which is what pprof.Profiles()
// would return without import-level side-effects we don't want to rely
// on). Custom profiles registered by application code will not show in
// the index — they remain reachable via /debug/pprof/{name}.
func pprofProfileNames() []string {
	return []string{"allocs", "block", "goroutine", "heap", "mutex", "threadcreate"}
}
