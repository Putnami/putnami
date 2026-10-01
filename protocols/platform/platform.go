// Package platform defines the shared platform-endpoints protocol used by
// framework plugins that expose the standard operational HTTP surface —
// liveness, readiness, version, and (optionally) pprof.
//
// The protocol fixes the contract that every Putnami runtime (Go, TS,
// future frameworks) presents to operators and orchestrators:
//
//   - the set of endpoint paths and their relative ordering,
//   - the JSON response shape per endpoint and per state,
//   - the HTTP status code rules (200 vs. 503) per state,
//   - the capability-interface contracts plugins implement to contribute
//     liveness and readiness probes,
//   - the discovery semantics for probes (auto-discovery from the
//     module tree, with explicit registrations winning over auto),
//   - the per-probe timeout default,
//   - and the policy for exposing pprof (off by default).
//
// Framework-specific implementation details (DI mechanics, plugin
// lifecycle hooks, language-native interface signatures) are out of
// scope; every implementation maps its native plugin model onto this
// contract.
package platform

// ProtocolVersion is the current platform protocol version. Bumped
// whenever a backwards-incompatible change to paths, response shapes,
// status-code rules, or capability semantics lands.
const ProtocolVersion = 1

// Canonical endpoint paths (relative to the configured prefix). Every
// compliant runtime mounts these at "<prefix><path>". The default
// prefix is empty (root); workloads may namespace under e.g. "/_" but
// the relative path component below is fixed.
const (
	PathLivez       = "/livez"
	PathHealthz     = "/healthz"
	PathReadyz      = "/readyz"
	PathVersion     = "/version"
	PathPprofPrefix = "/debug/pprof"
)

// CanonicalPaths is the canonical ordered set of operational paths every
// runtime must expose (pprof is omitted because it is opt-in).
var CanonicalPaths = []string{
	PathLivez,
	PathHealthz,
	PathReadyz,
	PathVersion,
}

// Status values for the canonical JSON envelope. Every endpoint that
// returns an envelope ({"status": "...", "checks": {...}}) uses exactly
// one of these.
type Status string

// Status values.
const (
	StatusOK          Status = "ok"          // probe / endpoint healthy
	StatusUnavailable Status = "unavailable" // process not in the running window
	StatusDegraded    Status = "degraded"    // running, but ≥1 probe failed
)

// ValidStatuses enumerates the canonical envelope status values.
var ValidStatuses = map[Status]bool{
	StatusOK:          true,
	StatusUnavailable: true,
	StatusDegraded:    true,
}

// HTTP status codes. The protocol only ever returns one of these on
// the canonical endpoints — anything else is a contract violation.
const (
	HTTPStatusOK          = 200
	HTTPStatusUnavailable = 503
)

// CapabilityKind identifies which capability a probe contributes to.
type CapabilityKind string

// CapabilityKind values.
const (
	CapabilityHealth    CapabilityKind = "health"    // contributes to /healthz
	CapabilityReadiness CapabilityKind = "readiness" // contributes to /readyz
)

// ValidCapabilityKinds enumerates the canonical probe capabilities.
var ValidCapabilityKinds = map[CapabilityKind]bool{
	CapabilityHealth:    true,
	CapabilityReadiness: true,
}

// EndpointKind classifies each canonical endpoint by behavior so
// runtimes and conformance tests can reason about expected shape.
type EndpointKind string

// EndpointKind values.
const (
	EndpointLiveness  EndpointKind = "liveness"  // /livez — process responsive
	EndpointHealth    EndpointKind = "health"    // /healthz — liveness aggregate
	EndpointReadiness EndpointKind = "readiness" // /readyz — readiness aggregate
	EndpointVersion   EndpointKind = "version"   // /version — build metadata
	EndpointPprof     EndpointKind = "pprof"     // /debug/pprof/* — opt-in
)

