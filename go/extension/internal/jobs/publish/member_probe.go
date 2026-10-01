package publish

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"

	"golang.org/x/mod/module"

	"go.putnami.dev/go/extension/internal/releaseplan"
	extproto "go.putnami.dev/protocol/extension"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/memberprobe"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// maxGoModuleProbeZipBytes bounds how much of a served module zip a probe
// hashes. It is the size limit the go command itself puts on a module zip, so
// a larger answer is not a module a publication could have written.
const maxGoModuleProbeZipBytes = 500 << 20

// probeGoModule asks the registry whether it already serves the module at the
// version a publish would write, and reports the answer as one member-probe
// event. It is the only registry request a dry run sends.
//
// zipPath is the staged module zip, or empty when the dry run built none: a
// planned dry run packages nothing, so a version the registry already serves
// is then a conflict the dry run cannot compare.
//
// The bearer is sent when one of the publisher's own sources yields it and the
// request is anonymous otherwise. A missing credential never fails the probe:
// the registry's answer to the anonymous request is what gets reported.
func probeGoModule(params pctx.Params, emit *jsonl.Emitter, route goPublishRoute, registryURL, modulePath, version, zipPath string) {
	token, _ := resolveGoPublishToken(params, route.credentialHost)
	subject := memberprobe.Subject{
		Ecosystem:  string(releaseplan.GoEcosystem),
		Coordinate: modulePath,
		Version:    version,
		Registry:   registryURL,
		Anonymous:  token == "",
	}
	if zipPath != "" {
		digest, err := sha256OfFile(zipPath)
		if err != nil {
			emit.Warn("Dry run: the staged module zip could not be read, so the registry copy cannot be compared: " + err.Error())
		}
		subject.ArtifactDigest = digest
	}
	memberprobe.Emit(emit, askGoRegistry(subject, route.endpoint, token))
}

// askGoRegistry sends the probe's one request: a GET of the standard Go proxy
// zip, the immutable bytes a consumer downloads. A 404 or a 410 means the
// version does not exist. A 200 is hashed, so the verdict compares the bytes
// the registry serves rather than a digest it claims.
func askGoRegistry(subject memberprobe.Subject, endpoint, token string) extproto.MemberProbe {
	escapedPath, err := module.EscapePath(subject.Coordinate)
	if err != nil {
		return subject.Unverified(fmt.Sprintf("the module path cannot be escaped: %v", err))
	}
	escapedVersion, err := module.EscapeVersion(subject.Version)
	if err != nil {
		return subject.Unverified(fmt.Sprintf("the module version cannot be escaped: %v", err))
	}
	client, err := newGoRegistryHTTPClient()
	if err != nil {
		return subject.Unverified(err.Error())
	}
	client.Timeout = memberprobe.Timeout
	req, err := http.NewRequest(http.MethodGet, endpoint+"/"+escapedPath+"/@v/"+escapedVersion+".zip", nil)
	if err != nil {
		return subject.Unverified("the Go registry request could not be built")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req) //nolint:gosec // registryURL is validated from explicit publish configuration
	if err != nil {
		return subject.Unreachable(err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		hash := sha256.New()
		read, err := io.Copy(hash, io.LimitReader(resp.Body, maxGoModuleProbeZipBytes+1))
		if err != nil {
			return subject.Unreachable(err)
		}
		if read > maxGoModuleProbeZipBytes {
			return subject.Unverified(fmt.Sprintf("the registry serves a module zip larger than %d bytes", maxGoModuleProbeZipBytes))
		}
		return subject.Held(fmt.Sprintf("sha256:%x", hash.Sum(nil)))
	case http.StatusNotFound, http.StatusGone:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxGoRegistryResponseBytes))
		return subject.Absent()
	default:
		// The body is upstream-controlled and may echo the request's
		// Authorization, so it is bounded and redacted before it is shown.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxGoRegistryResponseBytes))
		return subject.Refused(resp.StatusCode, memberprobe.Redact(string(body), token))
	}
}

// stagedGoModule returns the module a publication would write.
//
// A real publish reads the metadata package staged beside the zip. A dry-run
// package stages nothing: it writes only the go channel record, which names
// the module path and the version it would stage. A dry run therefore takes
// the module from that record, and keeps the staged metadata, and with it the
// zip to compare, only when it describes that same module and version. A zip
// left by an earlier run at another version says nothing about this one.
func stagedGoModule(channels *pkgmeta.ChannelIndex, wsRoot, projectPath string, dryRun bool) (*pkgmeta.GoModuleMetadata, error) {
	staged, err := pkgmeta.ReadGoModuleMetadata(wsRoot, projectPath)
	if !dryRun {
		return staged, err
	}
	record, recorded := channels.Records["go"]
	if !recorded || record.Artifact == "" || record.Version == "" {
		return staged, err
	}
	if err == nil && staged.ModulePath == record.Artifact && staged.Version == record.Version {
		return staged, nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return &pkgmeta.GoModuleMetadata{ModulePath: record.Artifact, Version: record.Version}, nil
}

// sha256OfFile returns the digest of a file in the form a member carries.
func sha256OfFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
}
