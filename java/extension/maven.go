package java

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"go.putnami.dev/sdk/extension/pinnedarchive"
	"go.putnami.dev/sdk/extension/putnamihome"
	"go.putnami.dev/tooling/stacks"
)

// mavenVersion is the Maven the extension installs for a build that states
// none, when the machine has no mvn.
const mavenVersion = "3.9.11"

// mavenURL is where the Maven archive is published. Tests point it at a local
// server.
var mavenURL = "https://archive.apache.org/dist/maven/maven-3/" + mavenVersion + "/binaries/apache-maven-" + mavenVersion + "-bin.tar.gz"

// mavenSHA256 is the SHA-256 of the archive, whose SHA-512 Apache publishes.
var mavenSHA256 = "4b7195b6a4f5c81af4c0212677a32ee8143643401bc6e1e8412e6b06ea82beac"

// mavenWrapperProperties is the file in which the Maven wrapper states the
// distribution a build runs.
const mavenWrapperProperties = ".mvn/wrapper/maven-wrapper.properties"

// maxMavenArchive bounds a Maven distribution the extension downloads.
const maxMavenArchive = 64 << 20

// mavenDistribution is a Maven distribution a task runs.
type mavenDistribution struct {
	version string
	url     string
	format  pinnedarchive.Format
	// sha256 is the digest the build states, or the extension pins; empty,
	// the distribution is checked against the digest its host publishes.
	sha256 string
}

// mavenDistributionURL reads the version out of a distribution URL:
// .../apache-maven-3.9.6-bin.zip is 3.9.6.
var mavenDistributionURL = regexp.MustCompile(`/apache-maven-([0-9][0-9A-Za-z.\-]*?)-bin\.(zip|tar\.gz)$`)

// statedMaven answers the distribution the Maven wrapper of the build at root
// states, or false when it states none.
func statedMaven(root string) (mavenDistribution, bool, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(mavenWrapperProperties))) //nolint:gosec // the build's wrapper properties
	if errors.Is(err, os.ErrNotExist) {
		return mavenDistribution{}, false, nil
	}
	if err != nil {
		return mavenDistribution{}, false, err
	}
	props := readProperties(data)
	raw := props["distributionUrl"]
	if raw == "" {
		return mavenDistribution{}, false, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return mavenDistribution{}, false, fmt.Errorf("%s: distributionUrl %q is no http or https URL without credentials", mavenWrapperProperties, raw)
	}
	m := mavenDistributionURL.FindStringSubmatch(u.Path)
	if m == nil {
		return mavenDistribution{}, false, fmt.Errorf("%s: distributionUrl %q names no apache-maven-<version>-bin.zip or .tar.gz", mavenWrapperProperties, raw)
	}
	d := mavenDistribution{version: m[1], url: raw, format: pinnedarchive.Zip, sha256: strings.ToLower(props["distributionSha256Sum"])}
	if m[2] == "tar.gz" {
		d.format = pinnedarchive.TarGz
	}
	return d, true, nil
}