// EndpointSpec describes one canonical endpoint.
type EndpointSpec struct {
	// Path is the canonical path relative to the configured prefix.
	Path string `json:"path"`
	// Method is the canonical HTTP method (always GET for the operational
	// surface; pprof's symbol handler also accepts POST — runtimes mount
	// both when EnablePprof is true).
	Method string `json:"method"`
	// Kind classifies the endpoint behavior.
	Kind EndpointKind `json:"kind"`
	// AggregatesProbes is true when the endpoint runs the registered
	// probes of its capability before responding. /livez and /version
	// never run probes; /healthz and /readyz do.
	AggregatesProbes bool `json:"aggregatesProbes,omitempty"`
	// GatedByRunning is true when the endpoint returns 503 unavailable
	// before Start and after Stop, regardless of probe state.
	GatedByRunning bool `json:"gatedByRunning,omitempty"`
	// OptIn is true when the endpoint is disabled by default and must
	// be explicitly enabled (pprof). All other endpoints are mandatory.
	OptIn bool `json:"optIn,omitempty"`
}

// CapabilitySpec describes one canonical probe-contribution capability.
type CapabilitySpec struct {
	Kind CapabilityKind `json:"kind"`
	// Endpoint is the canonical path the capability feeds.
	Endpoint string `json:"endpoint"`
	// FailureConsequence is a human-readable description of what
	// happens (in orchestration terms) when a probe of this kind
	// fails. Used in diagnostic messages and documentation.
	FailureConsequence string `json:"failureConsequence"`
}

// DiscoveryContract defines how runtimes discover and register probes.
type DiscoveryContract struct {
	// AutoDiscoverFromModuleTree is true when the runtime walks its
	// module/DI tree to find every plugin implementing the relevant
	// capability interface and registers it automatically under the
	// plugin's name. Every Putnami runtime must implement this.
	AutoDiscoverFromModuleTree bool `json:"autoDiscoverFromModuleTree"`
	// ExplicitRegistrationsWin is true when an explicit (caller-side)
	// probe registration overrides an auto-discovered probe of the
	// same name. This lets workloads override built-in probes per
	// environment without monkey-patching plugins.
	ExplicitRegistrationsWin bool `json:"explicitRegistrationsWin"`
	// KeyedByPluginName is true when the auto-discovered probe is
	// keyed by the plugin's Name() and surfaces in the checks map
	// under that key.
	KeyedByPluginName bool `json:"keyedByPluginName"`
}

// ProbeContract defines runtime behavior requirements for every probe.
type ProbeContract struct {
	// DefaultTimeout is the per-probe timeout the runtime enforces when
	// the workload does not override it. Probes are expected to be cheap
	// (sub-second); the default is a safety cap.
	DefaultTimeoutMS int `json:"defaultTimeoutMs"`
	// MustRespectContextCancellation is true when probes are required to
	// honor ctx cancellation (return promptly on deadline expiry).
	MustRespectContextCancellation bool `json:"mustRespectContextCancellation"`
	// MustBeConcurrencySafe is true when probes are required to be safe
	// to call concurrently — the runtime may invoke probes in parallel
	// from concurrent requests.
	MustBeConcurrencySafe bool `json:"mustBeConcurrencySafe"`
	// ExposesErrorAsString is true when failure messages surface as the
	// probe's error.Error() string in the canonical checks map. Runtimes
	// must not wrap or transform the error message — operators need to
	// see the originating cause verbatim.
	ExposesErrorAsString bool `json:"exposesErrorAsString"`
}

// PprofContract defines the rules for exposing /debug/pprof/*.
type PprofContract struct {
	// OptInOnly is true when pprof must be disabled unless the workload
	// explicitly enables it via configuration. This is the canonical
	// posture — pprof leaks heap and goroutine internals and is not safe
	// to expose on a public port.
	OptInOnly bool `json:"optInOnly"`
	// SubPaths is the canonical set of pprof sub-paths the runtime must
	// expose when enabled.
	SubPaths []string `json:"subPaths"`
}

