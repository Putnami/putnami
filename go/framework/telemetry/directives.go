package telemetry

import (
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.putnami.dev/logger"
	otlp "go.putnami.dev/protocol/telemetry"
)

// Export directives. Every OTLP exporter of the process, the log sink
// included, hands each collector response to processDirectives. A response
// that carries a valid Putnami-Export-Directive header replaces the directive
// in force: until its ttl ends, each trace setting it carries replaces that of
// the sampler of each running plugin, higher or lower, and every logger takes
// the stricter of the directive's level and its own. A void header changes
// nothing. The rule is the telemetry/otlp-push-contract one, shared with the
// TypeScript runtime.

// lowestLevel is the level limit outside a directive: every record passes it.
const lowestLevel = logger.Level(math.MinInt)

// directiveReceiver holds the directive in force and applies it. It is safe
// for concurrent use.
type directiveReceiver struct {
	now func() time.Time

	mu        sync.Mutex
	directive otlp.ExportDirective
	arrived   time.Time
	ends      time.Time
	samplers  map[*Sampler]struct{}

	// level is read on every log record, so it is lock-free.
	level atomic.Pointer[levelLimit]
}

// levelLimit is the log level a directive sets, until ends.
type levelLimit struct {
	level logger.Level
	ends  time.Time
}

var processDirectives = newDirectiveReceiver(time.Now)

func newDirectiveReceiver(now func() time.Time) *directiveReceiver {
	return &directiveReceiver{now: now, samplers: map[*Sampler]struct{}{}}
}

// observe applies the directive header of a collector response, if any.
func (r *directiveReceiver) observe(header http.Header) {
	values := header.Values(otlp.HeaderExportDirective)
	if len(values) == 0 {
		return
	}
	// Several header lines read as one list, as a fetch Headers object joins
	// them, so a key repeated across lines voids the directive in both
	// runtimes.
	r.receive(strings.Join(values, ", "))
}

// receive parses value and, when it is a valid directive, applies it.
func (r *directiveReceiver) receive(value string) {
	directive, ok := otlp.ParseExportDirective(value)
	if !ok {
		return
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.directive, r.arrived, r.ends = directive, now, now.Add(directive.TTL)
	for sampler := range r.samplers {
		sampler.budget.Direct(directive, now)
	}
	limit := &levelLimit{level: lowestLevel, ends: r.ends}
	if directive.LogLevel != "" {
		limit.level = logger.ParseLevel(directive.LogLevel)
		// The logger consults the limit on every record only once a directive
		// has set a level. Each such directive installs it again, so a caller
		// that removed the limit does not turn the directives off for good.
		logger.SetLevelLimit(r.logLevelLimit)
	}
	r.level.Store(limit)
}

// logLevelLimit is the level limit every logger consults: the directive's
// level while it holds.
func (r *directiveReceiver) logLevelLimit() logger.Level {
	limit := r.level.Load()
	if limit == nil || !r.now().Before(limit.ends) {
		return lowestLevel
	}
	return limit.level
}

// register makes sampler follow the directives, starting with the one in
// force, if any.
func (r *directiveReceiver) register(sampler *Sampler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samplers[sampler] = struct{}{}
	if r.now().Before(r.ends) {
		sampler.budget.Direct(r.directive, r.arrived)
	}
}

// unregister stops sampler from following the directives.
func (r *directiveReceiver) unregister(sampler *Sampler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.samplers, sampler)
}

// errExportDelayed reports an export skipped because the collector answered
// the previous one with a 429 and a Retry-After that has not passed.
var errExportDelayed = errors.New("otlp collector asked to wait before the next export")

// signalDelays holds the Retry-After delays of the process, by collector
// endpoint and signal path, so a 429 delays that signal for every exporter of
// the process that sends it there, and no other signal. It is safe for
// concurrent use.
type signalDelays struct {
	mu    sync.Mutex
	until map[string]time.Time // by endpoint + "|" + path
}

var processDelays = &signalDelays{}

// delayed reports whether an export of path to endpoint at now falls in a
// delay.
func (d *signalDelays) delayed(endpoint, path string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return now.Before(d.until[endpoint+"|"+path])
}

// observe starts the delay a response with status and header asks for.
func (d *signalDelays) observe(endpoint, path string, status int, header http.Header, now time.Time) {
	if delay := otlp.RetryAfterDelay(status, header.Get("Retry-After"), now); delay > 0 {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.until == nil {
			d.until = map[string]time.Time{}
		}
		d.until[endpoint+"|"+path] = now.Add(delay)
	}
}
