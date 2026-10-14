package remotecache

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"slices"
	"sync"

	jobmodel "go.putnami.dev/cli/model/jobs"
	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// fakeCacheServer is a shared cache the fake provider sessions talk to. Unlike
// the fixed-blob fake in remote_provider_test.go it actually STORES what one
// machine uploads and serves it to another, moving the bytes through each
// session's own blob exchange directory exactly as a real provider does. That is
// what makes the remote leg of the byte-equivalence matrix a real transfer
// rather than a mock.
type fakeCacheServer struct {
	mu       sync.Mutex
	entries  map[string]fakeCacheEntry
	blobs    map[string][]byte
	prefetch []string
	// dropped digests are accepted on upload but never served back, so a restore
	// succeeds only if the consumer already holds those bytes in its CAS. It is
	// how the mixed leg proves dedup instead of asserting it.
	dropped map[string]bool
	// channel is the provenance stamped on every hit (empty = legacy provider).
	channel cache.Channel
	// resultOnly makes initialize echo cache.CapabilityRestoreResultOnly when
	// core lists it; a result-only restore then places no blob.
	resultOnly bool
	// requests holds every raw request line, in arrival order.
	requests []string
	// placed counts the blobs restores wrote to an exchange directory.
	placed int
	// resultOnlyPrefetch holds every key a prefetch named result-only.
	resultOnlyPrefetch []string
}

type fakeCacheEntry struct {
	result   *cache.ActionResult
	manifest *cache.Manifest
}

func newFakeCacheServer() *fakeCacheServer {
	return &fakeCacheServer{
		entries: map[string]fakeCacheEntry{},
		blobs:   map[string][]byte{},
		dropped: map[string]bool{},
	}
}

// echoResultOnly makes later sessions echo cache.CapabilityRestoreResultOnly.
func (f *fakeCacheServer) echoResultOnly() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resultOnly = true
}

func (f *fakeCacheServer) echoesResultOnly() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resultOnly
}

func (f *fakeCacheServer) recordRequest(line []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, string(line))
}

func (f *fakeCacheServer) upload(key string, result *cache.ActionResult, manifest *cache.Manifest, exchangeDir string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, file := range manifest.Files {
		path, ok := cache.BlobExchangePath(exchangeDir, file.Digest)
		if !ok {
			return false
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return false // the client must stage every blob it names
		}
		f.blobs[file.Digest] = content
	}
	f.entries[key] = fakeCacheEntry{result: result, manifest: manifest}
	return true
}

func (f *fakeCacheServer) restore(key, exchangeDir string) (fakeCacheEntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.entries[key]
	if !ok {
		return fakeCacheEntry{}, false
	}
	for _, file := range entry.manifest.Files {
		if f.dropped[file.Digest] {
			continue
		}
		content, ok := f.blobs[file.Digest]
		if !ok {
			continue
		}
		if path, ok := cache.BlobExchangePath(exchangeDir, file.Digest); ok {
			writeExchangeBlobAtomically(path, content)
			f.placed++
		}
	}
	return entry, true
}

// lookup answers a result-only restore: the entry, and no blob placed.
func (f *fakeCacheServer) lookup(key string) (fakeCacheEntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.entries[key]
	return entry, ok
}

