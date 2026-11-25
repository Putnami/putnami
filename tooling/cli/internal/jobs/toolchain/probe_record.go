package toolchain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/procdeadline"
	"go.putnami.dev/tooling/cli/internal/store"
)

// A runtime toolchain probe answers once per binary identity: the resolved
// executable's path and file identity (store.FileIdentity), the probe's
// arguments, the variables it sets and unsets, the working directory it runs
// in, and the probe's self declaration. The run keeps each answer in memory
// under that identity and the whole probe environment (probeAnswers).
//
// A native executable with one link answers from a record in the artifact
// store's toolchainProbeDir, under its identity without the rest of the
// environment, when one of two rules says it answers its own version:
//
//   - a release the Putnami home's toolchains hold, which the engine unpacked
//     from the archive the workspace lock pins;
//   - a binary whose probe declares self, and whose self answer, checked once
//     when the engine records, names that same file (selfNamesExecutable).
//
// A binary whose self answer names another file, such as a dispatcher that
// runs the toolchain its configuration selects, is recorded with that verdict
// and no answer: later runs probe it once per run and check it no more. Any
// other executable is probed once per run, and so are a script and a file
// with several hard links. A probe that fails or times out is never kept. A
// self probe that fails, or names no file, counts as naming another file; one
// that times out records nothing.

// toolchainProbeDir is the directory of the artifact store root that holds the
// probe records, one file per binary identity.
const toolchainProbeDir = store.ToolchainProbeDir

// probeRecordSchema versions a probe record; a record of another schema is
// not read.
const probeRecordSchema = 1

// maxProbeRecord bounds the bytes read from a record: an answer is at most
// toolVersionMaxLen bytes, and the record adds its key.
const maxProbeRecord = 4096

// probeAnswers is the run's memo of probe answers, by probeIdentity key. A
// run empties it when it starts (ForgetProbeAnswers).
var probeAnswers sync.Map

// ForgetProbeAnswers empties the memo of probe answers, so the run that calls
// it starts with none: each answer it reads comes from a probe or a record.
// A concurrent run in the same process then probes again.
func ForgetProbeAnswers() {
	probeAnswers.Clear()
}

// probeRecord is the file a native executable's answer, or the verdict that
// it names another file, is recorded in. A record holds one or the other.
type probeRecord struct {
	Schema int    `json:"schema"`
	Key    string `json:"key"`
	Answer string `json:"answer,omitempty"`
	// NamesOther records that the binary's self answer named another file:
	// its version is probed once per run, and its self is not checked again.
	NamesOther bool `json:"namesOther,omitempty"`
}

// probeIdentity is the binary identity a probe of executable with args, the
// probe's own variables set (sorted NAME=value) and unset (sorted names), and
// its self declaration answers for, and env, the environment the probe runs
// under.
type probeIdentity struct {
	// key names the answer in memory: the binary identity and the whole
	// probe environment.
	key string
	// recordKey names the answer on disk: the binary identity alone.
	recordKey string
	// file is the executable's file identity, taken before the probe ran.
	file store.FileIdentity
	// native is true for an executable that is no script and has one link:
	// only such a file may be recorded.
	native bool
}

// probeRecordState is what a lookup of a probe answer found.
type probeRecordState int

const (
	// probeUnknown: neither the memo nor a record answers.
	probeUnknown probeRecordState = iota
	// probeAnswered: the memo or a record holds the answer.
	probeAnswered
	// probeNamesOther: a record states that the binary's self answer named
	// another file, so the binary is probed and its self is not checked.
	probeNamesOther
)

