package extension

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/sdk/extension/pinnedarchive"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// RegistryRef converts an extension or template name (e.g. "@putnami/go")
// into its registry namespace and package components ("putnami", "go").
// Unscoped names default to the "putnami" namespace.
func RegistryRef(name string) (namespace, pkg string) {
	name = strings.TrimPrefix(name, "@")
	if i := strings.Index(name, "/"); i >= 0 {
		return name[:i], name[i+1:]
	}
	return "putnami", name
}

// registryDownloadURL is the download route of name on the registry at base.
// The namespace and the package travel as escaped path segments, so a name
// holds nothing the route or its query reads.
func registryDownloadURL(base, name string, params url.Values) string {
	ns, pkg := RegistryRef(name)
	return fmt.Sprintf("%s/%s/%s/download?%s", base, url.PathEscape(ns), url.PathEscape(pkg), params.Encode())
}

// download fetches the artifact archive from the registry.
// Returns the path to the downloaded file, the resolved version, and the
// SHA-256 hash the resolver advertised for the archive (empty string when
// the resolver did not provide an integrity header).
func (inst *Installer) download(ctx context.Context, name, constraint string, spec ArtifactSpec) (string, string, string, error) {
	resp, _, err := inst.openArtifact(ctx, name, constraint, spec, false)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()

	resolvedVersion, err := artifactResolvedVersion(resp.Header.Get("X-Resolved-Version"), constraint)
	if err != nil {
		return "", "", "", fmt.Errorf("download %s: %w", name, err)
	}
	advertisedIntegrity := pinnedarchive.ReadAdvertisedIntegrity(resp.Header)

	// Write to temp file
	tmpFile, err := os.CreateTemp("", spec.ArchivePattern)
	if err != nil {
		return "", "", "", fmt.Errorf("download %s: create temp file: %w", name, err)
	}
	defer tmpFile.Close()

	// Cap the download size to prevent disk exhaustion from malicious archives
	limited := io.LimitReader(resp.Body, spec.MaxArchiveSize)
	if _, err := io.Copy(tmpFile, &contextReader{ctx: ctx, reader: limited}); err != nil {
		os.Remove(tmpFile.Name())
		return "", "", "", fmt.Errorf("download %s: save archive: %w", name, err)
	}

	return tmpFile.Name(), resolvedVersion, advertisedIntegrity, nil
}

// ResolveArtifact reads the version and digest the registry resolves for name
// at constraint, without downloading the archive.
func (inst *Installer) ResolveArtifact(ctx context.Context, name, constraint string, spec ArtifactSpec) (*InstallResult, error) {
	resp, source, err := inst.openArtifact(ctx, name, constraint, spec, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	version, err := artifactResolvedVersion(resp.Header.Get("X-Resolved-Version"), constraint)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", name, err)
	}
	return &InstallResult{
		Name:      name,
		Version:   version,
		Integrity: pinnedarchive.ReadAdvertisedIntegrity(resp.Header),
		Source:    source,
	}, nil
}

// ResolveArtifactIntegrity asks the registry for the archive digest it
// advertises for name at an exact version on the platform named by spec
// (spec.OS/spec.Arch), WITHOUT downloading the archive body.
//
// This is the cross-platform seam used when refreshing a lock that pins several
// platforms. A digest for an os/arch other than the one the CLI runs on
// is REGISTRY-ASSERTED by construction: no foreign bytes exist locally to hash,
// so the only thing streaming the archive would add is a consistency check of
// the registry against itself — while making every version bump pay a full
// (tens of MB, up to MaxArchiveSize) download per extra platform. So only the
// advertised header is read: the lookup sends HEAD, and a registry that refuses
// HEAD gets a GET whose body is closed unread.
//
// The LOCAL platform's digest keeps its strictly stronger guarantee and never
// comes from here: InstallArtifact hashes the bytes it actually downloaded and
// runs them through verifyArchiveIntegrity before they may enter the
// machine-global store. A carried-over foreign digest only ever gates a future
// install ON that platform, where it is checked against locally hashed bytes;
// it can never admit unverified bytes to the shared store on this machine.
func (inst *Installer) ResolveArtifactIntegrity(ctx context.Context, name, version string, spec ArtifactSpec) (string, error) {
	// Digests are bound to (name, version, platform), so there is nothing to
	// resolve without an exact version to pin the channel to. A moving channel
	// name ("latest") or the "0.0.0" unresolved sentinel would bind the digest
	// to whatever the registry happens to serve, which is precisely the stale
	// digest this refresh exists to avoid — so refuse before touching the
	// network and let the caller fail soft.
	if _, err := ParseVersion(version); err != nil || !isPinnableVersion(version) {
		return "", fmt.Errorf("no exact version to pin (got %q)", version)
	}

	resp, _, err := inst.openArtifact(ctx, name, version, spec, true)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// If the registry served some other version for this version-pinned channel,
	// its digest belongs to that version — recording it under `version` would
	// write exactly the stale digest this refresh exists to avoid.
	if got := resp.Header.Get("X-Resolved-Version"); got != "" && got != version {
		return "", fmt.Errorf("registry resolved %s, not %s", got, version)
	}

	advertised := pinnedarchive.ReadAdvertisedIntegrity(resp.Header)
	if advertised == "" {
		return "", fmt.Errorf("registry advertised no integrity for %s/%s", spec.OS, spec.Arch)
	}
	digest, err := pinnedarchive.NormalizeIntegrity(advertised)
	if err != nil {
		return "", fmt.Errorf("invalid advertised integrity for %s/%s: %w", spec.OS, spec.Arch, err)
	}
	return digest, nil
}

