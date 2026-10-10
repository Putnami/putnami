package distributioncli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// registryWriter persists/redacts the per-host credential entry in the
// registry's owned store. Implementations are responsible for idempotency
// (re-running Setup with a different token must overwrite, not duplicate) and
// for preserving unrelated entries (other hosts, other settings) on both Setup
// and Teardown.
type registryWriter interface {
	Setup(env map[string]string, host, token string) error
	Teardown(env map[string]string, host string) error
}

func writerFor(r Registry) registryWriter {
	switch r {
	case RegistryNPM:
		return npmWriter{}
	case RegistryGomod:
		return gomodWriter{}
	case RegistryOCI:
		return ociWriter{}
	case RegistryPut:
		return putWriter{}
	}
	return nil
}

// materializeRegistryLease writes one already-minted, exact Distribution lease
// into the native client's credential format. The credential remains host-keyed
// because npm, netrc, and Docker do not model Putnami's owner/package resource
// coordinate; its authority is nevertheless bounded by the signed <=5 minute
// scope. Callers must refresh immediately before each distinct target.
func materializeRegistryLease(env map[string]string, endpoint RegistryEndpoint, token string) error {
	if err := validateCredentialLine(token); err != nil {
		return err
	}
	switch endpoint.Registry {
	case RegistryNPM:
		return writeNpmrcAuth(env, endpoint.Host, token)
	case RegistryGomod:
		return gomodWriter{}.Setup(env, endpoint.Host, token)
	case RegistryOCI:
		return writeDockerCredential(env, endpoint.Host, token)
	case RegistryPut:
		return putWriter{}.Setup(env, endpoint.Host, token)
	default:
		return fmt.Errorf("unsupported registry protocol %q", endpoint.Registry)
	}
}

// ----------------------------------------------------------------------------
// npm — ~/.putnami/registries.json resolver auth
//
// The Putnami npm registry is authenticated through keys[].token: publish
// resolves `putnami cloud token --for npm` and injects the result into the npm
// child process environment. That command prints a short-lived JWT by default,
// so we do not write a static
// `//<host>/:_authToken=` line to ~/.npmrc. Setup/teardown still scrub that
// line for the configured Putnami host to migrate older logins while preserving
// third-party registry entries and npm settings.
// ----------------------------------------------------------------------------

type npmWriter struct{}

func (npmWriter) Setup(env map[string]string, host, token string) error {
	if err := storeRegistryAuth(env, host, token); err != nil {
		return err
	}
	return removeNpmrcAuth(env, host)
}

func (npmWriter) Teardown(env map[string]string, host string) error {
	if err := removeRegistryAuth(env, host); err != nil {
		return err
	}
	return removeNpmrcAuth(env, host)
}

func removeNpmrcAuth(env map[string]string, host string) error {
	return rewriteLines(npmrcPath(env), func(lines []string) []string {
		return repairResidue(filterEntry(lines, npmAuthPrefixMatcher(host)))
	})
}

func writeNpmrcAuth(env map[string]string, host, token string) error {
	if err := validateCredentialLine(token); err != nil {
		return err
	}
	return rewriteLines(npmrcPath(env), func(lines []string) []string {
		line := "//" + host + "/:_authToken=" + token
		return appendUnique(repairResidue(filterEntry(lines, npmAuthPrefixMatcher(host))), line)
	})
}

func npmrcPath(env map[string]string) string {
	return filepath.Join(nativeHomeRoot(env), ".npmrc")
}

// ----------------------------------------------------------------------------
// gomod — ~/.netrc (see netrcPath for NETRC and Windows)
//
// `go get`, `go mod download`, and curl all default to .netrc Basic auth.
// gomod-server opts the workload into Basic via AllowBasicAuth and treats
// the password slot as a Bearer token. The username is ignored; we use
// `_token` as the conventional placeholder, matching what `npm`'s
// `_authToken` pattern signals.
//
//   machine go.putnami.dev login _token password pkt_xxx
//
// The file uses 0o600 permissions per the netrc(5) convention. Existing
// machine blocks for the same host are replaced; entries for other hosts
// are preserved.
// ----------------------------------------------------------------------------

type gomodWriter struct{}