// identifyProbe returns the binary identity of a probe, and false when
// executable has no file identity on this platform: such a probe is never
// kept.
func identifyProbe(executable string, args, set, unset, env []string, self *extensionproto.RuntimeToolchainProbeSelf) (probeIdentity, bool) {
	identity, ok := store.StatFileIdentity(executable)
	if !ok {
		return probeIdentity{}, false
	}
	script, ok := startsWithShebang(executable)
	if !ok {
		return probeIdentity{}, false
	}
	dir, err := os.Getwd()
	if err != nil {
		return probeIdentity{}, false
	}
	parts := struct {
		Schema     int                `json:"schema"`
		Executable string             `json:"executable"`
		File       store.FileIdentity `json:"file"`
		Args       []string           `json:"args"`
		Set        []string           `json:"set"`
		Unset      []string           `json:"unset"`
		Dir        string             `json:"dir"`
		// Self is absent for a probe that declares none, so the identity of
		// such a probe is the one it had before the member existed.
		Self *extensionproto.RuntimeToolchainProbeSelf `json:"self,omitempty"`
	}{probeRecordSchema, executable, identity, args, set, unset, dir, self}
	record, err := json.Marshal(parts)
	if err != nil {
		return probeIdentity{}, false
	}
	environment, err := json.Marshal(slices.Sorted(slices.Values(env)))
	if err != nil {
		return probeIdentity{}, false
	}
	recordSum := sha256.Sum256(record)
	memorySum := sha256.Sum256(append(append(record, 0), environment...))
	return probeIdentity{
		key:       hex.EncodeToString(memorySum[:]),
		recordKey: hex.EncodeToString(recordSum[:]),
		file:      identity,
		native:    !script && identity.Links == 1,
	}, true
}

// startsWithShebang reports whether the file at path starts with "#!", and
// false as its second result when the file cannot be read.
func startsWithShebang(path string) (bool, bool) {
	file, err := os.Open(path)
	if err != nil {
		return false, false
	}
	defer func() { _ = file.Close() }()
	head := make([]byte, 2)
	n, err := io.ReadFull(file, head)
	if err != nil && n < len(head) {
		return false, errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
	}
	return string(head) == "#!", true
}

// recordedProbeAnswer returns the answer kept for identity in memory, or,
// when storeRoot is set and the executable is native, what its record under
// storeRoot states. A record answer it reads is kept in memory for the rest of
// the run.
func recordedProbeAnswer(storeRoot string, identity probeIdentity) (string, probeRecordState) {
	if kept, ok := probeAnswers.Load(identity.key); ok {
		if answer, ok := kept.(string); ok {
			return answer, probeAnswered
		}
	}
	if !identity.native || storeRoot == "" {
		return "", probeUnknown
	}
	file, err := os.Open(filepath.Join(storeRoot, toolchainProbeDir, identity.recordKey))
	if err != nil {
		return "", probeUnknown
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxProbeRecord+1))
	if err != nil || len(data) > maxProbeRecord {
		return "", probeUnknown
	}
	var record probeRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return "", probeUnknown
	}
	if record.Schema != probeRecordSchema || record.Key != identity.recordKey {
		return "", probeUnknown
	}
	switch {
	case record.NamesOther && record.Answer == "":
		store.KeepRecord(file)
		return "", probeNamesOther
	case !record.NamesOther && admissibleProbeAnswer(record.Answer):
		store.KeepRecord(file)
		probeAnswers.Store(identity.key, record.Answer)
		return record.Answer, probeAnswered
	}
	return "", probeUnknown
}

// keepProbeAnswer keeps answer, the answer of a probe that succeeded, in
// memory for identity, for the rest of the run.
func keepProbeAnswer(identity probeIdentity, answer string) {
	probeAnswers.Store(identity.key, answer)
}

// recordProbeAnswer records answer for identity, a native executable, in a
// record under storeRoot written by an atomic rename. A failed write is
// ignored: the next run probes again.
func recordProbeAnswer(storeRoot string, identity probeIdentity, answer string) {
	writeProbeRecord(storeRoot, identity, probeRecord{Schema: probeRecordSchema, Key: identity.recordKey, Answer: answer})
}

// recordNamesOther records that the self answer of identity, a native
// executable, named another file.
func recordNamesOther(storeRoot string, identity probeIdentity) {
	writeProbeRecord(storeRoot, identity, probeRecord{Schema: probeRecordSchema, Key: identity.recordKey, NamesOther: true})
}

