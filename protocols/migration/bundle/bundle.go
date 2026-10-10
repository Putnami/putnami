// Package bundle is the wire contract for shipping a migration bundle as one
// registry package: a single deterministic tar of the bundle directory, stored
// as one content-addressed blob, plus a small JSON manifest that points at it.
//
// Both sides of the wire use it, so they cannot drift: a publisher calls Pack,
// uploads the tar and publishes a Manifest; a migration runner downloads the
// blob the Manifest names and calls Unpack to recreate the exact bundle
// directory it then applies.
//
// The package imports only the standard library. It holds no registry client
// and no database, so a dependency-light publisher can import it cheaply. The
// bundle format itself, its validation and its digest belong to the parent
// package, go.putnami.dev/protocol/migration.
package bundle

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	// BlobMediaType is the content type of the bundle tar blob.
	BlobMediaType = "application/vnd.putnami.migration-bundle.v1.tar"
	// ManifestMediaType is the media type of the manifest payload.
	ManifestMediaType = "application/vnd.putnami.migration-bundle.v1+json"
	// Namespace is the default registry namespace of a migration bundle. A
	// consumer that receives a bundle reference without a namespace uses it.
	Namespace = "migrations"
)

// namespacePattern holds a namespace to one safe path segment. The leading
// character is alphanumeric, so a namespace is never "." or "..".
var namespacePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// ValidNamespace reports whether ns is a well-formed registry namespace: one
// lower-case path segment of at most 128 characters that starts with a letter
// or a digit. A publisher and a consumer check a namespace against it before
// either uses it as a registry path segment.
func ValidNamespace(ns string) bool { return namespacePattern.MatchString(ns) }

// PackageName derives the registry package name of an application's migration
// bundle. Application names are path-style ("billing/workloads/api"), while a
// registry package name is a single path segment, so every slash becomes a dash.
func PackageName(app string) string {
	return strings.ReplaceAll(app, "/", "-")
}

// Manifest is the registry manifest payload of a published migration bundle.
// It carries no SQL: only identity plus the one blob the tar is stored at, so
// a consumer resolves a channel, then the manifest, then one blob download.
type Manifest struct {
	// Protocol echoes the bundle protocol ("migration-bundle.v1").
	Protocol string `json:"protocol"`
	// AppName is the application the bundle belongs to.
	AppName string `json:"appName"`
	// BundleDigest is the bundle's content address, the bundle.json "digest".
	BundleDigest string `json:"bundleDigest"`
	// Artifact names the uploaded tar blob.
	Artifact Artifact `json:"artifact"`
}

// Artifact references the uploaded bundle tar by its registry blob digest.
type Artifact struct {
	Blob      string `json:"blob"`      // registry blob digest returned by the upload
	MediaType string `json:"mediaType"` // BlobMediaType
	Size      int64  `json:"size"`      // tar byte length
}

// Pack writes every regular file under fsys into a single deterministic tar.
// Entries are sorted by path and carry mode 0600 and a zero modification time,
// so the same bundle directory always packs to the same bytes and the same
// blob digest. Paths are forward-slashed and relative to the bundle root, for
// example "bundle.json" or "payload/sql/default/iam/001.up.sql".
func Pack(fsys fs.FS) ([]byte, error) {
	var paths []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || p == "." {
			return nil
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, p := range paths {
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: path.Clean(p),
			Mode: 0o600,
			Size: int64(len(data)),
		}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Unpack writes the tar produced by Pack into dest, recreating the bundle
// directory. A leading slash is stripped; an entry that escapes dest is
// refused. Only regular files and directories are accepted.
func Unpack(tarball []byte, dest WriteFS) error {
	tr := tar.NewReader(bytes.NewReader(tarball))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		clean, err := safeRel(hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			continue // file paths imply their directories
		case tar.TypeReg:
			data, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			if err := dest.WriteFile(clean, data); err != nil {
				return err
			}
		default:
			return fmt.Errorf("migration bundle: unsupported tar entry %q (type %d)", hdr.Name, hdr.Typeflag)
		}
	}
}

// WriteFS is the sink Unpack writes into. UnpackToDir backs it with the OS
// filesystem; any other implementation keeps Unpack off the disk.
type WriteFS interface {
	// WriteFile writes data at a forward-slashed relative path, creating parent
	// directories as needed.
	WriteFile(relPath string, data []byte) error
}

// safeRel validates a tar entry name: forward-slashed, relative, no traversal.
// A leading slash is stripped; an entry that climbs out of the root is refused.
func safeRel(name string) (string, error) {
	n := strings.TrimPrefix(strings.ReplaceAll(name, "\\", "/"), "/")
	clean := path.Clean(n)
	if clean == "" || clean == "." {
		return "", fmt.Errorf("migration bundle: empty tar entry name")
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("migration bundle: unsafe tar entry path %q", name)
	}
	return clean, nil
}

// UnpackToDir unpacks the tar onto the OS filesystem under destDir, creating
// parent directories as needed. The traversal guards are Unpack's.
func UnpackToDir(tarball []byte, destDir string) error {
	return Unpack(tarball, osWriteFS(destDir))
}

// osWriteFS writes files relative to a root directory on the OS filesystem.
type osWriteFS string

func (root osWriteFS) WriteFile(rel string, data []byte) error {
	dest := filepath.Join(string(root), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0o600)
}