func (gomodWriter) Setup(env map[string]string, host, token string) error {
	if err := validateCredentialLine(token); err != nil {
		return err
	}
	return rewriteLines(netrcPath(env), func(lines []string) []string {
		line := fmt.Sprintf("machine %s login _token password %s", host, token)
		return appendUnique(repairResidue(filterEntry(lines, netrcMachineMatcher(host))), line)
	})
}

func (gomodWriter) Teardown(env map[string]string, host string) error {
	return rewriteLines(netrcPath(env), func(lines []string) []string {
		return repairResidue(filterEntry(lines, netrcMachineMatcher(host)))
	})
}

// netrcPath returns the netrc file the go command reads, so the credential
// written there is the one `go mod download` sends.
func netrcPath(env map[string]string) string {
	return netrcPathFor(env, runtime.GOOS)
}

// netrcPathFor follows the go command's rule (cmd/go/internal/auth) on goos:
// NETRC when set; otherwise a file in the user home directory. On Windows Go
// reads _netrc when it exists and falls back to .netrc, and older Go releases
// read only _netrc. So on Windows the path is _netrc unless only .netrc
// exists; elsewhere it is .netrc.
func netrcPathFor(env map[string]string, goos string) string {
	if path := clicore.EnvGet(env, "NETRC"); path != "" {
		return path
	}
	home := nativeHomeRootFor(env, goos)
	dotNetrc := filepath.Join(home, ".netrc")
	if goos != "windows" {
		return dotNetrc
	}
	legacy := filepath.Join(home, "_netrc")
	if _, err := os.Stat(legacy); os.IsNotExist(err) {
		if _, err := os.Stat(dotNetrc); err == nil {
			return dotNetrc
		}
	}
	return legacy
}

// netrcMachineMatcher matches single-line `machine <host> …` records. Multi-line
// netrc records (where keywords span lines) are out of scope — the CLI only ever
// writes the single-line form and only ever removes entries it could have
// written.
func netrcMachineMatcher(host string) func(string) bool {
	return func(line string) bool {
		fields := strings.Fields(line)
		return len(fields) >= 2 && fields[0] == "machine" && fields[1] == host
	}
}

// npmAuthPrefixMatcher matches the per-host `_authToken` line.
func npmAuthPrefixMatcher(host string) func(string) bool {
	prefix := "//" + host + "/:_authToken="
	return func(line string) bool { return strings.HasPrefix(line, prefix) }
}

// ----------------------------------------------------------------------------
// oci — ~/.putnami/registries.json resolver auth
//
// The Putnami OCI registry is authenticated through keys[].token: publish uses
// a custom keychain that resolves `putnami cloud token --for oci` and returns
// the result as the OCI bearer. That command prints a short-lived JWT by
// default, so we do not write a static
// Docker auth entry or helper pin for the Putnami host. Setup/teardown still
// remove legacy auths[<host>] and the old empty credHelpers[<host>] pin while
// preserving global credsStore, third-party registry auth, and any non-empty
// user-installed helper for the same host.
// ----------------------------------------------------------------------------

type ociWriter struct{}

func (ociWriter) Setup(env map[string]string, host, token string) error {
	if err := storeRegistryAuth(env, host, token); err != nil {
		return err
	}
	return removeDockerCredential(env, host)
}

func (ociWriter) Teardown(env map[string]string, host string) error {
	if err := removeRegistryAuth(env, host); err != nil {
		return err
	}
	return removeDockerCredential(env, host)
}

func dockerConfigPath(env map[string]string) string {
	// DOCKER_CONFIG is Docker's own override (see `docker help` / the Docker
	// CLI config docs) and takes priority over any home-directory guess, the
	// same way the real `docker` binary resolves it.
	if v := clicore.EnvGet(env, "DOCKER_CONFIG"); v != "" {
		return filepath.Join(v, "config.json")
	}
	return filepath.Join(nativeHomeRoot(env), ".docker", "config.json")
}

