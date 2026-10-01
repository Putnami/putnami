// Package conformance provides the exported health-probe conformance pack runner.
// A downstream project certifies the framework's health/readiness
// probe contract in its own build with a single committed line:
//
//	func TestHealthConformance(t *testing.T) { conformance.Run(t) }
//
// Run asserts the app-owned half of the platform health contract:
//
//   - probe discovery across the module tree (app.Collect finds every
//     HealthChecker / ReadinessChecker the tree contributes — the discovery
//     /healthz and /readyz are built on),
//   - the liveness/readiness registry split (a ReadinessChecker never answers
//     /healthz, and vice versa),
//   - verbatim probe-error propagation (a passing probe reports "ok"; a failing
//     probe surfaces its error.Error() string, the checks-map contract), and
//   - the capability projection the emitter writes (a HealthChecker feeds probe
//     "health" → /healthz; a ReadinessChecker feeds probe "readiness" → /readyz),
//     re-run through the real go.putnami.dev/app describe emitter.
//
// It also pins the DI-injected probe's fail-closed rule: an unbound probe returns
// an error rather than silently passing, so a mis-wired probe drains traffic
// instead of masking a dead dependency.
//
// The HTTP response surface (/livez, /healthz, /readyz, /version envelopes and
// status codes) is served by go.putnami.dev/platform — a dependent of this module
// this package cannot import without a cycle — and is certified by the TypeScript
// twin (@putnami/application/conformance) and by go.putnami.dev/protocol/platform's
// own envelope conformance. This pack pins the probe machinery every runtime's
// endpoints stand on, so the two halves together certify the whole contract.
//
// It is a PURE pack: no external service, no skip gate — it runs in the normal
// unit gate. This package deliberately imports "testing" in non-test source: it
// exists to be called by a downstream project's own test binary, matching the
// database conformance-pack convention.
package conformance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/app"
	"go.putnami.dev/protocol/capabilities"
)

// conformanceProject is the project identity stamped into the emitted manifest
// the projection assertion reads. It matches the package the fixture probes are
// declared in so their provenance resolves to a complete inventory entry.
const conformanceProject = "go.putnami.dev/app"

// probeFailure is the verbatim failure a failing fixture probe reports. The
// contract is that this exact string reaches the /healthz or /readyz checks map,
// so the runner asserts it survives the probe boundary unaltered.
const probeFailure = "conn refused"

// healthOnly implements ONLY app.HealthChecker: it feeds /healthz (probe
// "health") and must never be collected as a readiness probe.
type healthOnly struct {
	name string
	fail bool
}

func (h *healthOnly) Name() string { return h.name }
func (h *healthOnly) CheckHealth(context.Context) error {
	if h.fail {
		return errProbe{}
	}
	return nil
}

// readinessOnly implements ONLY app.ReadinessChecker: it feeds /readyz (probe
// "readiness") and must never be collected as a liveness probe.
type readinessOnly struct {
	name string
	fail bool
}

func (r *readinessOnly) Name() string { return r.name }
func (r *readinessOnly) CheckReadiness(context.Context) error {
	if r.fail {
		return errProbe{}
	}
	return nil
}

// errProbe renders the verbatim failure string the checks-map contract requires.
type errProbe struct{}

func (errProbe) Error() string { return probeFailure }

// Run executes the health-probe conformance pack. See the package doc for the
// contract it certifies.
func Run(t *testing.T) {
	t.Helper()

	t.Run("discovery and registry split", func(t *testing.T) {
		assertDiscoveryAndSplit(t)
	})
	t.Run("verbatim probe errors", func(t *testing.T) {
		assertVerbatimProbeErrors(t)
	})
	t.Run("injected probe fails closed when unbound", func(t *testing.T) {
		assertInjectedProbeFailsClosed(t)
	})
	t.Run("probe-kind projection (health -> /healthz, readiness -> /readyz)", func(t *testing.T) {
		assertProbeKindProjection(t)
	})
}

// assertDiscoveryAndSplit pins that Collect finds every probe the module tree
// contributes (including from a child module) and that the liveness and readiness
// registries are independent: a ReadinessChecker is never returned as a
// HealthChecker, so /healthz and /readyz gate on disjoint probe sets.
func assertDiscoveryAndSplit(t *testing.T) {
	t.Helper()
	root := app.NewModule("root")
	root.Use(&healthOnly{name: "db"})
	child := app.NewModule("api")
	child.Use(&healthOnly{name: "cache"})
	child.Use(&readinessOnly{name: "warm"})
	root.Use(child)

	health := app.Collect[app.HealthChecker](root)
	readiness := app.Collect[app.ReadinessChecker](root)

	if got := names(health); !equalSet(got, []string{"cache", "db"}) {
		t.Errorf("Collect[HealthChecker] = %v, want [cache db] (both liveness probes, across the child module)", got)
	}
	if got := readinessNames(readiness); !equalSet(got, []string{"warm"}) {
		t.Errorf("Collect[ReadinessChecker] = %v, want [warm] (readiness only)", got)
	}
	// The registry split: a readiness probe must not answer /healthz.
	for _, h := range health {
		if h.Name() == "warm" {
			t.Errorf("readiness probe %q leaked into the liveness registry", h.Name())
		}
	}
}

