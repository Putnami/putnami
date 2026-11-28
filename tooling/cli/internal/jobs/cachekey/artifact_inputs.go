package cachekey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

// An input port that declares `fold: "artifact"` keys its consumer on the
// digest of the producer output its step binds instead of the producer's key
// (CLI ADR 0043). When the producer's result is published, the scheduler
// publishes that digest in the run's hash map at ArtifactDigestKey, beside the
// producer's key. A consumer's key folds, for each such port:
//
//   - with no producer in the plan, a fixed absent value;
//   - for a binding of a step of its own project, the producer's artifact
//     digest;
//   - for a `^step` binding, the sorted list of each producer's project and
//     artifact digest;
//
// and it does not fold those producers' keys. A producer whose digest is not
// published, before it ran (PrecomputeKeys, a dry run, an impact plan, a
// selection fingerprint) or when its digest could not be read, contributes its
// published key instead: a key never claims bytes it has not read. One that
// published neither contributes a value no other computation folds, so its
// consumer's key matches no stored entry.
// PrecomputeKeys publishes the digest a producer's local entry recorded
// (publishRecordedArtifacts), at the store key the producer's lookup would
// read, so a consumer's precomputed key is the one it computes when it starts
// whenever its producers hit locally. A producer may still execute, so
// PlanKeyMovement reports every consumer moving.
//
// A digest leaves out the subpaths the producer's output port excludes
// (extension.ArtifactExcludes), on an execution and on a hit alike
// (PortArtifactDigest).
//
// A keyless consumer folds its artifact inputs the same way into its identity
// and into the action an invocation consumer folds, and relays their digests
// instead of the producer's key to a dependent of its own project (keyless.go).

// ArtifactDigestKey is the entry of a run's hash map that holds the digest of
// the output port of the producer job at producerKey. No job key holds a NUL.
func ArtifactDigestKey(producerKey, port string) string {
	return producerKey + "\x00artifact\x00" + port
}

// ArtifactEmptyDigest is the digest of an artifact that holds nothing: a
// missing file or directory, or a directory without a file.
const ArtifactEmptyDigest = "artifact-v1:empty"

// artifactAbsent is what an artifact input with no producer in the plan folds.
const artifactAbsent = "artifact-v1:absent"

// ArtifactFile is one file of an artifact: its slash path relative to the
// output (empty for a file output), its permission bits and its content
// digest ("sha256:<hex>", the CAS digest).
type ArtifactFile struct {
	Path   string
	Mode   uint32
	Digest string
}