// openArtifact requests the archive of name at constraint from the registry.
// With headersOnly it sends HEAD, so the caller reads the resolved version and
// digest and the registry sends no archive.
func (inst *Installer) openArtifact(ctx context.Context, name, constraint string, spec ArtifactSpec, headersOnly bool) (*http.Response, string, error) {
	if err := ValidateRegistryURL(inst.ResolverURL); err != nil {
		return nil, "", err
	}
	params := url.Values{}
	params.Set("channel", constraint)
	params.Set("os", spec.OS)
	params.Set("arch", spec.Arch)
	dlURL := registryDownloadURL(inst.ResolverURL, name, params)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create request: %w", err)
	}
	// The put projection of a channel can be private. Send the user's credential
	// when the cloud resolves one; without it the request stays anonymous and the
	// registry decides.
	missing := ""
	if !inst.AnonymousRegistry {
		if missing, err = AuthorizeRegistryRequest(req); err != nil {
			return nil, "", fmt.Errorf("download %s: %w", name, err)
		}
	}

	send := DoWithRetry
	if headersOnly {
		send = DoHeadWithRetry
	}
	resp, err := send(ctx, inst.HTTPClient, req)
	if err != nil {
		return nil, "", fmt.Errorf("download %s: %w", name, err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		// A 404 keeps the fuller account below, which already names the missing
		// credential among its causes, except on a hosted run: its request
		// without a credential fails with ErrHostedRegistryCredential.
		if resp.StatusCode != http.StatusNotFound || runcredential.Hosted() {
			if authErr := RegistryRefusalError(inst.ResolverURL, resp.StatusCode, missing); authErr != nil {
				return nil, "", fmt.Errorf("download %s: %w", name, authErr)
			}
		}
		// Read the header off the request, not inst.AnonymousRegistry: that flag
		// means "skip credential discovery", and discovery can also come back
		// empty on a machine that simply holds no token. What the registry
		// answered is a function of what it was sent.
		authorized := req.Header.Get("Authorization") != ""
		return nil, "", inst.artifactFetchError(name, constraint, params, spec, resp.StatusCode, authorized, missing)
	}
	return resp, registryDownloadURL(inst.ResolverURL, name, url.Values{"channel": {constraint}}), nil
}

// artifactFetchError names everything needed to reproduce a failed fetch in one
// line: the extension, the channel that was asked for, the platform, the status,
// the URL, and whether the request carried a credential.
//
// That last fact is the one that used to cost a whole investigation. A pinned
// version that a credentialed reader downloads fine and an anonymous one cannot
// breaks only the credential-free gate, so the failure reads as "the
// registry is broken" to whoever holds a token and as "the repository is broken"
// to whoever does not.
//
// The URL is rebuilt from a redacted resolver base rather than reusing the
// request URL, so no userinfo or signed query in PUTNAMI_REGISTRY_URL can reach
// the message. The query it does carry — channel, os, arch — is built here from
// the arguments and is credential-free by construction.
func (inst *Installer) artifactFetchError(name, constraint string, params url.Values, spec ArtifactSpec, status int, authorized bool, missing string) error {
	endpoint := RedactRegistryURL(inst.ResolverURL)
	safeURL := "an unparseable registry URL"
	if endpoint != "" {
		safeURL = registryDownloadURL(endpoint, name, params)
	}

	credential := "the request was anonymous"
	if missing != "" {
		credential += ": " + missing
	}
	if authorized {
		credential = "the request carried a credential"
	}

	err := fmt.Errorf("download %s at %s for %s/%s: HTTP %d from %s (%s)",
		name, constraint, spec.OS, spec.Arch, status, safeURL, credential)
	if status == http.StatusNotFound {
		// 404 is the one status that says nothing about why. Listing the three
		// causes is what turns it into a next step instead of a retry.
		err = fmt.Errorf("%w: the registry serves no archive at this pin — it was "+
			"withdrawn, never published for this platform, or is visible only to an "+
			"account this request did not identify", err)
	}
	return err
}