// readProperties reads a Java properties file's key=value lines, with the
// escapes a URL in one holds.
func readProperties(data []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == '!' {
			continue
		}
		i := strings.IndexAny(line, "=:")
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		value := strings.TrimSpace(line[i+1:])
		out[key] = strings.NewReplacer(`\:`, ":", `\=`, "=", `\\`, `\`).Replace(value)
	}
	return out
}

// mavenExecutable is the launcher of a distribution installed in dir.
func mavenExecutable(dir, version string) string {
	name := "mvn"
	if runtime.GOOS == "windows" {
		name = "mvn.cmd"
	}
	return filepath.Join(dir, "apache-maven-"+version, "bin", name)
}

// mavenProgram answers the mvn a step of the Maven build at root runs: the
// release its wrapper states, installed under the Putnami home, else the mvn
// on the PATH of env, else the Maven the extension pins, installed the same
// way.
func mavenProgram(ctx context.Context, root string, env []string, info func(string)) (string, error) {
	d, stated, err := statedMaven(root)
	if err != nil {
		return "", err
	}
	if !stated {
		if found, ok := stacks.LookPathIn(env, "mvn"); ok {
			return found, nil
		}
		d = mavenDistribution{version: mavenVersion, url: mavenURL, format: pinnedarchive.TarGz, sha256: mavenSHA256}
	}
	return installMaven(ctx, d, info)
}

// installMaven installs the distribution under the Putnami home, unless an
// earlier call did, and answers its launcher. A distribution with no stated
// digest is checked against the SHA-512, else the SHA-256, its host publishes
// beside it. Offline, it answers the installed one or fails.
func installMaven(ctx context.Context, d mavenDistribution, info func(string)) (string, error) {
	home := putnamihome.Resolve(os.Getenv)
	if home == "" {
		return "", fmt.Errorf("no Putnami home to install Apache Maven in: set %s", putnamihome.Env)
	}
	dir := filepath.Join(home, "toolchains", "maven", "maven-"+d.version)
	executable := mavenExecutable(dir, d.version)
	if _, err := os.Stat(executable); err == nil {
		return executable, nil
	}
	if offline() {
		return "", fmt.Errorf("the Apache Maven %s is not installed: an offline run installs nothing", d.version)
	}
	info("installing Apache Maven " + d.version)
	opts := pinnedarchive.Options{Limits: pinnedarchive.DefaultLimits(), Complete: func(candidate string) bool {
		_, err := os.Stat(mavenExecutable(candidate, d.version))
		return err == nil
	}}
	pin := pinnedarchive.Pin{URL: d.url, SHA256: d.sha256, Format: d.format}
	if d.sha256 == "" {
		verified, err := downloadPublished(ctx, d.url)
		if err != nil {
			return "", fmt.Errorf("install Apache Maven %s: %w", d.version, err)
		}
		defer func() { _ = os.Remove(verified.path) }()
		pin.SHA256 = verified.sha256
		opts.Client = &http.Client{Transport: localFile{url: d.url, path: verified.path}}
	}
	if _, err := pinnedarchive.Install(ctx, pin, dir, opts); err != nil {
		return "", fmt.Errorf("install Apache Maven %s: %w", d.version, err)
	}
	return executable, nil
}

// verifiedArchive is an archive downloaded and checked against the digest its
// host publishes.
type verifiedArchive struct {
	path   string
	sha256 string
}

// downloadPublished downloads the archive at rawURL into a temporary file and
// checks it against the SHA-512 its host publishes at <url>.sha512, else the
// SHA-256 at <url>.sha256, and answers the file with its SHA-256. An archive
// whose host publishes neither is refused.
func downloadPublished(ctx context.Context, rawURL string) (verifiedArchive, error) {
	var want string
	var sum hash.Hash
	for _, c := range []struct {
		ext  string
		size int
		hash func() hash.Hash
	}{{".sha512", sha512.Size, sha512.New}, {".sha256", sha256.Size, sha256.New}} {
		digest, err := publishedDigest(ctx, rawURL+c.ext, c.size)
		if err != nil {
			return verifiedArchive{}, err
		}
		if digest != "" {
			want, sum = digest, c.hash()
			break
		}
	}
	if want == "" {
		return verifiedArchive{}, fmt.Errorf("%s publishes no .sha512 or .sha256 checksum: state distributionSha256Sum in %s", rawURL, mavenWrapperProperties)
	}
	body, err := get(ctx, rawURL)
	if err != nil {
		return verifiedArchive{}, err
	}
	defer func() { _ = body.Close() }()
	f, err := os.CreateTemp("", "putnami-maven-*")
	if err != nil {
		return verifiedArchive{}, err
	}
	sha := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, sum, sha), io.LimitReader(body, maxMavenArchive+1))
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil && n > maxMavenArchive {
		err = fmt.Errorf("%s exceeds %d bytes", rawURL, maxMavenArchive)
	}
	if err == nil && hex.EncodeToString(sum.Sum(nil)) != want {
		err = fmt.Errorf("%s does not match the checksum its host publishes", rawURL)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return verifiedArchive{}, err
	}
	return verifiedArchive{path: f.Name(), sha256: hex.EncodeToString(sha.Sum(nil))}, nil
}

// publishedDigest answers the hex digest of size bytes a checksum file holds,
// as its first word, or "" when the host has no such file.
func publishedDigest(ctx context.Context, rawURL string, size int) (string, error) {
	body, err := get(ctx, rawURL)
	if errors.Is(err, errNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(io.LimitReader(body, 4096))
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return "", fmt.Errorf("%s is empty", rawURL)
	}
	digest := strings.ToLower(fields[0])
	if b, err := hex.DecodeString(digest); err != nil || len(b) != size {
		return "", fmt.Errorf("%s holds no %d-byte hex digest", rawURL, size)
	}
	return digest, nil
}

var errNotFound = errors.New("not found")

// get answers the body of a GET; a 404 is errNotFound.
func get(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: pinnedarchive.DefaultTimeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", rawURL, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		_ = resp.Body.Close()
		return nil, fmt.Errorf("download %s: %w", rawURL, errNotFound)
	case resp.StatusCode != http.StatusOK:
		_ = resp.Body.Close()
		return nil, fmt.Errorf("download %s: %s", rawURL, resp.Status)
	}
	return resp.Body, nil
}

// localFile serves an archive already downloaded and verified to the one URL
// it was downloaded from, so that pinnedarchive installs it without a second
// download.
type localFile struct {
	url  string
	path string
}

func (l localFile) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.String() != l.url {
		return nil, fmt.Errorf("no download of %s", req.URL)
	}
	f, err := os.Open(l.path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: f, ContentLength: info.Size(), Header: http.Header{}, Request: req}, nil
}

// adaptMaven answers a Maven step to run: a step that names mvn runs the
// release the build's wrapper states, else the mvn on PATH, else the pinned
// Maven.
func adaptMaven(ctx context.Context, root stacks.RootStep, step []string, info func(string)) ([]string, error) {
	if step[0] != "mvn" {
		return step, nil
	}
	program, err := mavenProgram(ctx, root.Dir, root.Env, info)
	if err != nil {
		return nil, err
	}
	return append([]string{program}, step[1:]...), nil
}
