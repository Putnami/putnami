// Task-contract digest: the cache-key input derived from a task's declared
// contract.
//
// One task's contract, canonically serialized and hashed. The digest is what
// lets cache key v5 miss — instead of falsely hit — when a task's declared
// contract changes: binding invariant 6 activates it BEFORE output ownership
// changes (B3 declarations, B4 task-owned capture), so an entry written under
// one contract can never satisfy a task running under another.
//
// Two properties are load-bearing:
//
//   - SELF-NORMALIZING. The negotiating loader (LoadManifest) does not run
//     NormalizeManifest, while the strict path does. The digest normalizes its
//     own private copy (NormalizeTask: derived cache key, sorted effects,
//     cleaned declared paths), so the same manifest bytes digest identically
//     on every load path and independent of authoring order. Map members are
//     order-free by Go's sorted map marshaling; list members whose order is
//     meaningful (args, pipeline reads/writes) stay order-sensitive on
//     purpose — reordering them is conservatively a contract change.
//
//   - CONTRACT-SCOPED. Presentational members (task description and
//     visibility, port and declared-output descriptions) are cleared before
//     hashing: a docs-only manifest edit must not cold every cache. Every
//     other member — command, args, cwd, env, timeout, ports, reads/writes,
//     cache policy, batch policy, and the v3 declares block — IS contract,
//     and moving any of them moves the digest. A field added to
//     TaskDefinition later is contract BY DEFAULT (it enters the marshaled
//     view automatically), which is invariant 6's intent: contract evolution
//     invalidates v5 entries rather than silently reusing them.
//
// taskDigestFormat versions the serialization itself and is hashed with it.
// Bumping it moves every digest at once — a deliberate whole-cache miss — and
// is the reviewed lever for changing what the digest covers.

package extension

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// taskDigestFormat names the digest serialization format. It is part of the
// hashed payload and the digest's textual prefix.
const taskDigestFormat = "tc1"

// TaskContractDigest returns the canonical digest of one task's contract.
//
// The empty-definition digest is as valid as any other: a job synthesized by
// the CLI with no manifest task simply carries no digest (the CLI leaves the
// key field empty), while a manifest task whose members are all zero digests
// to the stable tc1 value of that empty contract. The caller's TaskDefinition
// is never mutated.
func TaskContractDigest(task TaskDefinition) string {
	view, err := taskDigestView(task)
	if err != nil {
		// TaskDefinition is JSON-native by construction — it is parsed from
		// manifest JSON, and every member marshals totally. If a future field
		// breaks that property, failing loudly here beats returning a shared
		// sentinel that would let two different contracts serve each other's
		// cache entries.
		panic(fmt.Sprintf("extension: task contract digest is not serializable: %v", err))
	}
	sum := sha256.Sum256(view)
	return taskDigestFormat + ":" + hex.EncodeToString(sum[:])
}

// taskDigestView produces the canonical bytes the digest hashes: a deep copy
// of the task, normalized, with presentational members cleared, wrapped with
// the format marker.
func taskDigestView(task TaskDefinition) ([]byte, error) {
	copied, err := deepCopyTask(task)
	if err != nil {
		return nil, err
	}
	NormalizeTask(&copied)

	copied.Description = ""
	copied.Visibility = ""
	for id, port := range copied.Outputs {
		if port.Description != "" {
			port.Description = ""
			copied.Outputs[id] = port
		}
	}
	if copied.Declares != nil {
		for id, output := range copied.Declares.Outputs {
			if output.Description != "" {
				output.Description = ""
				copied.Declares.Outputs[id] = output
			}
		}
	}

	return json.Marshal(struct {
		Format string         `json:"format"`
		Task   TaskDefinition `json:"task"`
	}{taskDigestFormat, copied})
}

// deepCopyTask copies a TaskDefinition through a JSON round trip so that
// normalizing and clearing members for the digest can never write through a
// shared map or slice into the caller's manifest. TaskCachePolicy's custom
// bool-or-object unmarshaling accepts its own marshaled object form, so the
// round trip is lossless.
func deepCopyTask(task TaskDefinition) (TaskDefinition, error) {
	raw, err := json.Marshal(task)
	if err != nil {
		return TaskDefinition{}, err
	}
	var copied TaskDefinition
	if err := json.Unmarshal(raw, &copied); err != nil {
		return TaskDefinition{}, err
	}
	return copied, nil
}
