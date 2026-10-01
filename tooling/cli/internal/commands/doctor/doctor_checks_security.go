package doctor

import (
	"fmt"
	"strings"

	doctor "go.putnami.dev/protocol/doctor"
)

// checkSecurityDefaults evaluates the security-default checks over the
// pre-parsed config schema fields and committed config sources
// supplied by checkConfigArtifacts. It keys off the config schema's
// productionUnsafeDefault marker — the additive field marker a producer stamps
// on a field whose fallback default is unsafe in production (an in-memory store,
// a process-generated signing key, a permissive transport, in-process rate
// limiting). A marked field that is left unset by the committed production
// config sources would silently apply its unsafe default, so it surfaces as one
// of the four frozen security codes.
//
// The check keys on the MARKER, never on a producer package name: a field is
// eligible purely because its own schema carries productionUnsafeDefault. The
// specific code is chosen from the field's config path (a schema-owned naming
// convention), not from which module emitted it — a module's keyring, for
// example, is inspected only through the marker it stamps.
//
// Security: config VALUES are never read. Eligibility is the marker plus whether
// a committed source ASSIGNS the path a value; Evidence names the schema file
// and the field path only, never a resolved value.
func checkSecurityDefaults(p doctorProject, fields []schemaField, assigned map[string]bool) []doctor.Finding {
	var findings []doctor.Finding
	// fields arrive ordered by dotted path, so the findings are deterministic
	// regardless of manifest field order.
	for _, f := range fields {
		if !f.productionUnsafe || assigned[f.path] {
			continue
		}
		code, ok := classifyUnsafeDefault(f.path)
		if !ok {
			continue
		}
		findings = append(findings, p.finding(
			code, false,
			doctorConfigSchemaPath, f.path,
			securityMessage(code, f.path, p.profile)))
	}
	return findings
}

// classifyUnsafeDefault maps a production-unsafe field to the frozen security
// code its category warrants, derived from the field's config path (a
// schema-owned naming convention) and never from the producing package. The
// keyword groups are checked in a fixed priority order so a path matching more
// than one group resolves deterministically. A marked field that matches no
// category is skipped rather than guessed at — the conservative,
// false-positive-free choice consistent with the rest of the doctor engine.
func classifyUnsafeDefault(fieldPath string) (doctor.CheckCode, bool) {
	lower := strings.ToLower(fieldPath)
	switch {
	case containsAny(lower, "signingkey", "signing", "keyring", "jwtsecret", "jwtsigning"):
		// A process-generated signing key: every issued token dies on restart.
		return doctor.CheckEphemeralSigningKey, true
	case containsAny(lower, "ratelimit", "rate_limit", "throttle"):
		// In-process limiting: the limit does not hold across replicas.
		return doctor.CheckLocalRateLimit, true
	case containsAny(lower, "cookie", "samesite", "insecure", "transport", "tls", "https"):
		// A permissive/plaintext transport or cookie default.
		return doctor.CheckInsecureTransport, true
	case containsAny(lower, "datasource", "database", "persistence", "storage", "store", "repository"):
		// A volatile in-memory store where durable persistence is expected.
		return doctor.CheckVolatilePersistence, true
	}
	return "", false
}

// securityMessage renders the human-readable finding message for a security
// code. It names the field path only — never a resolved value — so the message
// is safe to commit in a doctor report.
func securityMessage(code doctor.CheckCode, fieldPath string, profile doctor.Profile) string {
	switch code {
	case doctor.CheckVolatilePersistence:
		return fmt.Sprintf("persistence field %q falls back to a volatile in-memory store and is unset under the %s profile; data would not survive a restart", fieldPath, profile)
	case doctor.CheckEphemeralSigningKey:
		return fmt.Sprintf("signing-key field %q falls back to a process-generated ephemeral key and is unset under the %s profile; issued tokens would not survive a restart", fieldPath, profile)
	case doctor.CheckInsecureTransport:
		return fmt.Sprintf("transport field %q falls back to a permissive/insecure default and is unset under the %s profile", fieldPath, profile)
	case doctor.CheckLocalRateLimit:
		return fmt.Sprintf("rate-limit field %q falls back to in-process limiting and is unset under the %s profile; limits would not hold across replicas", fieldPath, profile)
	default:
		return fmt.Sprintf("configuration %q has a production-unsafe default and is unset under the %s profile", fieldPath, profile)
	}
}

// containsAny reports whether s contains any of the given substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
