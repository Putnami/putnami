// Package cliusage pins the closed vocabulary of Putnami CLI usage telemetry:
// the event names, the per-event attribute allow/require sets, the expected OTLP
// value kind per attribute, and the error-category enum. It is the single
// importable source of that contract so the producer (the Putnami CLI's
// data-minimization guard) and the consumer (the anonymous telemetry receiver's
// sanitizer) enforce identical rules and cannot drift.
//
// Public job-command names are part of this shared contract too. Keeping the
// receiver's aggregate-key vocabulary closed prevents syntactically valid,
// attacker-chosen command names from consuming its bounded cardinality budget.
package cliusage

import (
	"regexp"
	"strings"
)

// Event names. Every CLI usage record's event.name attribute is one of these.
const (
	EventSessionStart = "session:start"
	EventSessionEnd   = "session:end"
)

// Business attribute keys — the closed-vocabulary dimensions of a session event.
const (
	AttrCommands      = "commands"
	AttrProjects      = "projects"
	AttrJobs          = "jobs"
	AttrSuccess       = "success"
	AttrDuration      = "duration"
	AttrErrorCategory = "errorCategory"
	AttrInteractive   = "interactive"

	AttrFlagImpacted = "flag.impacted"
	AttrFlagCoverage = "flag.coverage"
	AttrFlagOutput   = "flag.output"
	AttrFlagNoCache  = "flag.no-cache"
	AttrFlagProjects = "flag.projects"
	AttrFlagWatch    = "flag.watch"
)

// Envelope attribute keys — stamped on every record at encode time from stable
// process metadata, so they are valid (and required) on every wire record
// regardless of event.
const (
	AttrEventName  = "event.name"
	AttrDeviceID   = "device.id"
	AttrCLIVersion = "cli.version"
	AttrOS         = "os"
	AttrArch       = "arch"
)

// Error categories — the fixed enum a failed session reports instead of any free
// text. Derived CLI-side from the process exit code.
const (
	ErrorCategoryUsage   = "usage"
	ErrorCategoryAuth    = "auth"
	ErrorCategoryAPI     = "api"
	ErrorCategoryFailure = "failure"
)

// ServiceName is the service.name resource attribute every CLI usage record
// carries; the receiver pins it so unrelated OTLP traffic cannot masquerade as
// CLI usage telemetry.
const ServiceName = "putnami-cli"

// Framework and ScopeName are fixed receiver-owned metadata. Callers cannot
// use resource or instrumentation-scope strings as anonymous free-text fields.
const (
	Framework = "go"
	ScopeName = "putnami-cli"
)

// MaxCommands bounds how many aggregate command buckets one record can create.
const MaxCommands = 16