// writeProbeRecord writes record for identity under storeRoot, by an atomic
// rename, when the executable is native and storeRoot is set.
func writeProbeRecord(storeRoot string, identity probeIdentity, record probeRecord) {
	if !identity.native || storeRoot == "" {
		return
	}
	data, err := json.Marshal(record)
	if err != nil {
		return
	}
	dir := filepath.Join(storeRoot, toolchainProbeDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, "."+identity.recordKey+"-")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, filepath.Join(dir, identity.recordKey)); err != nil {
		os.Remove(tmpName)
	}
}

// selfAnswerMaxLen is the longest self answer the engine reads: a path, which
// is longer than a version, under the probe's output limit.
const selfAnswerMaxLen = toolVersionProbeOutputLimit

// selfVerdict is what a self probe says of the binary it ran.
type selfVerdict int

const (
	// selfUnknown: the self probe timed out or the run was canceled, or the
	// binary moved or cannot be read while it ran; nothing is recorded, and
	// the next run checks again.
	selfUnknown selfVerdict = iota
	// selfNamesExecutable: the answer names the probed file, unchanged.
	selfNamesExecutable
	// selfNamesOther: the answer names another file, or no file: the self
	// probe failed, its answer is no absolute path, or the file it names does
	// not exist. A binary that cannot name itself is treated as one that names
	// another file, so it costs no second process on later runs.
	selfNamesOther
)

// checkSelf says whether executable, whose version probe answered got in the
// working directory, answers its own version wherever it runs. In an empty
// directory of its own, it runs the self probe and, when the executable its
// answer names, with its symbolic links resolved, is executable itself with
// the file identity identity took before the version probe, the version probe
// again: the binary names itself when that answer is got. A binary whose
// version follows the working directory, as a dispatcher that selects its
// target from it, names another file. The empty directory runs no
// configuration file of the working directory, which is code the repository
// controls.
func checkSelf(ctx context.Context, executable string, self extensionproto.RuntimeToolchainProbeSelf, versionArgs, probeEnv []string, identity probeIdentity, got string) selfVerdict {
	dir, err := os.MkdirTemp("", "putnami-probe-self-")
	if err != nil {
		slog.Debug("runtime toolchain: no empty directory to check a probe's self; its answer is not recorded", //nolint:gosec // G706: the executable is a resolved path and slog quotes attribute values
			"executable", executable, "error", err)
		return selfUnknown
	}
	defer func() { _ = os.RemoveAll(dir) }()
	answer, err := probeRuntimeToolchainAnswer(ctx, executable, dir, self.Args, probeEnv,
		procdeadline.HostProcessDeadline(runtimeToolchainProbeTimeout(ctx)), selfAnswerMaxLen)
	if err != nil && (ctx.Err() != nil || errors.Is(err, ErrToolchainProbeTimeout)) {
		return selfUnknown
	}
	if err != nil || !filepath.IsAbs(answer) {
		return selfNamesOther
	}
	named := answer
	if self.Path != "" {
		named = filepath.Join(answer, filepath.FromSlash(self.Path))
	}
	resolved, err := filepath.EvalSymlinks(named)
	if err != nil {
		return selfNamesOther
	}
	namedInfo, err := os.Stat(resolved)
	if err != nil {
		return selfNamesOther
	}
	info, err := os.Stat(executable)
	if err != nil {
		return selfUnknown
	}
	if !os.SameFile(namedInfo, info) {
		return selfNamesOther
	}
	if now, ok := store.FileIdentityOf(info); !ok || now != identity.file {
		return selfUnknown
	}
	elsewhere, err := probeRuntimeToolchainAnswer(ctx, executable, dir, versionArgs, probeEnv,
		procdeadline.HostProcessDeadline(runtimeToolchainProbeTimeout(ctx)), toolVersionMaxLen)
	switch {
	case err != nil && (ctx.Err() != nil || errors.Is(err, ErrToolchainProbeTimeout)):
		return selfUnknown
	case err != nil || elsewhere != got:
		return selfNamesOther
	}
	return selfNamesExecutable
}

// admissibleProbeAnswer reports whether answer is one a probe could return:
// one non-empty line of at most toolVersionMaxLen bytes.
func admissibleProbeAnswer(answer string) bool {
	return answer != "" && len(answer) <= toolVersionMaxLen && !strings.ContainsAny(answer, "\r\n")
}
