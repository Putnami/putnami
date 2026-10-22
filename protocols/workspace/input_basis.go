package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// inputBasisDigestDomain opens every digest InputBasisDigest hashes, so the
// digest of one record format never equals the digest of another.
const inputBasisDigestDomain = "putnami input basis v1\n"

// The kinds of path a basis record names.
const (
	inputBasisKindFile    = "file"
	inputBasisKindDir     = "dir"
	inputBasisKindSymlink = "symlink"
	inputBasisKindOther   = "other"
)

// InputBasisDigest returns the sha256 hex digest of the tree under root, an
// absolute project directory, as basis selects it.
//
// An input-resolve answer stays exact while this digest is unchanged,
// provided the extension's basis selects every path whose bytes (content) or
// whose existence, kind or link target (listing) its answer read.
//
// The walk never follows a link. Each path below root is named by its
// root-relative slash path; root itself is not a record. A set selects a path
// when SelectsPath of that set alone selects it. For each path a Content set
// selects, the digest records its kind and, for a regular file, the sha256 of
// its bytes, or, for a link, its target text. For each path a Listing set
// selects, it records its kind and, for a link, its target text. A path both
// kinds select gets both records. The records are sorted before they are
// hashed, so the digest does not depend on the walk's order, on root's
// location, or on the host. A directory no set can select, at or below it, is
// not walked.
//
// A basis that ValidateInputResolveResult would refuse, a root that is
// missing, relative or not a directory, and any error of the walk are errors.
func InputBasisDigest(root string, basis InputBasis) (string, error) {
	records, _, err := walkInputBasis(root, basis, true)
	if err != nil {
		return "", err
	}
	return hashInputBasisRecords(records), nil
}

// walkInputBasis returns the records of the tree under root that basis
// selects, and the number of paths it visited. With prune false it walks the
// whole tree, which yields the same records.
func walkInputBasis(root string, basis InputBasis, prune bool) ([]string, int, error) {
	if diags := validateInputBasis("basis", basis); diag.HasErrors(diags) {
		return nil, 0, fmt.Errorf("input basis rejected: %s", formatProbeDiagnostics(diags))
	}
	if !filepath.IsAbs(root) {
		return nil, 0, fmt.Errorf("input basis root %q is not an absolute path", root)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, 0, fmt.Errorf("input basis root: %w", err)
	}
	if !info.IsDir() {
		return nil, 0, fmt.Errorf("input basis root %s is not a directory", root)
	}

	content, listing := splitBasisSets(basis.Content), splitBasisSets(basis.Listing)
	var records []string
	visited := 0
	err = filepath.WalkDir(root, func(abs string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if abs == root {
			return nil
		}
		visited++
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		kind := inputBasisKind(entry.Type())
		if selectsAny(content, rel) {
			payload, err := inputBasisContentPayload(abs, kind)
			if err != nil {
				return err
			}
			records = append(records, inputBasisRecord("c", rel, kind, payload))
		}
		if selectsAny(listing, rel) {
			payload := ""
			if kind == inputBasisKindSymlink {
				if payload, err = os.Readlink(abs); err != nil {
					return err
				}
			}
			records = append(records, inputBasisRecord("l", rel, kind, payload))
		}
		if prune && entry.IsDir() && !mayReachAtOrBelow(content, rel) && !mayReachAtOrBelow(listing, rel) {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("walk input basis under %s: %w", root, err)
	}
	return records, visited, nil
}

// hashInputBasisRecords hashes the records in sorted order.
func hashInputBasisRecords(records []string) string {
	sorted := append([]string(nil), records...)
	sort.Strings(sorted)
	h := sha256.New()
	_, _ = io.WriteString(h, inputBasisDigestDomain)
	for _, record := range sorted {
		_, _ = io.WriteString(h, record)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// inputBasisRecord frames one record. The path and the payload are quoted, so
// no byte of either can be read as a separator.
func inputBasisRecord(tag, rel, kind, payload string) string {
	return tag + " " + strconv.Quote(rel) + " " + kind + " " + strconv.Quote(payload) + "\n"
}

// inputBasisKind names the kind of a directory entry from its type bits, which
// the walk reads without following a link.
func inputBasisKind(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return inputBasisKindSymlink
	case mode.IsDir():
		return inputBasisKindDir
	case mode.IsRegular():
		return inputBasisKindFile
	default:
		return inputBasisKindOther
	}
}

// inputBasisContentPayload is what a content record binds beside the kind: the
// sha256 of a regular file's bytes, a link's target text, or nothing.
func inputBasisContentPayload(abs, kind string) (string, error) {
	switch kind {
	case inputBasisKindFile:
		file, err := os.Open(abs)
		if err != nil {
			return "", err
		}
		defer file.Close()
		h := sha256.New()
		if _, err := io.Copy(h, file); err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	case inputBasisKindSymlink:
		return os.Readlink(abs)
	default:
		return "", nil
	}
}

// basisSet is one pattern set, split once into its includes and exclusions.
type basisSet struct {
	includes []string
	excludes []string
}

func splitBasisSets(sets [][]string) []basisSet {
	split := make([]basisSet, 0, len(sets))
	for _, set := range sets {
		includes, excludes := SplitFilePatterns(set)
		split = append(split, basisSet{includes: includes, excludes: excludes})
	}
	return split
}

// selectsAny reports whether a set selects rel, with SelectsPath's rule.
func selectsAny(sets []basisSet, rel string) bool {
	for _, set := range sets {
		if !MatchesAnyFilePattern(rel, set.excludes) && MatchesAnyFilePattern(rel, set.includes) {
			return true
		}
	}
	return false
}

// mayReachAtOrBelow reports whether a set may select the directory dir or a
// path below it. It answers false only when, for every set, the exclusions
// remove the whole tree (ExcludesTree) or no include can match dir or a path
// below it, so a walk that skips such a directory records what a full walk
// records.
func mayReachAtOrBelow(sets []basisSet, dir string) bool {
	for _, set := range sets {
		if ExcludesTree(dir, set.excludes) {
			continue
		}
		for _, include := range set.includes {
			if includeMayMatchAtOrBelow(include, dir) {
				return true
			}
		}
	}
	return false
}

// includeMayMatchAtOrBelow reports whether pattern may match dir or a path
// below it, with MatchFilePattern's rule. A pattern without "**" is matched
// against the whole path by path.Match, where a character class can match
// "/", so such a pattern with a class is taken to reach any tree. Every other
// pattern matches segment by segment, and is compared with dir that way.
func includeMayMatchAtOrBelow(pattern, dir string) bool {
	pattern, _ = GitFilePattern(pattern)
	pattern = strings.TrimPrefix(filepath.ToSlash(pattern), "./")
	if !strings.Contains(pattern, "**") && strings.Contains(pattern, "[") {
		return true
	}
	return segmentsMayMatchAtOrBelow(strings.Split(pattern, "/"), strings.Split(dir, "/"))
}

// segmentsMayMatchAtOrBelow reports whether the pattern segments can match a
// path that starts with the dir segments: dir itself or a path below it. A
// "**" segment can match any rest; any other segment matches one dir segment
// by path.Match.
func segmentsMayMatchAtOrBelow(patterns, dir []string) bool {
	if len(dir) == 0 {
		return true
	}
	if len(patterns) == 0 {
		return false
	}
	if patterns[0] == "**" {
		return true
	}
	ok, _ := path.Match(patterns[0], dir[0])
	return ok && segmentsMayMatchAtOrBelow(patterns[1:], dir[1:])
}