func serveSharedFakeProvider(stdin io.Reader, stdout io.Writer, server *fakeCacheServer) {
	exchangeDir := ""
	in := bufio.NewScanner(stdin)
	in.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	write := func(resp *cache.ProviderResponse) {
		b, _ := json.Marshal(resp)
		_, _ = stdout.Write(append(b, '\n'))
	}
	ok := func(protocolVersion int, id int64, payload any) {
		raw, _ := cache.MarshalPayload(payload)
		write(&cache.ProviderResponse{ProtocolVersion: protocolVersion, ID: id, OK: true, Payload: raw})
	}

	for in.Scan() {
		server.recordRequest(in.Bytes())
		req, diags := cache.ParseAndValidateProviderRequest(in.Bytes())
		if req == nil {
			write(&cache.ProviderResponse{
				ProtocolVersion: cache.ProviderProtocolVersion,
				OK:              false,
				Error:           &cache.ProviderError{Code: "bad-request", Message: diagString(diags)},
			})
			continue
		}
		switch req.Op {
		case cache.OpInitialize:
			p, _ := cache.ParseAndValidateInitializeParams(req.Payload)
			negotiated := req.ProtocolVersion
			var capabilities []string
			if p != nil {
				exchangeDir = p.BlobExchangeDir
				if slices.Contains(p.Capabilities, cache.CapabilityProviderProtocolV2) {
					negotiated = cache.ProviderProtocolVersion
				}
				if server.echoesResultOnly() && slices.Contains(p.Capabilities, cache.CapabilityRestoreResultOnly) {
					capabilities = append(capabilities, cache.CapabilityRestoreResultOnly)
				}
			}
			ok(req.ProtocolVersion, req.ID, &cache.InitializeResult{
				ProtocolVersion: negotiated,
				ProviderName:    fakeProviderExtensionName,
				ProviderVersion: fakeProviderVersion,
				Ready:           true,
				Capabilities:    capabilities,
			})
		case cache.OpPrefetch:
			started := 0
			if p, _ := cache.ParseAndValidatePrefetchParams(req.Payload); p != nil {
				started = len(p.Keys)
				server.mu.Lock()
				server.prefetch = append(server.prefetch, p.Keys...)
				server.resultOnlyPrefetch = append(server.resultOnlyPrefetch, p.ResultOnlyKeys...)
				server.mu.Unlock()
			}
			ok(req.ProtocolVersion, req.ID, &cache.PrefetchResult{Started: started})
		case cache.OpRestore:
			p, _ := cache.ParseAndValidateRestoreParams(req.Payload)
			if p == nil {
				ok(req.ProtocolVersion, req.ID, &cache.RestoreResult{Status: cache.RestoreMiss})
				continue
			}
			var entry fakeCacheEntry
			var hit bool
			if p.ResultOnly {
				entry, hit = server.lookup(p.Key)
			} else {
				entry, hit = server.restore(p.Key, exchangeDir)
			}
			if !hit {
				ok(req.ProtocolVersion, req.ID, &cache.RestoreResult{Status: cache.RestoreMiss})
				continue
			}
			res := &cache.RestoreResult{Status: cache.RestoreHit, Result: entry.result, Manifest: entry.manifest}
			applyFakeRestoreProvenance(res, server.channel, req.ProtocolVersion)
			ok(req.ProtocolVersion, req.ID, res)
		case cache.OpUpload:
			accepted := false
			if p, _ := cache.ParseAndValidateUploadParams(req.Payload); p != nil && p.Manifest != nil {
				accepted = server.upload(p.Key, p.Result, p.Manifest, exchangeDir)
			}
			ok(req.ProtocolVersion, req.ID, &cache.UploadResult{Accepted: accepted})
		case cache.OpSummary:
			ok(req.ProtocolVersion, req.ID, &cache.SummaryResult{})
		case cache.OpShutdown:
			ok(req.ProtocolVersion, req.ID, nil)
			return
		default:
			write(&cache.ProviderResponse{
				ProtocolVersion: req.ProtocolVersion, ID: req.ID, OK: false,
				Error: &cache.ProviderError{Code: "unsupported", Message: string(req.Op)},
			})
		}
	}
}

// matrixJob is the fixture task: one command-output directory, one project
// directory, one workspace file, and one optional output the task legitimately
// never produces. It covers all three declaration roots, both output kinds, and
// the explicit-empty state in a single entry.
func matrixJob() *jobmodel.ScheduledJob {
	return declaredJob("build~emit", "emit", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"lib":      dirDeclaration(extension.OutputRootCommandOutput, "lib", false),
			"gen":      dirDeclaration(extension.OutputRootProject, ".gen", false),
			"lock":     fileDeclaration(extension.OutputRootWorkspace, "bun.lock", false),
			"coverage": fileDeclaration(extension.OutputRootProject, "coverage.json", true),
		},
	})
}