// ArtifactDigest is the digest of an artifact of kind (extension.OutputKindFile
// or extension.OutputKindDirectory) made of files: ArtifactEmptyDigest without
// a file, a file's content digest, or the digest of a directory's sorted
// relative paths, executable bits and content digests. A directory folds no
// other permission bit: the umask of the machine that wrote a file decides
// them, as a file output folds no mode at all.
func ArtifactDigest(kind string, files []ArtifactFile) string {
	if len(files) == 0 {
		return ArtifactEmptyDigest
	}
	if kind == extension.OutputKindFile {
		return "artifact-v1:file:" + files[0].Digest
	}
	sorted := append([]ArtifactFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	h.Write([]byte("artifact-v1\ndirectory\n"))
	for _, file := range sorted {
		executable := "-"
		if file.Mode&0o111 != 0 {
			executable = "x"
		}
		fmt.Fprintf(h, "%s\x00%s\x00%s\n", file.Path, executable, file.Digest)
	}
	return "artifact-v1:directory:" + hex.EncodeToString(h.Sum(nil))
}

// PortArtifactDigest is the digest of the artifact the task's output port
// names, made of files of kind: ArtifactDigest of the files outside the
// subpaths the port excludes (extension.ArtifactExcludes). An execution and a
// hit both digest through it, so one set of bytes has one digest.
func PortArtifactDigest(task extension.TaskDefinition, port, kind string, files []ArtifactFile) string {
	excludes := extension.ArtifactExcludes(task, port)
	if kind != extension.OutputKindDirectory || len(excludes) == 0 {
		return ArtifactDigest(kind, files)
	}
	kept := make([]ArtifactFile, 0, len(files))
	for _, file := range files {
		if !artifactExcluded(file.Path, excludes) {
			kept = append(kept, file)
		}
	}
	return ArtifactDigest(kind, kept)
}

// artifactExcluded reports whether the output-relative slash path rel is one of
// excludes or lies under one.
func artifactExcluded(rel string, excludes []string) bool {
	for _, exclude := range excludes {
		if rel == exclude || strings.HasPrefix(rel, exclude+"/") {
			return true
		}
	}
	return false
}

// VersionStampRelPath is the project-relative slash path of the build stamp
// the scheduler writes before a project's first task and rewrites at the
// current identity after every restore that replaced it.
const VersionStampRelPath = ".gen/version.json"

// VersionStampWithin returns the path of the build stamp relative to a
// directory output at rel under root, and whether that output holds it. The
// stamp lives under the project root only. The containment folds case, as
// every ownership comparison does.
func VersionStampWithin(root, rel string) (string, bool) {
	if root != extension.OutputRootProject {
		return "", false
	}
	if len(VersionStampRelPath) <= len(rel)+1 || VersionStampRelPath[len(rel)] != '/' ||
		!strings.EqualFold(VersionStampRelPath[:len(rel)], rel) {
		return "", false
	}
	return VersionStampRelPath[len(rel)+1:], true
}

// IsArtifactVersionStamp reports whether the file at rel of an output of kind
// at path under root is the build stamp, which is part of no artifact digest
// (rel is empty for a file output). The scheduler rewrites the stamp at the
// current identity after every restore, so a stored verdict already replays
// against another stamp, and no cache key folds it: a task whose output
// embeds the version folds the version itself (TaskIsVersionAware). Folding
// it would move every consumer's key on every run.
func IsArtifactVersionStamp(root, kind, path, rel string) bool {
	if kind == extension.OutputKindFile {
		return root == extension.OutputRootProject && strings.EqualFold(path, VersionStampRelPath)
	}
	stamp, held := VersionStampWithin(root, path)
	return held && strings.EqualFold(rel, stamp)
}

// ArtifactFilesFromEntry returns the files the entry records for the declared
// output id, from its manifest's paths, modes and content digests, without
// reading a file, the build stamp left out
// (IsArtifactVersionStamp).
func ArtifactFilesFromEntry(entry *store.TaskEntry, id string) ([]ArtifactFile, bool) {
	recorded, ok := entry.Output(id)
	if !ok {
		return nil, false
	}
	if !recorded.Present() {
		return nil, true
	}
	if entry.Manifest == nil {
		return nil, false
	}
	var files []ArtifactFile
	listed := 0
	for _, file := range entry.Manifest.Files {
		var artifact ArtifactFile
		switch {
		case recorded.Kind == extension.OutputKindFile && file.Path == id:
			artifact = ArtifactFile{Mode: file.Mode, Digest: file.Digest}
		case recorded.Kind != extension.OutputKindFile && strings.HasPrefix(file.Path, id+"/"):
			artifact = ArtifactFile{Path: strings.TrimPrefix(file.Path, id+"/"), Mode: file.Mode, Digest: file.Digest}
		default:
			continue
		}
		listed++
		if !IsArtifactVersionStamp(recorded.Root, recorded.Kind, recorded.Path, artifact.Path) {
			files = append(files, artifact)
		}
	}
	if listed != recorded.Files {
		return nil, false
	}
	return files, true
}

// publishRecordedArtifacts puts into published, for each output port of job a
// planned consumer binds (ScheduledJob.ArtifactOutputs), the digest the job's
// local entry at key recorded, the one a hit at that key publishes
// (restoreDeclaredCacheHit), so a key computed before the job runs folds it
// when the entry is local. A source-bound job's entry is read where its
// lookup reads it, at the store key entryKey resolves for its project's
// current source binding (SourceBoundEntryKey). It reads the local store only,
// and publishes nothing for a job whose entry is absent, cannot be restored,
// or does not record the output, nor for a source-bound job entryKey resolves
// no store key for: its consumers fold its key.
func publishRecordedArtifacts(job *jobmodel.ScheduledJob, key string, cache *store.CacheManager, entryKey EntryKeyResolver, published map[string]string) {
	if job == nil || len(job.ArtifactOutputs) == 0 || cache == nil || key == "" || job.Step == nil || job.Extension == nil ||
		!jobmodel.UsesDeclaredCapture(job) {
		return
	}
	address := key
	if TaskIsSourceBound(job) {
		if entryKey == nil {
			return
		}
		bound, ok := entryKey(job, key)
		if !ok || bound == "" {
			return
		}
		address = bound
	}
	task, ok := job.Extension.Tasks[job.Step.Task]
	if !ok {
		return
	}
	entry, err := cache.LookupTaskEntry(address)
	if err != nil || entry == nil || entry.Result == nil {
		return
	}
	if DeclaredSourceRewriter(job) && JobResultFromEntryResult(entry.Result).SourceMutated {
		return
	}
	for _, port := range job.ArtifactOutputs {
		id, output, err := extension.ArtifactOutputFor(task, port)
		if err != nil {
			continue
		}
		var digest string
		files, ok := ArtifactFilesFromEntry(entry, id)
		if ok {
			recorded, _ := entry.Output(id)
			digest = PortArtifactDigest(task, port, recorded.Kind, files)
		}
		if _, recorded := entry.Output(id); !recorded && output.PathFrom != "" {
			// A pathFrom output whose port reported no path holds nothing.
			digest, ok = ArtifactEmptyDigest, true
		}
		if ok {
			published[ArtifactDigestKey(job.Key(), port)] = digest
		}
	}
}

// ArtifactDigestKeys returns the hash-map entries a job's key reads for its
// artifact inputs and for those of each producer of an invocation-scoped
// resource it consumes, whose action its key folds (invocationProducerDigest),
// once each.
func ArtifactDigestKeys(job *jobmodel.ScheduledJob) []string {
	var keys []string
	seen := make(map[string]bool)
	for owner := job; owner != nil; owner = owner.InvocationProducer {
		for _, input := range owner.ArtifactInputs {
			for _, producer := range input.Producers {
				key := ArtifactDigestKey(producer.Key, input.Output)
				if !seen[key] {
					seen[key] = true
					keys = append(keys, key)
				}
			}
		}
	}
	return keys
}

// artifactInputFolds returns what each artifact input of job contributes to
// its key, read from hashes.
func artifactInputFolds(job *jobmodel.ScheduledJob, hashes map[string]string) []string {
	if job == nil || len(job.ArtifactInputs) == 0 {
		return nil
	}
	folds := make([]string, 0, len(job.ArtifactInputs))
	for _, input := range job.ArtifactInputs {
		var parts []string
		for _, producer := range input.Producers {
			// A producer that published neither a digest nor a key folds a
			// value no other computation shares: the bytes the consumer reads
			// are unknown, so its key must match no stored entry.
			value := "unpublished\x00" + unpublishedNonce()
			if digest, ok := hashes[ArtifactDigestKey(producer.Key, input.Output)]; ok {
				value = "digest\x00" + digest
			} else if key, ok := hashes[producer.Key]; ok {
				value = "key\x00" + key
			}
			if input.Upstream {
				value = producer.Project + "\x00" + value
			}
			parts = append(parts, value)
		}
		sort.Strings(parts)
		body := artifactAbsent
		if len(parts) > 0 {
			body = strings.Join(parts, "\x01")
		}
		shape := "step"
		if input.Upstream {
			shape = "upstream"
		}
		sum := sha256.Sum256([]byte("artifact-input-v1\x00" + input.Port + "\x00" + input.Output + "\x00" + shape + "\x00" + body))
		folds = append(folds, "artifactInput:"+hex.EncodeToString(sum[:]))
	}
	return folds
}

// unpublishedNonce returns a value no other key computation folds.
func unpublishedNonce() string {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(nonce[:])
}
