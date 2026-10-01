package depsupgrade

import (
	"bytes"
	"encoding/json"

	"go.putnami.dev/go/extension/internal/workspacejob"
)

// ReleaseSet is the Go projection of the release the orchestrator resolved.
type ReleaseSet struct {
	// ID and Digest name the validated release reference.
	ID     string
	Digest string
	// Versions maps each Go member coordinate to its exact version.
	Versions map[string]string
}

// The diagnostics a malformed release set fails the job with.
const (
	problemInvalidMap   = "Invalid Go member map in release-set snapshot"
	problemProjection   = "Release-set snapshot contains a non-string or duplicate Go member projection"
	problemMissingRefID = "Release-set snapshot is missing its validated reference"
)

// ParseReleaseSet projects the Go members of a release-set response and its
// reference. It returns the diagnostic to fail with when the response has no
// iterable member list, when a Go member has a non-string coordinate or
// version or repeats a coordinate, or when the reference id or digest is not a
// string.
//
// The CLI strict-parses the response through protocol/distribution before it
// injects it, so this projects only what the job consumes, with the edges the
// script's jq program had: members may be an array or an object, a null
// member is skipped, and the last of two equal coordinates wins before the
// count check refuses the set.
func ParseReleaseSet(raw json.RawMessage) (*ReleaseSet, string) {
	response, ok := object(raw)
	if !ok {
		return nil, problemInvalidMap
	}
	inner, ok := object(response["releaseSet"])
	if !ok {
		return nil, problemInvalidMap
	}
	members, ok := iterate(inner["members"])
	if !ok {
		return nil, problemInvalidMap
	}

	versions := map[string]string{}
	goMembers := 0
	for _, member := range members {
		if isNull(member) {
			continue
		}
		fields, ok := object(member)
		if !ok {
			return nil, problemInvalidMap
		}
		if ecosystem, isString := jsonString(fields["ecosystem"]); !isString || ecosystem != "go" {
			continue
		}
		goMembers++
		coordinate, coordinateOK := jsonString(fields["coordinate"])
		version, versionOK := jsonString(fields["version"])
		if coordinateOK && versionOK {
			versions[coordinate] = version
		}
	}
	if len(versions) != goMembers {
		return nil, problemProjection
	}

	ref, _ := object(response["ref"])
	id, idOK := jsonString(ref["id"])
	digest, digestOK := jsonString(ref["digest"])
	if !idOK || !digestOK {
		return nil, problemMissingRefID
	}
	return &ReleaseSet{ID: id, Digest: digest, Versions: versions}, ""
}

// object decodes a JSON object; anything else, null included, reports false.
func object(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if !bytes.HasPrefix(trimmed, []byte("{")) {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return nil, false
	}
	return fields, true
}

// iterate is jq's `.[]`: the elements of an array or the member values of an
// object. null and scalars report false, as jq fails on them.
func iterate(raw json.RawMessage) ([]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || (trimmed[0] != '[' && trimmed[0] != '{') {
		return nil, false
	}
	return workspacejob.IterateOrEmpty(trimmed)
}

func isNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

func jsonString(raw json.RawMessage) (string, bool) {
	return workspacejob.JSONString(raw)
}
