package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"go.putnami.dev/errors"
)

// FileBackend stores objects on the local filesystem.
type FileBackend struct {
	dataDir string
}

// Ensure FileBackend implements Backend and Stater at compile time.
var (
	_ Backend = (*FileBackend)(nil)
	_ Stater  = (*FileBackend)(nil)
)

// NewFileBackend creates a filesystem-based storage backend.
func NewFileBackend(dataDir string) *FileBackend {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		abs = dataDir
	}
	return &FileBackend{dataDir: abs}
}

// safePath resolves a path under dataDir and verifies it does not escape.
func (b *FileBackend) safePath(parts ...string) (string, error) {
	for _, p := range parts {
		if strings.Contains(p, "\x00") {
			return "", errors.New(CodeStorageRead, "invalid path: null byte", errors.String("backend", "file"))
		}
	}
	elems := append([]string{b.dataDir}, parts...)
	resolved := filepath.Clean(filepath.Join(elems...))
	if !strings.HasPrefix(resolved, b.dataDir+string(filepath.Separator)) && resolved != b.dataDir {
		return "", errors.New(CodeStorageRead, "invalid path: traversal detected", errors.String("backend", "file"))
	}
	return resolved, nil
}

func (b *FileBackend) bucketPath(bucket string) (string, error) {
	return b.safePath(bucket)
}

func (b *FileBackend) objectPath(bucket, key string) (string, error) {
	return b.safePath(bucket, key)
}

func (b *FileBackend) metaPath(bucket, key string) (string, error) {
	return b.safePath(bucket, key+fileMetaSuffix)
}

// fileMetaSuffix ends the name of an object's metadata file.
const fileMetaSuffix = ".meta.json"

// fileTempSuffix ends the name of every temporary file Put writes next to an
// object. List skips names with this suffix, so an in-flight or abandoned
// temporary file never appears as an object.
const fileTempSuffix = ".putnami-tmp"

// reservedSuffix returns the reserved suffix that ends a path segment of key,
// compared without regard to case, or "" when no segment ends with one.
func reservedSuffix(key string) string {
	for _, segment := range strings.Split(strings.ToLower(key), "/") {
		for _, suffix := range []string{fileMetaSuffix, fileTempSuffix} {
			if strings.HasSuffix(segment, suffix) {
				return suffix
			}
		}
	}
	return ""
}

// Put stores an object on the filesystem. It streams data into a temporary
// file in the object's directory and renames it over the object path only
// after the whole body is written, so a failing reader leaves any existing
// object and its metadata intact. Metadata is then replaced the same way; a
// Put without metadata removes the replaced object's metadata file.
//
// Put rejects a key with a path segment ending in fileMetaSuffix or
// fileTempSuffix, compared without regard to case: the backend reserves those
// names for metadata and temporary files, and a directory with such a name
// would block the metadata or temporary file of another key.
func (b *FileBackend) Put(_ context.Context, bucket, key string, data io.Reader, meta *ObjectMetadata) (*PutResult, error) {
	if suffix := reservedSuffix(key); suffix != "" {
		return nil, errors.New(CodeStorageWrite, "key has a path segment with a suffix the file backend reserves",
			errors.String("backend", "file"), errors.String("bucket", bucket),
			errors.String("key", key), errors.String("suffix", suffix))
	}
	objPath, err := b.objectPath(bucket, key)
	if err != nil {
		return nil, errors.Wrapf(err, CodeStorageWrite, "resolve object path", errors.String("op", "put"))
	}
	mp, err := b.metaPath(bucket, key)
	if err != nil {
		return nil, errors.Wrapf(err, CodeStorageWrite, "resolve metadata path", errors.String("op", "put"))
	}
	if err := os.MkdirAll(filepath.Dir(objPath), 0o750); err != nil {
		return nil, errors.Wrap(err, CodeStorageWrite, errors.String("backend", "file"), errors.String("op", "mkdir"))
	}

	size, err := replaceFile(objPath, data)
	if err != nil {
		return nil, err
	}

	if meta != nil {
		metaBytes, err := json.Marshal(meta)
		if err == nil {
			if _, werr := replaceFile(mp, bytes.NewReader(metaBytes)); werr != nil {
				return nil, errors.Wrapf(werr, CodeStorageWrite, "write metadata", errors.String("backend", "file"), errors.String("op", "write_metadata"))
			}
		}
	} else if rerr := os.Remove(mp); rerr != nil && !os.IsNotExist(rerr) {
		return nil, errors.Wrap(rerr, CodeStorageWrite, errors.String("backend", "file"), errors.String("op", "remove_metadata"))
	}

	return &PutResult{
		Key:  key,
		Size: size,
		ETag: fmt.Sprintf("%x", size),
	}, nil
}