func removeDockerCredential(env map[string]string, host string) error {
	path := dockerConfigPath(env)
	cfg := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(data) > 0 {
		if jerr := json.Unmarshal(data, &cfg); jerr != nil {
			return fmt.Errorf("%s: %w", path, jerr)
		}
	}
	changed := false
	if auths, ok := cfg["auths"].(map[string]any); ok {
		if _, exists := auths[host]; exists {
			delete(auths, host)
			changed = true
		}
		if len(auths) == 0 {
			delete(cfg, "auths")
		} else {
			cfg["auths"] = auths
		}
	}
	if helpers, ok := cfg["credHelpers"].(map[string]any); ok {
		if v, exists := helpers[host]; exists {
			if s, _ := v.(string); s == "" {
				delete(helpers, host)
				changed = true
			}
		}
		if len(helpers) == 0 {
			delete(cfg, "credHelpers")
		} else {
			cfg["credHelpers"] = helpers
		}
	}
	if !changed {
		return nil
	}
	if len(cfg) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func writeDockerCredential(env map[string]string, host, token string) error {
	if err := validateCredentialLine(token); err != nil {
		return err
	}
	path := dockerConfigPath(env)
	cfg := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	auths, _ := cfg["auths"].(map[string]any)
	if auths == nil {
		auths = map[string]any{}
	}
	auths[host] = map[string]any{
		"auth": base64.StdEncoding.EncodeToString([]byte("_token:" + token)),
	}
	cfg["auths"] = auths
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return clicore.WriteFileAtomic(path, append(out, '\n'), 0o600)
}

// ----------------------------------------------------------------------------
// put — no native client format yet.
//
// Until a put client lands, the raw token rides in ~/.putnami/registries.json
// alongside the KeyRef index (same 0o600 permissions as auth.json). When a
// real put client appears, this can move into a dedicated config without
// changing the public CLI surface — the writer interface stays the same.
// ----------------------------------------------------------------------------

type putWriter struct{}

func (putWriter) Setup(env map[string]string, host, token string) error {
	if err := validateCredentialLine(token); err != nil {
		return err
	}
	return mutateRegistriesState(env, func(s *RegistriesState) {
		if s.PutAuth == nil {
			s.PutAuth = map[string]string{}
		}
		s.PutAuth[host] = token
		removeRegistryAccess(s, host)
	})
}

func (putWriter) Teardown(env map[string]string, host string) error {
	return mutateRegistriesState(env, func(s *RegistriesState) {
		delete(s.PutAuth, host)
		if len(s.PutAuth) == 0 {
			s.PutAuth = nil
		}
		removeRegistryAccess(s, host)
	})
}

// mutateRegistriesState applies a closure to the state file. Unlike the
// KeyRef list which is managed by the setup orchestrator, this lets the put
// writer participate in the file without leaking write semantics outside
// the writer.
func mutateRegistriesState(env map[string]string, mutate func(*RegistriesState)) error {
	s, err := readRegistriesState(env)
	if err != nil {
		return err
	}
	if s == nil {
		s = &RegistriesState{Version: 1}
	}
	if s.Version == 0 {
		s.Version = 1
	}
	mutate(s)
	return WriteRegistriesState(env, s)
}

func storeRegistryAuth(env map[string]string, host, token string) error {
	return mutateRegistriesState(env, func(s *RegistriesState) {
		if s.Auth == nil {
			s.Auth = map[string]string{}
		}
		s.Auth[host] = token
		removeRegistryAccess(s, host)
	})
}

func removeRegistryAuth(env map[string]string, host string) error {
	return mutateRegistriesState(env, func(s *RegistriesState) {
		delete(s.Auth, host)
		if len(s.Auth) == 0 {
			s.Auth = nil
		}
		removeRegistryAccess(s, host)
	})
}

// ----------------------------------------------------------------------------
// Shared helpers
// ----------------------------------------------------------------------------

// nativeHomeRoot returns the user's real home directory — where npm, go, and
// Docker already look for their own credential files, with no knowledge of
// Putnami at all. It deliberately never consults PUTNAMI_HOME.
//
// This is the fix for a 2026-09-03 regression: the framework CLI runs every
// extension with PUTNAMI_HOME=~/.putnami, and an earlier version of this file
// used that same variable to locate ~/.npmrc and ~/.netrc. `putnami cloud
// token --for npm --materialize` (and the `--for go` / `registry-token
// --materialize` equivalents) then wrote a real, working credential to
// ~/.putnami/.npmrc and ~/.putnami/.netrc — files bun and go never read — so
// the materialized login silently did nothing from the native tool's point of
// view. PUTNAMI_HOME only relocates Putnami's OWN state (registries.json,
// auth.json, see clicore.PutnamiHome); it must never redirect a file a
// third-party tool reads by a fixed, hardcoded path.
//
// The directory follows the os.UserHomeDir rule (clicore.UserHomeDirFor), the
// one go, Node's os.homedir and Docker use: %USERPROFILE% on Windows, never a
// HOME that a POSIX shell exports there.
func nativeHomeRoot(env map[string]string) string {
	return nativeHomeRootFor(env, runtime.GOOS)
}

func nativeHomeRootFor(env map[string]string, goos string) string {
	if home := clicore.UserHomeDirFor(env, goos); home != "" {
		return home
	}
	return "."
}

// rewriteLines reads a UTF-8 line-oriented file, applies a transform, and
// writes it back with private permissions. A missing input file is
// treated as empty; an empty output deletes the file (idempotent teardown).
func rewriteLines(path string, transform func([]string) []string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var lines []string
	if len(data) > 0 {
		text := strings.TrimRight(string(data), "\n")
		if text != "" {
			lines = strings.Split(text, "\n")
		}
	}
	lines = transform(lines)
	if len(lines) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	out := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// validateCredentialLine refuses a token that cannot live on one line of a
// line-oriented credential file.
//
// This is the write-side half of a real corruption (2026-09-02): a token
// carrying embedded newlines was written as `//<host>/:_authToken=<token>`,
// which spilled the token's remaining lines into ~/.npmrc as free-standing
// junk. The next `removeNpmrcAuth` then matched only the FIRST physical line —
// the one carrying the prefix — and left the rest behind forever, so the file
// ended up holding nothing but the tail of a registries.json token recipe.
// Both halves are fixed: this guard stops the write, dropJSONResidue below
// cleans up what earlier versions already wrote.
//
// It fails the command instead of sanitizing the value: a token that is not a
// single line is not a token, and silently trimming one would hand the native
// client a truncated credential that fails far from its cause.
func validateCredentialLine(token string) error {
	if token == "" {
		return fmt.Errorf("refusing to write an empty registry credential")
	}
	if strings.ContainsAny(token, "\n\r") {
		return fmt.Errorf("refusing to write a multi-line registry credential; the token source returned %d lines instead of a bearer",
			len(strings.FieldsFunc(token, func(r rune) bool { return r == '\n' || r == '\r' })))
	}
	return nil
}

// filterEntry removes the lines a matcher selects AND the orphaned continuation
// lines that immediately follow one, which is the anchored half of the 2026-09-02
// repair.
//
// The damage is directional: a multi-line credential occupies its first physical
// line under the entry's own prefix and spills the rest as free-standing lines
// directly beneath it. So the continuation lines are exactly "the JSON fragments
// that follow a removed entry", and nothing else in the file is touched. A user's
// own `[section]`, bare key, comment, or third-party entry elsewhere in the file
// is never even examined.
func filterEntry(lines []string, matches func(string) bool) []string {
	out := make([]string, 0, len(lines))
	dropping := false
	for _, line := range lines {
		switch {
		case matches(line):
			dropping = true
		case dropping && isJSONResidueLine(line):
			// A continuation line of the entry just removed.
		default:
			dropping = false
			out = append(out, line)
		}
	}
	return out
}

// repairResidue handles the case the anchored filter cannot: a file whose entry
// line was already removed by an earlier CLI, leaving nothing but the orphaned
// continuation lines. That is precisely the artifact found on 2026-09-02 — a
// ~/.npmrc holding only `"command": [` … `]` `}`.
//
// It is deliberately all-or-nothing. A file in which EVERY remaining line is a
// JSON fragment is not npm or netrc config at all, so emptying it can lose no
// setting. A file with even one real line is left completely alone, because
// there the orphan's position is unknowable and guessing could delete something
// the user wrote.
func repairResidue(lines []string) []string {
	for _, line := range lines {
		if strings.TrimSpace(line) != "" && !isJSONResidueLine(line) {
			return lines
		}
	}
	return nil
}

// isJSONResidueLine reports whether a line is a JSON fragment rather than
// credential-file content. The test is narrow on purpose: no npmrc key and no
// netrc keyword begins with one of `"{}[],`.
func isJSONResidueLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	switch trimmed[0] {
	case '"', '{', '}', '[', ']', ',':
		return true
	default:
		return false
	}
}

func appendUnique(lines []string, candidate string) []string {
	if slices.Contains(lines, candidate) {
		return lines
	}
	return append(lines, candidate)
}
