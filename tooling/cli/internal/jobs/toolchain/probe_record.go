package toolchain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"go.putnami.dev/tooling/cli/internal/store"
)

// A runtime toolchain probe answers once per binary identity: the resolved
// executable's path and file identity (store.FileIdentity), the probe's
// arguments, the variables it sets and unsets, and the working directory it
// runs in. The run keeps each answer in memory under that identity and the
// whole probe environment (probeAnswers).
//
// A release the Putnami home's toolchains hold, which the engine unpacked from
// the archive the workspace lock pins, answers its own version: its answer is
// also recorded in the artifact store's toolchainProbeDir, under its identity
// without the rest of the environment, so the next run reads the answer
// instead of starting the probe. The caller passes the store root for such a
// candidate alone. Any other executable is probed once per run: a shim or a
// dispatcher answers for the toolchain its configuration names, which no file
// identity follows. A script and a file with several hard links are never
// recorded either. A probe that fails or times out is never kept.

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

// probeRecord is the file a native executable's answer is recorded in.
type probeRecord struct {
	Schema int    `json:"schema"`
	Key    string `json:"key"`
	Answer string `json:"answer"`
}

// probeIdentity is the binary identity a probe of executable with args, the
// probe's own variables set (sorted NAME=value) and unset (sorted names)
// answers for, and env, the environment the probe runs under.
type probeIdentity struct {
	// key names the answer in memory: the binary identity and the whole
	// probe environment.
	key string
	// recordKey names the answer on disk: the binary identity alone.
	recordKey string
	// recorded is true for a native executable with one link, whose answer
	// is recorded in the artifact store.
	recorded bool
}

// identifyProbe returns the binary identity of a probe, and false when
// executable has no file identity on this platform: such a probe is never
// kept.
func identifyProbe(executable string, args, set, unset, env []string) (probeIdentity, bool) {
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
	}{probeRecordSchema, executable, identity, args, set, unset, dir}
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
		recorded:  !script && identity.Links == 1,
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

// recordedProbeAnswer returns the answer kept for identity: in memory, or,
// for a native executable, in the record under storeRoot. A record it reads
// is kept in memory for the rest of the run.
func recordedProbeAnswer(storeRoot string, identity probeIdentity) (string, bool) {
	if kept, ok := probeAnswers.Load(identity.key); ok {
		if answer, ok := kept.(string); ok {
			return answer, true
		}
	}
	if !identity.recorded || storeRoot == "" {
		return "", false
	}
	file, err := os.Open(filepath.Join(storeRoot, toolchainProbeDir, identity.recordKey))
	if err != nil {
		return "", false
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxProbeRecord+1))
	if err != nil || len(data) > maxProbeRecord {
		return "", false
	}
	var record probeRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return "", false
	}
	if record.Schema != probeRecordSchema || record.Key != identity.recordKey || !admissibleProbeAnswer(record.Answer) {
		return "", false
	}
	store.KeepRecord(file)
	probeAnswers.Store(identity.key, record.Answer)
	return record.Answer, true
}

// recordProbeAnswer keeps answer, the answer of a probe that succeeded, for
// identity: in memory, and, for a native executable, in a record under
// storeRoot written by an atomic rename. A failed write is ignored: the next
// run probes again.
func recordProbeAnswer(storeRoot string, identity probeIdentity, answer string) {
	probeAnswers.Store(identity.key, answer)
	if !identity.recorded || storeRoot == "" {
		return
	}
	data, err := json.Marshal(probeRecord{Schema: probeRecordSchema, Key: identity.recordKey, Answer: answer})
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

// admissibleProbeAnswer reports whether answer is one a probe could return:
// one non-empty line of at most toolVersionMaxLen bytes.
func admissibleProbeAnswer(answer string) bool {
	return answer != "" && len(answer) <= toolVersionMaxLen && !strings.ContainsAny(answer, "\r\n")
}