// replaceFile streams data into a new temporary file in path's directory and
// renames it over path once the body is written and the file closed. On any
// failure it removes the temporary file and leaves an existing file at path
// untouched. The temporary name carries 128 random bits and is created
// exclusively, so concurrent Puts never share a temporary file. The file gets
// the mode os.Create gives: 0666 narrowed by the process umask.
func replaceFile(path string, data io.Reader) (int64, error) {
	tmpPath := filepath.Clean(filepath.Join(filepath.Dir(path), ".put-"+rand.Text()+fileTempSuffix))
	tmp, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666) //nolint:gosec // G302: a stored file takes os.Create's mode, 0666 narrowed by the umask
	if err != nil {
		return 0, errors.Wrap(err, CodeStorageWrite, errors.String("backend", "file"), errors.String("op", "create"))
	}
	size, op, err := fillTemp(tmp, data)
	if err == nil {
		op = "rename"
		err = os.Rename(tmpPath, path)
	}
	if err != nil {
		werr := errors.Wrap(err, CodeStorageWrite, errors.String("backend", "file"), errors.String("op", op))
		if rerr := os.Remove(tmpPath); rerr != nil && !os.IsNotExist(rerr) {
			werr = werr.WithAttr(errors.String("cleanupError", rerr.Error()))
		}
		return 0, werr
	}
	return size, nil
}

// fillTemp copies data into tmp and closes it. It closes tmp on every path and
// names the step that failed.
func fillTemp(tmp *os.File, data io.Reader) (int64, string, error) {
	op := "write"
	size, err := io.Copy(tmp, data)
	if cerr := tmp.Close(); cerr != nil && err == nil {
		op, err = "close", cerr
	}
	return size, op, err
}

// Get retrieves an object from the filesystem.
func (b *FileBackend) Get(_ context.Context, bucket, key string) (*GetResult, error) {
	objPath, err := b.objectPath(bucket, key)
	if err != nil {
		return nil, errors.Wrapf(err, CodeStorageRead, "resolve object path", errors.String("op", "get"))
	}
	info, err := os.Stat(objPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, errors.Wrap(err, CodeStorageRead, errors.String("backend", "file"), errors.String("op", "stat"))
	}

	f, err := os.Open(objPath) //nolint:gosec // path validated by safePath
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageRead, errors.String("backend", "file"), errors.String("op", "open"))
	}

	result := &GetResult{
		Key:          key,
		Body:         f,
		Size:         info.Size(),
		LastModified: info.ModTime(),
		ETag:         fmt.Sprintf("%x", info.Size()),
	}

	if meta := b.loadMetadata(bucket, key); meta != nil {
		result.ContentType = meta.ContentType
		result.Metadata = meta.Custom
	}

	return result, nil
}

// loadMetadata reads an object's metadata file. It returns nil when the object
// has none or the file cannot be read.
func (b *FileBackend) loadMetadata(bucket, key string) *ObjectMetadata {
	mp, err := b.metaPath(bucket, key)
	if err != nil {
		return nil
	}
	metaBytes, err := os.ReadFile(mp) //nolint:gosec // path validated by safePath
	if err != nil {
		return nil
	}
	var meta ObjectMetadata
	if json.Unmarshal(metaBytes, &meta) != nil {
		return nil
	}
	return &meta
}

// Stat returns the metadata of an object on the filesystem. A directory, a
// path below an object, and a key Put would reject for a reserved suffix hold
// no object.
func (b *FileBackend) Stat(_ context.Context, bucket, key string) (*ObjectInfo, error) {
	if reservedSuffix(key) != "" {
		return nil, statNotFound("file", bucket, key)
	}
	objPath, err := b.objectPath(bucket, key)
	if err != nil {
		return nil, errors.Wrapf(err, CodeStorageRead, "resolve object path", errors.String("op", "stat"))
	}
	fi, err := os.Stat(objPath)
	if err != nil {
		// ENOTDIR: a parent segment of key is an object, so key holds none.
		if os.IsNotExist(err) || stderrors.Is(err, syscall.ENOTDIR) {
			return nil, statNotFound("file", bucket, key)
		}
		return nil, errors.Wrap(err, CodeStorageRead, errors.String("backend", "file"), errors.String("op", "stat"))
	}
	if fi.IsDir() {
		return nil, statNotFound("file", bucket, key)
	}

	info := &ObjectInfo{
		Key:          key,
		Size:         fi.Size(),
		ETag:         fmt.Sprintf("%x", fi.Size()),
		LastModified: fi.ModTime(),
	}
	if meta := b.loadMetadata(bucket, key); meta != nil {
		info.ContentType = meta.ContentType
	}
	return info, nil
}