// Contract is the top-level platform protocol contract that every
// compliant runtime implementation publishes via DefaultContract.
type Contract struct {
	Version      int               `json:"version"`
	Endpoints    []EndpointSpec    `json:"endpoints"`
	Capabilities []CapabilitySpec  `json:"capabilities"`
	Discovery    DiscoveryContract `json:"discovery"`
	Probe        ProbeContract     `json:"probe"`
	Pprof        PprofContract     `json:"pprof"`
}

// CheckEntry is the canonical per-probe entry in the checks map. Values
// are strings: "ok" when the probe passed, or the probe's error.Error()
// otherwise. The protocol intentionally avoids structured per-probe
// payloads — operators read these in dashboards and ad-hoc curl.
type CheckEntry string

// Envelope is the canonical JSON response shape for endpoints that
// aggregate probes (/healthz, /readyz). /livez uses the same shape
// without the Checks field.
type Envelope struct {
	Status Status                `json:"status"`
	Checks map[string]CheckEntry `json:"checks,omitempty"`
}

// VersionInfo is the canonical /version response shape. Empty fields
// are omitted; runtimes fill what they can from build metadata and the
// workload's explicit configuration.
type VersionInfo struct {
	Name      string `json:"name,omitempty"`
	Version   string `json:"version,omitempty"`
	SHA       string `json:"sha,omitempty"`
	Branch    string `json:"branch,omitempty"`
	BuildTime string `json:"buildTime,omitempty"`
}

// DefaultContract returns the canonical platform contract that Putnami
// runtimes implement unless a later protocol version explicitly changes
// it.
func DefaultContract() Contract {
	return Contract{
		Version: ProtocolVersion,
		Endpoints: []EndpointSpec{
			{Path: PathLivez, Method: "GET", Kind: EndpointLiveness},
			{Path: PathHealthz, Method: "GET", Kind: EndpointHealth, AggregatesProbes: true, GatedByRunning: true},
			{Path: PathReadyz, Method: "GET", Kind: EndpointReadiness, AggregatesProbes: true, GatedByRunning: true},
			{Path: PathVersion, Method: "GET", Kind: EndpointVersion},
			{Path: PathPprofPrefix, Method: "GET", Kind: EndpointPprof, OptIn: true},
		},
		Capabilities: []CapabilitySpec{
			{
				Kind:               CapabilityHealth,
				Endpoint:           PathHealthz,
				FailureConsequence: "orchestrator restarts the pod (Kubernetes liveness)",
			},
			{
				Kind:               CapabilityReadiness,
				Endpoint:           PathReadyz,
				FailureConsequence: "orchestrator drains traffic from the pod without restarting (Kubernetes readiness)",
			},
		},
		Discovery: DiscoveryContract{
			AutoDiscoverFromModuleTree: true,
			ExplicitRegistrationsWin:   true,
			KeyedByPluginName:          true,
		},
		Probe: ProbeContract{
			DefaultTimeoutMS:               5000,
			MustRespectContextCancellation: true,
			MustBeConcurrencySafe:          true,
			ExposesErrorAsString:           true,
		},
		Pprof: PprofContract{
			OptInOnly: true,
			SubPaths: []string{
				"",         // index
				"/cmdline", // process argv
				"/profile", // CPU profile
				"/symbol",  // symbol lookup (also accepts POST)
				"/trace",   // execution trace
				"/{name}",  // named profiles: heap, goroutine, allocs, block, mutex, threadcreate
			},
		},
	}
}

// EndpointByKind returns the canonical endpoint spec for kind, or false
// when no endpoint matches.
func EndpointByKind(kind EndpointKind) (EndpointSpec, bool) {
	for _, e := range DefaultContract().Endpoints {
		if e.Kind == kind {
			return e, true
		}
	}
	return EndpointSpec{}, false
}

// JoinPrefix returns the full mounted path for endpoint under prefix,
// applying the canonical prefix-normalisation rules of NormalizePrefix.
func JoinPrefix(prefix, endpointPath string) string {
	normalized := NormalizePrefix(prefix)
	return normalized + endpointPath
}