// artifactResolvedVersion returns the version the registry resolved, read from
// its X-Resolved-Version header. That version names the install directory
// (layout.ArtifactDir), which is removed and extracted into before the manifest
// hash is checked, so a header that is not a version is refused: a value such
// as "1.0.0-x/../../victim" would otherwise point that directory anywhere.
func artifactResolvedVersion(resolvedVersion string, constraint string) (string, error) {
	if resolvedVersion == "" {
		// Fallback: try to infer from constraint if it's an exact version
		v, err := ParseVersion(constraint)
		if err == nil {
			return v.String(), nil
		}
		return "0.0.0", nil
	}
	if err := checkArtifactVersion(resolvedVersion); err != nil {
		return "", fmt.Errorf("registry resolved version %q: %w", resolvedVersion, err)
	}
	return resolvedVersion, nil
}

// checkArtifactVersion refuses a version that cannot name a directory.
// ParseVersion accepts any prerelease text, so the prerelease is also held to
// the semver identifier and build characters [0-9A-Za-z.+-], which exclude
// every path separator.
func checkArtifactVersion(version string) error {
	v, err := ParseVersion(version)
	if err != nil {
		return fmt.Errorf("not a version: %w", err)
	}
	for _, r := range v.Prerelease {
		if (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && r != '.' && r != '-' && r != '+' {
			return fmt.Errorf("not a version: invalid prerelease %q", v.Prerelease)
		}
	}
	return nil
}

// ExtractTarGz extracts a .tar.gz archive to the destination directory.
//
// All filesystem operations are sandboxed inside destDir via os.Root, so
// archive entries cannot escape via "..", absolute paths, or symlink traversal.
// An entry whose name is unsafe fails the extraction (archiveEntryPath).
// Symlinks with absolute targets are rejected outright; relative-target
// symlinks are further validated to ensure the resolved target stays inside
// destDir. Regular files are opened through the Root, so a symlink planted
// earlier in the archive cannot redirect a later file write outside destDir.
// On Windows a contained symlink fails the extraction instead (extractSymlink).
func ExtractTarGz(archivePath, destDir string) (err error) {
	return ExtractTarGzContext(context.Background(), archivePath, destDir)
}

// ExtractTarGzContext is ExtractTarGz with cancellation checkpoints around
// every archive entry and each streamed file chunk. It never abandons a
// background extractor: cancellation is observed by the same goroutine before
// it performs more writes, and Admit removes its private staging directory.
func ExtractTarGzContext(ctx context.Context, archivePath, destDir string) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(&contextReader{ctx: ctx, reader: f})
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer func() {
		if closeErr := gz.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close gzip reader: %w", closeErr)
		}
	}()

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create dest dir: %w", err)
	}
	root, err := os.OpenRoot(destDir)
	if err != nil {
		return fmt.Errorf("open extraction root: %w", err)
	}
	defer func() { _ = root.Close() }()

	const maxTotalExtractSize int64 = 1024 * 1024 * 1024 // 1GB total extraction limit
	var totalExtracted int64

	tr := tar.NewReader(gz)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}

		name, err := archiveEntryPath(hdr.Name)
		if err != nil {
			return err
		}

		// Skip macOS resource fork files (._*) and .DS_Store
		base := filepath.Base(name)
		if strings.HasPrefix(base, "._") || base == ".DS_Store" {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return fmt.Errorf("create dir %s: %w", name, err)
			}

		case tar.TypeReg:
			const maxFileSize int64 = 100 * 1024 * 1024 // 100MB per file
			if hdr.Size > maxFileSize {
				return fmt.Errorf("file %s exceeds %dMB size limit (%d bytes)", name, maxFileSize/(1024*1024), hdr.Size)
			}
			if dir := filepath.Dir(name); dir != "." {
				if err := root.MkdirAll(dir, 0o755); err != nil {
					return fmt.Errorf("create parent dir: %w", err)
				}
			}
			perm := os.FileMode(uint32(hdr.Mode) & 0o777)
			// Root.OpenFile refuses to follow symlinks that escape the root,
			// so a malicious symlink planted earlier in the archive cannot
			// redirect this write outside destDir.
			out, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
			if err != nil {
				return fmt.Errorf("create file %s: %w", name, err)
			}

			// Limit extraction size to prevent zip bombs even if header lies
			limited := io.LimitReader(&contextReader{ctx: ctx, reader: tr}, maxFileSize+1)
			n, err := io.Copy(out, limited)
			closeErr := out.Close()
			if err != nil {
				return fmt.Errorf("write file %s: %w", name, err)
			}
			if closeErr != nil {
				return fmt.Errorf("close file %s: %w", name, closeErr)
			}
			if n > maxFileSize {
				_ = root.Remove(name)
				return fmt.Errorf("file %s exceeds %dMB size limit during extraction", name, maxFileSize/(1024*1024))
			}
			totalExtracted += n
			if totalExtracted > maxTotalExtractSize {
				return fmt.Errorf("total extraction size exceeds %dMB limit", maxTotalExtractSize/(1024*1024))
			}

		case tar.TypeLink:
			// A hard link is refused rather than skipped: skipping it installs
			// a tree without the file and reports success.
			return fmt.Errorf("archive entry %s is a hard link to %s: Putnami does not extract hard links, "+
				"so the publisher must ship a regular file at that path",
				filepath.ToSlash(name), hdr.Linkname)

		case tar.TypeSymlink:
			if err := extractArchiveSymlink(root, name, hdr.Linkname); err != nil {
				return err
			}
		}
	}
	return nil
}

