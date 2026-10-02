package main

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/memberprobe"
	"go.putnami.dev/sdk/extension/npmpublish"
)

// maxNPMProbeTarballBytes bounds how much of a served tarball a probe hashes.
const maxNPMProbeTarballBytes = 1 << 30

// npmDryRun is what a dry-run npm publication asks its registry about: the
// staged package and the route the real publish would take.
type npmDryRun struct {
	// managed is true for a release-set member, which the real publish uploads
	// over HTTP. An unmanaged publication goes through npm.
	managed bool
	// wsRoot, projectPath and npmDir locate the staged package.
	wsRoot, projectPath, npmDir string
	packageName, version        string
	// registry is the declared registry. It is always set on the managed path.
	// On the unmanaged path it is empty when npm's own configuration names the
	// registry.
	registry string
	// endpoint is where a managed publication sends its requests: the declared
	// registry, or the private broker of a native publication run.
	endpoint string
	// credentialHost is the host the credential seam is asked about.
	credentialHost string
	// authEnv is the npm environment the unmanaged publish runs npm with, before
	// any credential is added.
	authEnv map[string]string
}

// probeNPMPackage asks the registry whether it already holds the staged package
// at its version and emits the answer as one member-probe event.
//
// A managed publication sends one GET of the tarball, with the bearer when the
// host-only seam yields one. An unmanaged publication runs `npm view` with the
// npm environment of the real publish.
func probeNPMPackage(emit *jsonl.Emitter, run npmDryRun) {
	if run.managed {
		memberprobe.Emit(emit, probeManagedNPM(run))
		return
	}
	memberprobe.Emit(emit, probeUnmanagedNPM(run))
}

// probeManagedNPM compares the tarball the registry serves with the tarball the
// managed publish would upload. It packs the staged package only when the
// registry holds the version.
func probeManagedNPM(run npmDryRun) extproto.MemberProbe {
	token, _ := npmResolveRegistryToken(run.credentialHost)
	subject := memberprobe.Subject{
		Ecosystem: npmEcosystem, Coordinate: run.packageName, Version: run.version,
		Registry: run.registry, Anonymous: token == "",
	}
	registryDigest, verdict := readNPMTarballDigest(subject, run.endpoint, token)
	if verdict != nil {
		return *verdict
	}

	config, cleanupConfig, err := newManagedNPMConfig(nil)
	if err != nil {
		return subject.Conflict(registryDigest, notComparedReason(err))
	}
	defer cleanupConfig()
	_, localDigest, cleanupArtifact, err := packNPMArtifact(config, run.wsRoot, run.projectPath, run.npmDir)
	if cleanupArtifact != nil {
		defer cleanupArtifact()
	}
	if err != nil {
		return subject.Conflict(registryDigest, notComparedReason(err))
	}
	subject.ArtifactDigest = localDigest
	return subject.Held(registryDigest)
}

// notComparedReason is the reason of a conflict whose local side is missing:
// the registry holds the version and the staged package could not be packed.
func notComparedReason(err error) string {
	return "the registry already holds this version and the staged package could not be packed to compare: " + err.Error()
}

