package workspacejob

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.putnami.dev/sdk/extension/pinnedarchive"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// LockFileName is the workspace lock that pins the Go toolchain a managed
// install downloads, with one SHA-256 per platform.
const LockFileName = "putnami.lock.json"

// GoLock is the Go toolchain pin of the workspace lock.
type GoLock struct {
	// Version is the exact Go release, such as "1.26.1".
	Version string `json:"version"`
	// Integrities maps "<goos>/<goarch>" to the SHA-256 of that platform's
	// archive, as 64 hexadecimal characters.
	Integrities map[string]string `json:"integrities"`
	// Source is the URL the archives are published under, such as
	// "https://go.dev/dl/".
	Source string `json:"source"`
}

// ReadGoLock returns the Go toolchain pin of the workspace lock at path.
//
// Only toolchains.go is decoded. The CLI validates the whole document when it
// writes it; this reader must not refuse a lock a newer CLI wrote because the
// document grew a field the extension does not know.
func ReadGoLock(path string) (GoLock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return GoLock{}, err
	}
	var doc struct {
		Toolchains map[string]json.RawMessage `json:"toolchains"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return GoLock{}, fmt.Errorf("parse %s: %w", path, err)
	}
	raw, ok := doc.Toolchains["go"]
	if !ok || string(raw) == "null" {
		return GoLock{}, fmt.Errorf("%s pins no Go toolchain (toolchains.go)", path)
	}
	var entry GoLock
	if err := json.Unmarshal(raw, &entry); err != nil {
		return GoLock{}, fmt.Errorf("parse toolchains.go in %s: %w", path, err)
	}
	entry.Version = NormalizeGoVersion(strings.TrimSpace(entry.Version))
	if entry.Version == "" {
		return GoLock{}, fmt.Errorf("%s pins no Go version (toolchains.go.version)", path)
	}
	return entry, nil
}

// GoArchivePin is the archive of lock's Go release for goos/goarch: the Go
// distribution names it go<version>.<goos>-<goarch>, a zip on Windows and a
// gzip-compressed tar everywhere else.
func GoArchivePin(lock GoLock, goos, goarch string) pinnedarchive.Pin {
	format, extension := pinnedarchive.TarGz, ".tar.gz"
	if goos == "windows" {
		format, extension = pinnedarchive.Zip, ".zip"
	}
	source := lock.Source
	if source == "" {
		source = "https://go.dev/dl/"
	}
	if !strings.HasSuffix(source, "/") {
		source += "/"
	}
	return pinnedarchive.Pin{
		URL:    source + "go" + lock.Version + "." + goos + "-" + goarch + extension,
		SHA256: lock.Integrities[goos+"/"+goarch],
		Format: format,
	}
}

// goInstallComplete reports whether dir holds a whole Go distribution of
// version: its go command and the VERSION file naming that release.
func goInstallComplete(version string) func(dir string) bool {
	return func(dir string) bool {
		binary := filepath.Join(dir, "go", "bin", pkgmeta.ExecutableName(runtime.GOOS, "go"))
		if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() {
			return false
		}
		file, err := os.Open(filepath.Join(dir, "go", "VERSION"))
		if err != nil {
			return false
		}
		defer func() { _ = file.Close() }()
		scanner := bufio.NewScanner(file)
		return scanner.Scan() && strings.TrimSpace(scanner.Text()) == "go"+version
	}
}

// installLockedGo installs the Go release the workspace lock pins and returns
// its go command. The archive is downloaded only when its SHA-256 is pinned
// for this platform, and it is refused unless it matches, so the Go this job
// installs is always the exact bytes the lock names.
//
// The release is installed under GoToolchainRoot, once for every workspace of
// the machine that pins it: a complete install there is returned without a
// download, and installers that start together, from one workspace or from
// several, exclude each other on the lock file beside the install directory,
// so one of them downloads and the others return its install.
//
// The release is the lock's, not the workspace's requested minimum: it must
// satisfy the request, and a lock that pins an older release is refused.
func (j *Job) installLockedGo(requested string) (string, bool) {
	lockPath := filepath.Join(j.WorkspaceRoot, LockFileName)
	lock, err := ReadGoLock(lockPath)
	if err != nil {
		version := requested
		if version == "" {
			version = "(unpinned)"
		}
		j.Emit.Log("info", "Installing Go "+version+"...")
		j.Emit.Diagnostic("error", "Cannot install a verified Go toolchain: "+err.Error()+
			"; run `putnami install` again once the workspace lock pins one", "", 0)
		j.Emit.Diagnostic("error", "Failed to install Go "+version+".", "", 0)
		return "", false
	}
	version := lock.Version
	if requested != "" && !GoVersionSatisfies(version, requested) {
		j.Emit.Log("info", "Installing Go "+requested+"...")
		j.Emit.Diagnostic("error", fmt.Sprintf(
			"Cannot install Go %s: %s pins Go %s, which is older than the workspace requires",
			requested, LockFileName, version), lockPath, 0)
		j.Emit.Diagnostic("error", "Failed to install Go "+requested+".", "", 0)
		return "", false
	}

	root := j.GoToolchainRoot()
	dest := managedGoDir(root, version)
	binary := managedGoBinary(root, version)
	complete := goInstallComplete(version)
	if complete(dest) {
		return binary, true
	}

	j.Emit.Log("info", "Installing Go "+version+"...")
	if !j.downloadGo(lock, dest, binary, complete) {
		j.Emit.Diagnostic("error", "Failed to install Go "+version+".", "", 0)
		return "", false
	}
	return binary, true
}

// downloadGo makes dest hold the Go release lock pins, whose go command is
// binary, and reports whether it does. It emits the reason when it does not.
func (j *Job) downloadGo(lock GoLock, dest, binary string, complete func(string) bool) bool {
	pin := GoArchivePin(lock, runtime.GOOS, runtime.GOARCH)
	platform := runtime.GOOS + "/" + runtime.GOARCH
	if strings.TrimSpace(pin.SHA256) == "" {
		j.Emit.Diagnostic("error", fmt.Sprintf(
			"Refusing to download Go %s: %s records no SHA-256 for %s", lock.Version, LockFileName, platform),
			filepath.Join(j.WorkspaceRoot, LockFileName), 0)
		return false
	}

	pin, client, err := withoutCredentials(pin)
	if err != nil {
		j.Emit.Diagnostic("error", fmt.Sprintf(
			"Refusing to download Go %s: the source in %s is not a valid URL", lock.Version, LockFileName),
			filepath.Join(j.WorkspaceRoot, LockFileName), 0)
		return false
	}

	j.Emit.Log("info", "Downloading Go "+lock.Version+" from "+pin.URL+"...")
	_, err = pinnedarchive.Install(j.Ctx, pin, dest, pinnedarchive.Options{
		Client:    client,
		UserAgent: j.UserAgent(),
		Complete:  complete,
	})
	j.trap.check()
	switch {
	case err == nil:
		j.Emit.Log("info", "Go "+lock.Version+" installed successfully")
		return true
	case errors.Is(err, pinnedarchive.ErrNoDigest):
		j.Emit.Diagnostic("error", fmt.Sprintf(
			"Refusing to download Go %s: the %s SHA-256 in %s is not valid (%v)",
			lock.Version, platform, LockFileName, err), filepath.Join(j.WorkspaceRoot, LockFileName), 0)
	case errors.Is(err, pinnedarchive.ErrDigestMismatch):
		j.Emit.Diagnostic("error", fmt.Sprintf(
			"Refusing Go %s from %s: %v", lock.Version, pin.URL, err), filepath.Join(j.WorkspaceRoot, LockFileName), 0)
	case errors.Is(err, pinnedarchive.ErrIncomplete):
		j.Emit.Diagnostic("error", "Go binary not found at "+binary+" after extraction", "", 0)
	case errors.Is(err, pinnedarchive.ErrUnsafeEntry), errors.Is(err, pinnedarchive.ErrTooLarge):
		j.Emit.Diagnostic("error", "Failed to extract Go archive: "+err.Error(), "", 0)
	default:
		j.Emit.Diagnostic("error", fmt.Sprintf("Failed to download Go %s from %s: %v", lock.Version, pin.URL, err), "", 0)
	}
	return false
}

// withoutCredentials returns pin with the userinfo of its URL removed, and the
// client to download it with. A URL that carries userinfo gets a client that
// sends it as basic authentication with every request to the URL's scheme and
// host, a redirect's included, and with no request to any other, as curl does
// with the userinfo of a URL; any other URL gets nil, pinnedarchive's default
// client. Every URL the job logs, and every error pinnedarchive returns, then
// names the archive without its credentials. A URL that does not parse is
// refused without being repeated, since it may hold them.
func withoutCredentials(pin pinnedarchive.Pin) (pinnedarchive.Pin, *http.Client, error) {
	parsed, err := url.Parse(pin.URL)
	if err != nil {
		return pinnedarchive.Pin{}, nil, errors.New("the archive URL does not parse")
	}
	if parsed.User == nil {
		return pin, nil, nil
	}
	transport := basicAuthTransport{scheme: parsed.Scheme, host: parsed.Host, user: parsed.User, base: http.DefaultTransport}
	parsed.User = nil
	pin.URL = parsed.String()
	return pin, &http.Client{Timeout: pinnedarchive.DefaultTimeout, Transport: transport}, nil
}

// basicAuthTransport sends user as basic authentication with every request to
// scheme and host that carries no Authorization header of its own.
type basicAuthTransport struct {
	scheme, host string
	user         *url.Userinfo
	base         http.RoundTripper
}

func (t basicAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.EqualFold(req.URL.Scheme, t.scheme) && strings.EqualFold(req.URL.Host, t.host) && req.Header.Get("Authorization") == "" {
		password, _ := t.user.Password()
		req = req.Clone(req.Context())
		req.SetBasicAuth(t.user.Username(), password)
	}
	return t.base.RoundTrip(req)
}
