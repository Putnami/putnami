package pinnedarchive

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Extract writes the regular files and directories of the archive at
// archivePath into dir, creating dir when missing. It refuses the whole
// archive when one entry is unsafe (see the package documentation) or when
// the archive exceeds limits; a zero Limits selects DefaultLimits. Extract
// verifies no digest: Install and Download do that before calling it.
//
// Extracted files are written with mode 0755 when the archive marks any
// execute bit and 0644 otherwise, and directories with 0755.
func Extract(ctx context.Context, archivePath string, format Format, dir string, limits Limits) error {
	limits = limitsOrDefault(limits)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	x := &extractor{ctx: ctx, root: root, limits: limits}
	switch format {
	case TarGz:
		return x.tarGz(archivePath)
	case Zip:
		return x.zip(archivePath)
	default:
		return fmt.Errorf("unsupported archive format %q", format)
	}
}

type extractor struct {
	ctx     context.Context
	root    *os.Root
	limits  Limits
	entries int
	written int64
}

func (x *extractor) tarGz(archivePath string) (err error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("read %s: %w", archivePath, err)
	}
	defer func() {
		if closeErr := gz.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("read %s: %w", archivePath, closeErr)
		}
	}()

	tr := tar.NewReader(gz)
	for {
		if err := x.ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if errors.Is(err, tar.ErrInsecurePath) && hdr != nil {
			return fmt.Errorf("%w: %q", ErrUnsafeEntry, hdr.Name)
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", archivePath, err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			// A pax global header carries metadata for the entries after it
			// and names no file.
			continue
		}
		if err := x.count(); err != nil {
			return err
		}
		name, skip, err := entryName(hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if skip {
				continue
			}
			if err := x.root.MkdirAll(name, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", name, err)
			}
		case tar.TypeReg:
			if skip {
				return fmt.Errorf("%w: a file entry named %q", ErrUnsafeEntry, hdr.Name)
			}
			if err := x.file(name, hdr.Size, hdr.Mode&0o111 != 0, tr); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: %q is a %s", ErrUnsafeEntry, hdr.Name, tarTypeName(hdr.Typeflag))
		}
	}
}

func (x *extractor) zip(archivePath string) error {
	zr, err := zip.OpenReader(archivePath)
	if errors.Is(err, zip.ErrInsecurePath) {
		if zr != nil {
			_ = zr.Close()
		}
		return fmt.Errorf("%w: %s names a path outside the archive", ErrUnsafeEntry, archivePath)
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", archivePath, err)
	}
	defer func() { _ = zr.Close() }()

	for _, entry := range zr.File {
		if err := x.ctx.Err(); err != nil {
			return err
		}
		if err := x.count(); err != nil {
			return err
		}
		name, skip, err := entryName(entry.Name)
		if err != nil {
			return err
		}
		mode := entry.Mode()
		switch {
		case mode.IsDir():
			if skip {
				continue
			}
			if err := x.root.MkdirAll(name, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", name, err)
			}
		case mode.IsRegular():
			if skip {
				return fmt.Errorf("%w: a file entry named %q", ErrUnsafeEntry, entry.Name)
			}
			// The declared size as an int64: a size past math.MaxInt64 comes
			// back negative, and is as much too large as any other.
			size := entry.FileInfo().Size()
			if size < 0 || size > x.limits.ExtractedBytes {
				return fmt.Errorf("%w: %s is larger than %d bytes", ErrTooLarge, entry.Name, x.limits.ExtractedBytes)
			}
			if err := x.zipFile(name, entry, size, mode&0o111 != 0); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: %q is a %s", ErrUnsafeEntry, entry.Name, mode.Type())
		}
	}
	return nil
}

func (x *extractor) zipFile(name string, entry *zip.File, size int64, executable bool) error {
	rc, err := entry.Open()
	if err != nil {
		return fmt.Errorf("read %s: %w", entry.Name, err)
	}
	defer func() { _ = rc.Close() }()
	return x.file(name, size, executable, rc)
}

// count enforces the entry limit.
func (x *extractor) count() error {
	x.entries++
	if x.entries > x.limits.Entries {
		return fmt.Errorf("%w: more than %d entries", ErrTooLarge, x.limits.Entries)
	}
	return nil
}

// file writes one regular file of the declared size. The content is read
// through a limit one byte past the declared size, so an entry whose header
// lies about its size cannot write past the extraction budget.
func (x *extractor) file(name string, size int64, executable bool, content io.Reader) error {
	if size < 0 {
		return fmt.Errorf("%w: %s declares a negative size", ErrUnsafeEntry, name)
	}
	if x.written+size > x.limits.ExtractedBytes {
		return fmt.Errorf("%w: more than %d extracted bytes", ErrTooLarge, x.limits.ExtractedBytes)
	}
	if dir := filepath.Dir(name); dir != "." {
		if err := x.root.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	perm := fs.FileMode(0o644)
	if executable {
		perm = 0o755
	}
	out, err := x.root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	n, copyErr := io.Copy(out, io.LimitReader(&contextReader{ctx: x.ctx, r: content}, size+1))
	closeErr := out.Close()
	if copyErr != nil {
		return fmt.Errorf("write %s: %w", name, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("write %s: %w", name, closeErr)
	}
	if n != size {
		return fmt.Errorf("%w: %s holds %d bytes, its header declares %d", ErrUnsafeEntry, name, n, size)
	}
	x.written += n
	return nil
}

// entryName turns an archive entry name into a path local to the extraction
// root. skip reports an entry that names the root itself ("./"), which a
// directory entry may do and a file entry may not.
//
// The checks are the same on every platform, so one archive is accepted or
// refused identically everywhere: a backslash or a colon is refused even where
// it is an ordinary file name character, because on Windows it is a
// separator or a drive and stream marker. filepath.IsLocal adds the checks of
// the running platform, such as Windows reserved device names.
func entryName(raw string) (name string, skip bool, err error) {
	unsafe := func() (string, bool, error) {
		return "", false, fmt.Errorf("%w: %q", ErrUnsafeEntry, raw)
	}
	if raw == "" || strings.ContainsAny(raw, "\\:\x00") || path.IsAbs(raw) {
		return unsafe()
	}
	clean := path.Clean(raw)
	if clean == "." {
		return "", true, nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return unsafe()
	}
	local := filepath.FromSlash(clean)
	if !filepath.IsLocal(local) {
		return unsafe()
	}
	return local, false, nil
}

func tarTypeName(flag byte) string {
	switch flag {
	case tar.TypeSymlink:
		return "symbolic link"
	case tar.TypeLink:
		return "hard link"
	case tar.TypeChar:
		return "character device"
	case tar.TypeBlock:
		return "block device"
	case tar.TypeFifo:
		return "FIFO"
	default:
		return fmt.Sprintf("tar entry of type %q", flag)
	}
}

// contextReader stops a copy when its context ends.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