// assertVerbatimProbeErrors pins the checks-map contract: a passing probe reports
// success (nil), a failing probe surfaces its error.Error() string verbatim — the
// runtime must not wrap or transform it, or an operator loses the originating
// cause.
func assertVerbatimProbeErrors(t *testing.T) {
	t.Helper()
	if err := (&healthOnly{name: "ok"}).CheckHealth(context.Background()); err != nil {
		t.Errorf("passing liveness probe returned %v, want nil", err)
	}
	if err := (&readinessOnly{name: "ok"}).CheckReadiness(context.Background()); err != nil {
		t.Errorf("passing readiness probe returned %v, want nil", err)
	}
	if err := (&healthOnly{name: "down", fail: true}).CheckHealth(context.Background()); err == nil || err.Error() != probeFailure {
		t.Errorf("failing liveness probe error = %v, want verbatim %q", err, probeFailure)
	}
	if err := (&readinessOnly{name: "down", fail: true}).CheckReadiness(context.Background()); err == nil || err.Error() != probeFailure {
		t.Errorf("failing readiness probe error = %v, want verbatim %q", err, probeFailure)
	}
}

// assertInjectedProbeFailsClosed pins the fail-closed rule: a DI-injected probe
// (app.HealthFunc) satisfies BOTH capability interfaces, and a probe that was
// never bound to a module returns an error rather than silently passing — a
// mis-wired probe must drain traffic, not mask a dead dependency.
func assertInjectedProbeFailsClosed(t *testing.T) {
	t.Helper()
	probe := app.HealthFunc("db", func(context.Context) error { return nil })

	// The DI probe feeds either endpoint depending on the capability it is
	// contributed under.
	var _ app.HealthChecker = probe
	var _ app.ReadinessChecker = probe

	// Unbound (never registered via app.Contribute): must fail closed.
	if err := probe.CheckHealth(context.Background()); err == nil {
		t.Error("unbound injected probe CheckHealth returned nil; a mis-wired probe must fail closed")
	}
	if err := probe.CheckReadiness(context.Background()); err == nil {
		t.Error("unbound injected probe CheckReadiness returned nil; a mis-wired probe must fail closed")
	}
}

// assertProbeKindProjection runs the real go.putnami.dev/app describe emitter over
// a fixture app carrying one liveness and one readiness probe, then asserts the
// emitted capability manifest projects HealthChecker -> probe "health" (/healthz)
// and ReadinessChecker -> probe "readiness" (/readyz). This pins the wiring in
// go/framework/app/capabilities.go against the capabilities protocol's probe-kind
// vocabulary.
func assertProbeKindProjection(t *testing.T) {
	t.Helper()

	out := t.TempDir()
	writeVersionStamp(t, out)

	a := app.New(conformanceProject)
	a.Use(&healthOnly{name: "diskSpace"})
	a.Use(&readinessOnly{name: "primaryDatabase"})
	if err := a.Describe(out, nil); err != nil {
		t.Fatalf("describe fixture app: %v", err)
	}

	//nolint:gosec // out is a test-controlled temp dir (t.TempDir()), not attacker-supplied
	data, err := os.ReadFile(filepath.Join(out, "schema", capabilities.ManifestFilename))
	if err != nil {
		t.Fatalf("read emitted capability manifest: %v", err)
	}
	document, diags := capabilities.ParseAndValidateManifestDocument(data)
	if document == nil || document.V2 == nil || len(diags) != 0 {
		t.Fatalf("emitted manifest failed strict validation: %v", diags)
	}

	byName := map[string]capabilities.ProbeKind{}
	for _, hc := range document.V2.HealthContributors {
		byName[hc.Name] = hc.Probe
	}
	if got := byName["diskSpace"]; got != capabilities.ProbeKindHealth {
		t.Errorf("HealthChecker projected to probe %q, want %q (feeds /healthz)", got, capabilities.ProbeKindHealth)
	}
	if got := byName["primaryDatabase"]; got != capabilities.ProbeKindReadiness {
		t.Errorf("ReadinessChecker projected to probe %q, want %q (feeds /readyz)", got, capabilities.ProbeKindReadiness)
	}
}

// writeVersionStamp writes the .gen/version.json the build pipeline stamps, so the
// describe emitter resolves a complete provenance inventory for probes declared in
// this package. The volatile version string is intentionally inert to the emitter.
func writeVersionStamp(t *testing.T, outDir string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"name":           conformanceProject,
		"capabilityRoot": ".",
		"capabilityPackages": []map[string]string{
			{"package": conformanceProject, "version": "0.0.0", "sourceRoot": "go/framework/app", "evidencePath": "go/framework/app/go.mod", "sourceBinding": conformanceSourceBinding()},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "version.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func conformanceSourceBinding() string {
	binding, err := capabilities.ComputeSourceBinding([]capabilities.SourceBindingFile{{Path: "conformance/conformance.go", Mode: capabilities.SourceModeRegular, Digest: capabilities.SourceDigest([]byte("conformance"))}})
	if err != nil {
		panic(err)
	}
	return binding
}

func names(cs []app.HealthChecker) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name()
	}
	return out
}

func readinessNames(cs []app.ReadinessChecker) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name()
	}
	return out
}

func equalSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		if seen[w] == 0 {
			return false
		}
		seen[w]--
	}
	return true
}