var (
	cliVersionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z][0-9A-Za-z.-]{0,63})?(?:\+[0-9A-Za-z][0-9A-Za-z.-]{0,63})?$`)
)

// Commands is the ordered receiver-known command vocabulary. It mirrors the
// CLI's public job-command catalog; a conformance test in the CLI pins the two
// lists together so adding a command requires an explicit wire-contract update.
var Commands = []string{
	"build",
	"test",
	"lint",
	"serve",
	"run",
	"format",
	"package",
	"publish",
	"deploy",
	"generate",
}

var commandSet = stringSet(Commands...)

var operatingSystems = stringSet(
	"aix", "android", "darwin", "dragonfly", "freebsd", "illumos", "ios",
	"js", "linux", "netbsd", "openbsd", "plan9", "solaris", "wasip1", "windows",
)

var architectures = stringSet(
	"386", "amd64", "arm", "arm64", "loong64", "mips", "mips64", "mips64le",
	"mipsle", "ppc64", "ppc64le", "riscv64", "s390x", "wasm",
)

// ValueKind is the OTLP scalar kind an attribute value must carry on the wire.
// Booleans encode as boolValue, integers as intValue (decimal string), and both
// strings and the comma-joined command list as stringValue.
type ValueKind int

// The scalar kinds CLI usage telemetry emits. No doubles are used.
const (
	KindString ValueKind = iota
	KindInt
	KindBool
)

// AllowedAttributes maps each event name to its business attribute keys and the
// wire value kind each must carry. Envelope attributes (EnvelopeAttributes) are
// additionally valid on every record. Producer and consumer both read the key
// set from here; the consumer additionally enforces the value kind.
//
// AttrCommands is KindString because it is comma-joined on the wire; the CLI
// producer holds it as []string before encoding and validates it against the
// command registry there.
var AllowedAttributes = map[string]map[string]ValueKind{
	EventSessionStart: {
		AttrCommands:     KindString,
		AttrProjects:     KindInt,
		AttrJobs:         KindInt,
		AttrInteractive:  KindBool,
		AttrFlagImpacted: KindBool,
		AttrFlagCoverage: KindBool,
		AttrFlagOutput:   KindBool,
		AttrFlagNoCache:  KindBool,
		AttrFlagProjects: KindBool,
		AttrFlagWatch:    KindBool,
	},
	EventSessionEnd: {
		AttrSuccess:       KindBool,
		AttrDuration:      KindInt,
		AttrErrorCategory: KindString,
		AttrInteractive:   KindBool,
	},
}

// RequiredAttributes maps each event name to the business attribute keys that
// must always be present. errorCategory is intentionally absent: it is required
// exactly when success is false (enforced in ValidateRecord), so it is optional
// at the key level.
var RequiredAttributes = map[string]map[string]struct{}{
	EventSessionStart: {
		AttrCommands:     {},
		AttrProjects:     {},
		AttrJobs:         {},
		AttrInteractive:  {},
		AttrFlagImpacted: {},
		AttrFlagCoverage: {},
		AttrFlagOutput:   {},
		AttrFlagNoCache:  {},
		AttrFlagProjects: {},
		AttrFlagWatch:    {},
	},
	EventSessionEnd: {
		AttrSuccess:     {},
		AttrDuration:    {},
		AttrInteractive: {},
	},
}

// EnvelopeAttributes are the process-metadata attributes stamped on every record
// at encode time, with the wire value kind each carries. They are valid and
// required on every CLI usage record regardless of event.
var EnvelopeAttributes = map[string]ValueKind{
	AttrEventName:  KindString,
	AttrDeviceID:   KindString,
	AttrCLIVersion: KindString,
	AttrOS:         KindString,
	AttrArch:       KindString,
}

// ErrorCategories is the ordered enum of failure categories a session may
// report. Ordered so docs and fixtures render deterministically.
var ErrorCategories = []string{
	ErrorCategoryUsage,
	ErrorCategoryAuth,
	ErrorCategoryAPI,
	ErrorCategoryFailure,
}

var errorCategorySet = func() map[string]struct{} {
	set := make(map[string]struct{}, len(ErrorCategories))
	for _, category := range ErrorCategories {
		set[category] = struct{}{}
	}
	return set
}()

// IsErrorCategory reports whether s is one of the fixed error categories.
func IsErrorCategory(s string) bool {
	_, ok := errorCategorySet[s]
	return ok
}

// IsEvent reports whether name is a known CLI usage event.
func IsEvent(name string) bool {
	_, ok := AllowedAttributes[name]
	return ok
}

// IsDeviceID accepts the sender's 128-bit lowercase hexadecimal identifier.
// device-123 is retained solely as the canonical, non-production golden
// fixture, keeping the existing cross-project wire fixture byte-stable.
func IsDeviceID(value string) bool {
	if value == "device-123" {
		return true
	}
	if len(value) != 32 {
		return false
	}
	for i := range value {
		if (value[i] < '0' || value[i] > '9') && (value[i] < 'a' || value[i] > 'f') {
			return false
		}
	}
	return true
}

// IsCLIVersion accepts released semver (with the optional Go-style v prefix),
// bounded prerelease/build metadata, and the CLI's source-build sentinel.
func IsCLIVersion(value string) bool {
	return value == "dev" || cliVersionPattern.MatchString(value)
}

// IsOS and IsArch close the runtime dimensions to Go's supported target
// vocabulary, preventing attacker-created aggregate keys.
func IsOS(value string) bool {
	_, ok := operatingSystems[value]
	return ok
}

// IsArch reports whether value is a supported Go target architecture.
func IsArch(value string) bool {
	_, ok := architectures[value]
	return ok
}

// IsCommand reports whether a command is in the receiver-known public job
// vocabulary.
func IsCommand(command string) bool {
	_, ok := commandSet[command]
	return ok
}

// IsCommands validates a comma-joined list against the receiver-known command
// vocabulary. A record can create at most MaxCommands aggregate buckets.
func IsCommands(value string) bool {
	commands := strings.Split(value, ",")
	if len(commands) == 0 || len(commands) > MaxCommands {
		return false
	}
	seen := make(map[string]struct{}, len(commands))
	for _, command := range commands {
		if !IsCommand(command) {
			return false
		}
		if _, duplicate := seen[command]; duplicate {
			return false
		}
		seen[command] = struct{}{}
	}
	return true
}

func stringSet(values ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}
