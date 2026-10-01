package publish

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"

	"go.putnami.dev/go/extension/internal/releaseplan"
	extproto "go.putnami.dev/protocol/extension"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/memberprobe"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// maxGoModuleProbeZipBytes bounds how much of a served module zip a probe
// hashes. It is the size limit the go command puts on a module zip.
const maxGoModuleProbeZipBytes = 500 << 20

// reasonGoReleased is the reason of a held version outside a release set that
// the dry run cannot prove differs from the staged zip.
const reasonGoReleased = "the registry already serves this version; a Go publish outside a release set fails " +
	"when the registry has publicly released it, and a read cannot tell a released version from a private one"

// reasonGoOtherDigest is the reason of a held version whose zip differs from
// the staged one.
const reasonGoOtherDigest = "the registry already holds this version with another zip digest; it keeps those bytes " +
	"when the version is sent again, so the real publish fails its verification of the served zip"

// probeGoModule asks the registry whether it already serves modulePath at
// version and emits the answer as one member-probe event.
//
// zipPath is the staged module zip the answer is compared with, or empty when
// no staged zip is that module and version. A zip that cannot be read makes
// the probe unverified. planned selects the rule of the real publish. Both
// verify the zip the registry serves after the upload, so a held version with
// another zip digest is a conflict. Under a release-set plan a held version
// with the staged digest is identical. Outside one, the real publish fails on
// a version the registry has publicly released, which a read cannot tell from
// a private one, so any held version is a conflict.
//
// The request carries the bearer resolveGoPublishToken yields, and is
// anonymous when it yields none.
func probeGoModule(params pctx.Params, emit *jsonl.Emitter, route goPublishRoute, registryURL, modulePath, version, zipPath string, planned bool) {
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
			memberprobe.Emit(emit, subject.Unverified("the staged module zip cannot be read: "+err.Error()))
			return
		}
		subject.ArtifactDigest = digest
	}
	memberprobe.Emit(emit, askGoRegistry(subject, route.endpoint, token, planned))
}

// askGoRegistry sends the probe's one request, a GET of the standard Go proxy
// zip, and hashes a 200 body. A 404 or a 410 is absent.
func askGoRegistry(subject memberprobe.Subject, endpoint, token string, planned bool) extproto.MemberProbe {
	client, err := newGoRegistryHTTPClient()
	if err != nil {
		return subject.Unverified(err.Error())
	}
	client.Timeout = memberprobe.Timeout
	resp, err := getGoModuleZip(client, endpoint, token, subject.Coordinate, subject.Version)
	if err != nil {
		if sendErr := (*url.Error)(nil); errors.As(err, &sendErr) {
			return subject.Unreachable(err)
		}
		return subject.Unverified(err.Error())
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
		registryDigest := fmt.Sprintf("sha256:%x", hash.Sum(nil))
		if !planned && (subject.ArtifactDigest == "" || subject.ArtifactDigest == registryDigest) {
			return subject.Conflict(registryDigest, reasonGoReleased)
		}
		return subject.HeldWith(registryDigest, reasonGoOtherDigest)
	case http.StatusNotFound, http.StatusGone:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxGoRegistryResponseBytes))
		return subject.Absent()
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxGoRegistryResponseBytes))
		return subject.Refused(resp.StatusCode, memberprobe.Redact(string(body), token))
	}
}

// stagedGoZip returns the zip an earlier package staged when its metadata
// names modulePath at version, or empty otherwise.
func stagedGoZip(wsRoot, projectPath, modulePath, version string) string {
	staged, err := pkgmeta.ReadGoModuleMetadata(wsRoot, projectPath)
	if err != nil || staged.ModulePath != modulePath || staged.Version != version {
		return ""
	}
	return staged.ZipPath
}

// stagedGoModule returns the module a publication would write.
//
// A real publish reads the staged module metadata. A dry run reads the module
// path and version from the go channel record, which a dry-run package writes,
// and keeps the staged metadata, and with it the zip to compare, only when it
// names that same module and version.
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
