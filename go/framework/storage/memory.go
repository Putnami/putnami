package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/errors"
)

type memoryObject struct {
	data         []byte
	metadata     *ObjectMetadata
	lastModified time.Time
}

// MemoryBackend is an in-memory storage backend for testing.
type MemoryBackend struct {
	mu      sync.RWMutex
	buckets map[string]map[string]*memoryObject
}

// Ensure MemoryBackend implements Backend and Stater at compile time.
var (
	_ Backend = (*MemoryBackend)(nil)
	_ Stater  = (*MemoryBackend)(nil)
)

// NewMemoryBackend creates a new in-memory storage backend.
func NewMemoryBackend() *MemoryBackend {
	return &MemoryBackend{
		buckets: make(map[string]map[string]*memoryObject),
	}
}

func (b *MemoryBackend) ensureBucket(bucket string) map[string]*memoryObject {
	if b.buckets[bucket] == nil {
		b.buckets[bucket] = make(map[string]*memoryObject)
	}
	return b.buckets[bucket]
}

// Put stores an object in memory.
func (b *MemoryBackend) Put(_ context.Context, bucket, key string, data io.Reader, meta *ObjectMetadata) (*PutResult, error) {
	buf, err := io.ReadAll(data)
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageRead, errors.String("backend", "memory"))
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	objects := b.ensureBucket(bucket)
	objects[key] = &memoryObject{
		data:         buf,
		metadata:     meta,
		lastModified: time.Now(),
	}

	return &PutResult{
		Key:  key,
		Size: int64(len(buf)),
		ETag: fmt.Sprintf("%x", len(buf)),
	}, nil
}

// Get retrieves an object from memory.
func (b *MemoryBackend) Get(_ context.Context, bucket, key string) (*GetResult, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	objects := b.buckets[bucket]
	if objects == nil {
		return nil, nil
	}
	obj, ok := objects[key]
	if !ok {
		return nil, nil
	}

	result := &GetResult{
		Key:          key,
		Body:         io.NopCloser(bytes.NewReader(obj.data)),
		Size:         int64(len(obj.data)),
		LastModified: obj.lastModified,
		ETag:         fmt.Sprintf("%x", len(obj.data)),
	}
	if obj.metadata != nil {
		result.ContentType = obj.metadata.ContentType
		result.Metadata = obj.metadata.Custom
	}
	return result, nil
}

// Delete removes an object from memory.
func (b *MemoryBackend) Delete(_ context.Context, bucket, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if objects := b.buckets[bucket]; objects != nil {
		delete(objects, key)
	}
	return nil
}

// List returns objects in a bucket matching the given options.
func (b *MemoryBackend) List(_ context.Context, bucket string, opts *ListOptions) (*ListResult, error) {
	if opts == nil {
		opts = &ListOptions{}
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	objects := b.buckets[bucket]
	if objects == nil {
		return &ListResult{}, nil
	}

	result := &ListResult{}
	prefixSet := make(map[string]struct{})
	infos := make([]ObjectInfo, 0, len(objects))

	for key, obj := range objects {
		if opts.Prefix != "" && !strings.HasPrefix(key, opts.Prefix) {
			continue
		}

		if opts.Delimiter != "" {
			remaining := key
			if opts.Prefix != "" {
				remaining = strings.TrimPrefix(key, opts.Prefix)
			}
			if idx := strings.Index(remaining, opts.Delimiter); idx >= 0 {
				prefix := key[:len(opts.Prefix)+idx+len(opts.Delimiter)]
				prefixSet[prefix] = struct{}{}
				continue
			}
		}

		info := ObjectInfo{
			Key:          key,
			Size:         int64(len(obj.data)),
			ETag:         fmt.Sprintf("%x", len(obj.data)),
			LastModified: obj.lastModified,
		}
		if obj.metadata != nil {
			info.ContentType = obj.metadata.ContentType
		}
		infos = append(infos, info)
	}

	sort.Slice(infos, func(i, j int) bool {
		return infos[i].Key < infos[j].Key
	})

	for p := range prefixSet {
		result.Prefixes = append(result.Prefixes, p)
	}
	sort.Strings(result.Prefixes)

	// Resume from a previous page: the continuation token is the first key not
	// yet returned, so drop everything strictly before it.
	if opts.ContinuationToken != "" {
		start := sort.Search(len(infos), func(i int) bool {
			return infos[i].Key >= opts.ContinuationToken
		})
		infos = infos[start:]
	}

	if opts.MaxKeys > 0 && len(infos) > opts.MaxKeys {
		result.Objects = infos[:opts.MaxKeys]
		result.IsTruncated = true
		result.ContinuationToken = infos[opts.MaxKeys].Key
	} else {
		result.Objects = infos
	}

	return result, nil
}

// Exists checks whether an object exists in memory.
func (b *MemoryBackend) Exists(_ context.Context, bucket, key string) (bool, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if objects := b.buckets[bucket]; objects != nil {
		_, ok := objects[key]
		return ok, nil
	}
	return false, nil
}

// Stat returns the metadata of an object in memory.
func (b *MemoryBackend) Stat(_ context.Context, bucket, key string) (*ObjectInfo, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	obj, ok := b.buckets[bucket][key]
	if !ok {
		return nil, statNotFound("memory", bucket, key)
	}
	info := &ObjectInfo{
		Key:          key,
		Size:         int64(len(obj.data)),
		ETag:         fmt.Sprintf("%x", len(obj.data)),
		LastModified: obj.lastModified,
	}
	if obj.metadata != nil {
		info.ContentType = obj.metadata.ContentType
	}
	return info, nil
}

// Copy duplicates an object within the same bucket in memory.
func (b *MemoryBackend) Copy(_ context.Context, bucket, source, destination string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	objects := b.buckets[bucket]
	if objects == nil {
		return errors.New(CodeStorageNotFound, "bucket not found", errors.String("backend", "memory"), errors.String("bucket", bucket))
	}
	obj, ok := objects[source]
	if !ok {
		return errors.New(CodeStorageNotFound, "source not found", errors.String("backend", "memory"), errors.String("bucket", bucket), errors.String("key", source))
	}

	data := make([]byte, len(obj.data))
	copy(data, obj.data)

	objects[destination] = &memoryObject{
		data:         data,
		metadata:     obj.metadata,
		lastModified: time.Now(),
	}
	return nil
}

// Close is a no-op for the memory backend.
func (b *MemoryBackend) Close() error {
	return nil
}