// readNPMTarballDigest sends the managed probe's one request, a GET of the
// version's tarball, and returns the digest of the bytes the registry serves.
// It returns a verdict instead when the registry does not hold the version or
// could not answer.
func readNPMTarballDigest(subject memberprobe.Subject, endpoint, token string) (string, *extproto.MemberProbe) {
	verdict := func(probe extproto.MemberProbe) (string, *extproto.MemberProbe) { return "", &probe }
	client, err := newManagedNPMHTTPClient()
	if err != nil {
		return verdict(subject.Unverified(err.Error()))
	}
	client.Timeout = memberprobe.Timeout
	resp, err := getManagedNPMTarball(client, endpoint, token, subject.Coordinate, subject.Version)
	if err != nil {
		if sendErr := (*url.Error)(nil); errors.As(err, &sendErr) {
			return verdict(subject.Unreachable(err))
		}
		return verdict(subject.Unverified(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return verdict(subject.Absent())
	case resp.StatusCode != http.StatusOK:
		return verdict(subject.Refused(resp.StatusCode, npmpublish.ErrorExcerpt(resp.Body, token)))
	}
	hash := sha256.New()
	read, err := io.Copy(hash, io.LimitReader(resp.Body, maxNPMProbeTarballBytes+1))
	if err != nil {
		return verdict(subject.Unreachable(err))
	}
	if read > maxNPMProbeTarballBytes {
		return verdict(subject.Unverified(fmt.Sprintf("the registry serves a tarball larger than %d bytes", maxNPMProbeTarballBytes)))
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
}

// npmViewAnswer is what `npm view <name>@<version> dist --json` prints: the
// version's dist object, or the error npm met.
type npmViewAnswer struct {
	Integrity string `json:"integrity"`
	Error     *struct {
		Code    string `json:"code"`
		Summary string `json:"summary"`
	} `json:"error"`
}

// npmErrorCode finds the error code npm prints on stderr.
var npmErrorCode = regexp.MustCompile(`(?m)^npm (?:ERR!|error) code (\S+)`)

// npmUnreachableCodes are the npm error codes of a request that got no
// response from the registry.
var npmUnreachableCodes = map[string]bool{
	"ECONNREFUSED": true, "ECONNRESET": true, "ENOTFOUND": true, "EAI_AGAIN": true,
	"ETIMEDOUT": true, "ENETUNREACH": true, "EHOSTUNREACH": true, "ERR_SOCKET_TIMEOUT": true,
}

// probeUnmanagedNPM asks through npm, with the environment the real publish
// runs npm with. It runs `npm view`, and `npm pack` when the registry holds the
// version, and compares the packed tarball with the registry's integrity.
//
// The probe never marks the answer anonymous: npm's own configuration may hold
// a credential the probe does not see.
func probeUnmanagedNPM(run npmDryRun) extproto.MemberProbe {
	viewEnv := maps.Clone(run.authEnv)
	token := ""
	if run.registry != "" {
		if token, _ = npmResolveRegistryToken(run.credentialHost); token != "" {
			viewEnv[npmAuthKey(run.registry)] = token
		}
	}
	subject := memberprobe.Subject{
		Ecosystem: npmEcosystem, Coordinate: run.packageName, Version: run.version,
		Registry: unmanagedNPMRegistry(run),
	}

	// --fetch-retries 0 sends one request per question.
	result, err := runNPM(run.npmDir, viewEnv, "view", run.packageName+"@"+run.version, "dist", "--json", "--fetch-retries", "0")
	if err != nil || result == nil {
		return subject.Unverified(memberprobe.Redact(fmt.Sprintf("npm could not be run: %v", err), token))
	}
	var answer npmViewAnswer
	stdout := strings.TrimSpace(result.Stdout)
	if stdout != "" {
		if err := json.Unmarshal([]byte(stdout), &answer); err != nil {
			return subject.Unverified("npm view answered something that is not one JSON object")
		}
	}
	if !result.Success || answer.Error != nil {
		code, summary := "", ""
		if answer.Error != nil {
			code, summary = answer.Error.Code, answer.Error.Summary
		} else if match := npmErrorCode.FindStringSubmatch(result.Stderr); match != nil {
			code = match[1]
		}
		if code == "E404" {
			return subject.Absent()
		}
		return subject.Unverified(npmFailureReason(code, memberprobe.Redact(summary, token)))
	}
	if stdout == "" {
		// npm prints nothing for a package that exists without this version.
		return subject.Absent()
	}

	localDigest, localIntegrity, err := packWithNPM(run)
	if err != nil {
		return subject.Conflict("", notComparedReason(err))
	}
	subject.ArtifactDigest = localDigest
	same, registryDigest, comparable := compareNPMIntegrity(answer.Integrity, localDigest, localIntegrity)
	switch {
	case !comparable:
		return subject.Unverified("the registry holds this version but advertised no sha512 or sha256 integrity to compare")
	case same:
		return subject.Held(localDigest)
	default:
		return subject.Conflict(registryDigest, reasonNPMUnmanagedOtherBytes)
	}
}

// reasonNPMUnmanagedOtherBytes is the reason of a version the registry holds
// with other bytes than the packed package, outside a release set.
const reasonNPMUnmanagedOtherBytes = "the registry already holds this version with other bytes than the packed package; " +
	"the real publish outside a release set reuses the existing version without comparing the bytes"

// npmFailureReason says why `npm view` could not answer, from npm's error code.
func npmFailureReason(code, summary string) string {
	switch {
	case npmUnreachableCodes[code]:
		return "the registry could not be reached: npm reports " + code
	case code == "E401" || code == "E403" || code == "ENEEDAUTH":
		return "the registry refused the request with " + code + "; sign in to npm or supply the registry token, then run the dry run again"
	case code == "":
		return "npm view failed without an error code"
	case summary == "":
		return "npm view failed with " + code
	default:
		return "npm view failed with " + code + ": " + summary
	}
}

// compareNPMIntegrity compares the integrity a registry advertises for a
// version with the local tarball. An integrity is one or more
// `<algorithm>-<base64>` entries; the strongest algorithm present decides, as
// npm itself reads it. registryDigest is the registry's sha256 in the form a
// probe carries, when the registry advertised one. comparable is false when it
// advertised neither a sha512 nor a sha256.
func compareNPMIntegrity(integrity, localDigest, localIntegrity string) (same bool, registryDigest string, comparable bool) {
	var sha512Entries, sha256Entries []string
	for _, entry := range strings.Fields(integrity) {
		switch {
		case strings.HasPrefix(entry, "sha512-"):
			sha512Entries = append(sha512Entries, entry)
		case strings.HasPrefix(entry, "sha256-"):
			sha256Entries = append(sha256Entries, entry)
		}
	}
	for _, entry := range sha256Entries {
		if raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(entry, "sha256-")); err == nil && len(raw) == sha256.Size {
			registryDigest = "sha256:" + hex.EncodeToString(raw)
			break
		}
	}
	switch {
	case len(sha512Entries) > 0:
		for _, entry := range sha512Entries {
			if entry == localIntegrity {
				return true, localDigest, true
			}
		}
		if registryDigest == localDigest {
			// The sha512 and sha256 entries disagree: the conflict carries
			// no registry digest.
			registryDigest = ""
		}
		return false, registryDigest, true
	case registryDigest != "":
		return registryDigest == localDigest, registryDigest, true
	default:
		return false, "", false
	}
}

// unmanagedNPMRegistry names the registry an unmanaged publication writes to:
// the declared one, or the one npm's configuration resolves for the package's
// scope. When npm cannot say, it is npm's own default.
func unmanagedNPMRegistry(run npmDryRun) string {
	if run.registry != "" {
		return run.registry
	}
	keys := []string{"registry"}
	if scope, _, scoped := strings.Cut(run.packageName, "/"); scoped && strings.HasPrefix(scope, "@") {
		keys = []string{scope + ":registry", "registry"}
	}
	result, err := runNPM(run.npmDir, run.authEnv, append([]string{"config", "get"}, keys...)...)
	if err != nil || result == nil || !result.Success {
		return defaultNPMRegistry
	}
	// npm prints the bare value for one key and one `key=value` line per key
	// for several, in the order asked.
	for _, line := range strings.Split(strings.TrimSpace(result.Stdout), "\n") {
		value := strings.TrimSpace(line)
		if _, after, found := strings.Cut(value, "="); found && len(keys) > 1 {
			value = strings.TrimSpace(after)
		}
		if value != "" && value != "undefined" && value != "null" {
			return value
		}
	}
	return defaultNPMRegistry
}

// packWithNPM packs the staged package the way `npm publish` does and returns
// the tarball's digest in the form a probe carries and its npm integrity. The
// pack runs no lifecycle script and reaches no registry, so it carries no
// credential.
func packWithNPM(run npmDryRun) (digest, integrity string, err error) {
	tempDir, err := os.MkdirTemp("", "putnami-npm-probe-")
	if err != nil {
		return "", "", fmt.Errorf("create npm pack directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()
	result, runErr := runNPM(run.npmDir, run.authEnv, "pack", "--json", "--ignore-scripts", "--pack-destination", tempDir)
	if runErr != nil {
		return "", "", fmt.Errorf("npm pack failed: %w", runErr)
	}
	if result == nil || !result.Success {
		return "", "", fmt.Errorf("npm pack failed: %s", execStderr(result))
	}
	matches, err := filepath.Glob(filepath.Join(tempDir, "*.tgz"))
	if err != nil || len(matches) != 1 {
		return "", "", fmt.Errorf("npm pack produced %d tarballs, want exactly one", len(matches))
	}
	file, err := os.Open(matches[0])
	if err != nil {
		return "", "", fmt.Errorf("open npm tarball: %w", err)
	}
	defer func() { _ = file.Close() }()
	sum256, sum512 := sha256.New(), sha512.New()
	if _, err := io.Copy(io.MultiWriter(sum256, sum512), file); err != nil {
		return "", "", fmt.Errorf("hash npm tarball: %w", err)
	}
	return fmt.Sprintf("sha256:%x", sum256.Sum(nil)), "sha512-" + base64.StdEncoding.EncodeToString(sum512.Sum(nil)), nil
}

// runNPM runs one npm command of the probe in the staged package directory,
// through the npm npmCommand resolves, and bounds it by the probe timeout.
func runNPM(npmDir string, env map[string]string, args ...string) (*exec.Result, error) {
	npmBin, err := npmCommand(npmGOOS, args)
	if err != nil {
		return nil, err
	}
	return npmExecRun(npmBin, args, append(npmExecOpts(npmDir, env), exec.Timeout(memberprobe.Timeout))...)
}
