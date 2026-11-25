package job

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	diag "go.putnami.dev/protocol/diagnostic"
)

// EncodeCandidateFiles is the content of a candidate listing file (Tree's
// WorkspaceCandidates) and its digest, the lowercase hex SHA-256 of that
// content: the JSON array of files, in their order. The orchestrator writes
// it, and a job reads it back with ReadCandidateListing.
func EncodeCandidateFiles(files []CandidateFile) ([]byte, string, error) {
	if files == nil {
		files = []CandidateFile{}
	}
	data, err := json.Marshal(files)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

// ReadCandidateListing reads the candidate cut the listing names, the
// workspace's: it refuses a file whose bytes do not match the digest, and a
// cut that is not sorted, unique and made of clean paths and known modes.
func ReadCandidateListing(listing *CandidateListing) ([]CandidateFile, error) {
	if listing == nil {
		return nil, fmt.Errorf("no candidate listing")
	}
	data, err := os.ReadFile(listing.File)
	if err != nil {
		return nil, fmt.Errorf("read the candidate listing: %w", err)
	}
	sum := sha256.Sum256(data)
	if digest := hex.EncodeToString(sum[:]); digest != listing.Digest {
		return nil, fmt.Errorf("the candidate listing %s has digest %s, not %s", listing.File, digest, listing.Digest)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var files []CandidateFile
	if err := decoder.Decode(&files); err != nil {
		return nil, fmt.Errorf("decode the candidate listing %s: %w", listing.File, err)
	}
	if files == nil {
		return nil, fmt.Errorf("the candidate listing %s holds no array", listing.File)
	}
	if diags := ValidateCandidateFiles("workspaceCandidates", []string{"."}, files); diag.HasErrors(diags) {
		return nil, fmt.Errorf("the candidate listing %s: %s", listing.File, diags[0].Message)
	}
	return files, nil
}