// archiveEntryPath returns the path, relative to the extraction root, that the
// archive entry raw names. An unsafe name fails the extraction and names its
// entry rather than being skipped, for the same reason as a hard link.
//
// The checks are the same on every platform, so one archive is accepted or
// refused identically everywhere: a backslash counts as a separator and a
// drive prefix is refused even on Unix, because Windows reads them so.
// filepath.IsLocal then adds the running platform's own checks, such as
// Windows reserved device names and colons.
func archiveEntryPath(raw string) (string, error) {
	refuse := func(why string) (string, error) {
		return "", fmt.Errorf("archive entry %s %s: Putnami extracts only relative paths that stay inside "+
			"the archive, so the publisher must rename that entry", raw, why)
	}
	if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, `\`) {
		return refuse("is an absolute or UNC path")
	}
	if len(raw) >= 2 && raw[1] == ':' && ('a' <= raw[0] && raw[0] <= 'z' || 'A' <= raw[0] && raw[0] <= 'Z') {
		return refuse("starts with a Windows drive")
	}
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return refuse("has a .. component")
		}
	}
	name := filepath.Clean(raw)
	if !filepath.IsLocal(name) {
		return refuse("is not a local path on this platform")
	}
	return name, nil
}

// extractArchiveSymlink creates the archive's symbolic link at name, a path
// relative to root, once its target is known to stay inside root.
func extractArchiveSymlink(root *os.Root, name, target string) error {
	// Absolute symlink targets can never be safe — they escape destDir the
	// moment they are followed, even though Root.Symlink itself would not
	// validate oldname. Resolve a relative target as it will be evaluated from
	// the symlink's parent directory, and verify it stays inside destDir.
	// Either escape is refused rather than skipped, for the same reason as a
	// hard link.
	resolved := filepath.Clean(filepath.Join(filepath.Dir(name), target))
	if filepath.IsAbs(target) || resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
		return fmt.Errorf("archive entry %s is a symbolic link to %s, which leaves the extracted tree: "+
			"the publisher must ship a link that stays inside the archive, or a regular file",
			filepath.ToSlash(name), target)
	}
	// The lexical check above holds only when every directory it walks is a
	// real one. A .. after a descending component can climb out of a link
	// (`d -> .` makes `d/..` the tree's parent), so .. may only lead the
	// target, where it walks up the link's own parent directories.
	descended := false
	for _, part := range strings.Split(filepath.ToSlash(target), "/") {
		switch {
		case part == ".." && descended:
			return fmt.Errorf("archive entry %s is a symbolic link to %s, whose .. follows a descending component "+
				"and can climb out of the extracted tree through another link: the publisher must ship a link "+
				"whose .. components lead its target", filepath.ToSlash(name), target)
		case part != ".." && part != "." && part != "":
			descended = true
		}
	}
	// Those parent directories must be real: os.Root follows a link extracted
	// earlier, so with `d -> .` the entry `d/l -> ../x` would become `l -> ../x`
	// at the top of the tree, outside it.
	if dir := filepath.Dir(name); dir != "." {
		prefix := ""
		for _, part := range strings.Split(dir, string(filepath.Separator)) {
			prefix = filepath.Join(prefix, part)
			info, err := root.Lstat(prefix)
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err != nil {
				return fmt.Errorf("inspect parent dir %s: %w", filepath.ToSlash(prefix), err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("archive entry %s is a symbolic link created through the symbolic link %s, "+
					"which can place it outside the extracted tree: the publisher must ship the link under "+
					"its real directory", filepath.ToSlash(name), filepath.ToSlash(prefix))
			}
		}
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create parent dir: %w", err)
		}
	}
	return extractSymlink(root, name, target)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