// Delete removes an object from the filesystem.
func (b *FileBackend) Delete(_ context.Context, bucket, key string) error {
	mp, err := b.metaPath(bucket, key)
	if err != nil {
		return errors.Wrapf(err, CodeStorageDelete, "resolve metadata path", errors.String("op", "delete"))
	}
	if err := os.Remove(mp); err != nil && !os.IsNotExist(err) {
		return errors.Wrap(err, CodeStorageDelete, errors.String("backend", "file"), errors.String("op", "delete_metadata"))
	}
	op, err := b.objectPath(bucket, key)
	if err != nil {
		return errors.Wrapf(err, CodeStorageDelete, "resolve object path", errors.String("op", "delete"))
	}
	if err := os.Remove(op); err != nil && !os.IsNotExist(err) {
		return errors.Wrap(err, CodeStorageDelete, errors.String("backend", "file"))
	}
	return nil
}

// List returns objects in a bucket matching the given options.
func (b *FileBackend) List(_ context.Context, bucket string, opts *ListOptions) (*ListResult, error) {
	if opts == nil {
		opts = &ListOptions{}
	}

	bucketDir, err := b.bucketPath(bucket)
	if err != nil {
		return nil, errors.Wrapf(err, CodeStorageList, "resolve bucket path", errors.String("op", "list"))
	}
	result := &ListResult{}

	prefixSet := make(map[string]struct{})
	var objects []ObjectInfo

	walkErr := filepath.Walk(bucketDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip inaccessible entries, continue walking
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, fileMetaSuffix) || strings.HasSuffix(path, fileTempSuffix) {
			return nil
		}

		rel, err := filepath.Rel(bucketDir, path)
		if err != nil {
			return nil //nolint:nilerr // skip entries with unresolvable paths
		}
		key := filepath.ToSlash(rel)

		if opts.Prefix != "" && !strings.HasPrefix(key, opts.Prefix) {
			return nil
		}

		if opts.Delimiter != "" {
			remaining := key
			if opts.Prefix != "" {
				remaining = strings.TrimPrefix(key, opts.Prefix)
			}
			if idx := strings.Index(remaining, opts.Delimiter); idx >= 0 {
				prefix := key[:len(opts.Prefix)+idx+len(opts.Delimiter)]
				prefixSet[prefix] = struct{}{}
				return nil
			}
		}

		objects = append(objects, ObjectInfo{
			Key:          key,
			Size:         info.Size(),
			ETag:         fmt.Sprintf("%x", info.Size()),
			LastModified: info.ModTime(),
		})
		return nil
	})
	if walkErr != nil && !os.IsNotExist(walkErr) {
		return nil, errors.Wrap(walkErr, CodeStorageList, errors.String("backend", "file"), errors.String("bucket", bucket))
	}

	sort.Slice(objects, func(i, j int) bool {
		return objects[i].Key < objects[j].Key
	})

	for p := range prefixSet {
		result.Prefixes = append(result.Prefixes, p)
	}
	sort.Strings(result.Prefixes)

	// Resume from a previous page: the continuation token is the first key not
	// yet returned, so drop everything strictly before it.
	if opts.ContinuationToken != "" {
		start := sort.Search(len(objects), func(i int) bool {
			return objects[i].Key >= opts.ContinuationToken
		})
		objects = objects[start:]
	}

	if opts.MaxKeys > 0 && len(objects) > opts.MaxKeys {
		result.Objects = objects[:opts.MaxKeys]
		result.IsTruncated = true
		result.ContinuationToken = objects[opts.MaxKeys].Key
	} else {
		result.Objects = objects
	}

	return result, nil
}

// Exists checks whether an object exists on the filesystem.
func (b *FileBackend) Exists(_ context.Context, bucket, key string) (bool, error) {
	op, perr := b.objectPath(bucket, key)
	if perr != nil {
		return false, errors.Wrapf(perr, CodeStorageRead, "resolve object path", errors.String("op", "exists"))
	}
	_, err := os.Stat(op)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, errors.Wrap(err, CodeStorageRead, errors.String("backend", "file"), errors.String("op", "exists"))
	}
	return true, nil
}

// Copy duplicates an object within the same bucket.
func (b *FileBackend) Copy(ctx context.Context, bucket, source, destination string) (retErr error) {
	result, err := b.Get(ctx, bucket, source)
	if err != nil {
		return errors.Wrapf(err, CodeStorageCopy, "get source", errors.String("op", "copy"))
	}
	if result == nil {
		return errors.New(CodeStorageNotFound, "source not found", errors.String("backend", "file"), errors.String("bucket", bucket), errors.String("key", source))
	}
	defer func() {
		if cerr := result.Body.Close(); cerr != nil && retErr == nil {
			retErr = errors.Wrap(cerr, CodeStorageCopy, errors.String("backend", "file"), errors.String("op", "close_source"))
		}
	}()

	_, err = b.Put(ctx, bucket, destination, result.Body, &ObjectMetadata{
		ContentType: result.ContentType,
		Custom:      result.Metadata,
	})
	if err != nil {
		return errors.Wrapf(err, CodeStorageCopy, "put destination", errors.String("op", "copy"))
	}
	return nil
}

// Close is a no-op for the file backend.
func (b *FileBackend) Close() error {
	return nil
}
