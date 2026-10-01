package features

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	putnamigit "go.putnami.dev/tooling/sdd/extension/internal/gitread"
)

const (
	// MaxReadBytes bounds every document and referenced artifact read. Feature
	// metadata must stay reviewable; unbounded source or payload collection is
	// explicitly outside the protocol.
	MaxReadBytes = 16 << 20
	// MaxEvidenceFiles bounds one exact root's direct evidence fragments.
	MaxEvidenceFiles = 4096

	maxSemanticMetadataBytes = 512
	maxProtocolPathBytes     = 4096
)

// DirEntry is the minimal deterministic directory metadata discovery needs.
// Reader implementations may return entries in any order.
type DirEntry struct {
	Name        string
	IsDirectory bool
}

// Reader abstracts the selected repository state. Root and relative are both
// canonical slash paths relative to the workspace and selected discovery root,
// respectively. Implementations must resolve symlinks and enforce containment.
type Reader interface {
	ReadFile(root, relative string) ([]byte, error)
	ReadDir(root, relative string) ([]DirEntry, error)
	SourceBinding(root string) (string, error)
}

// ReaderErrorKind classifies containment and bounded-read failures without
// exposing host paths through error strings.
type ReaderErrorKind string

const (
	ReaderErrorInvalidPath       ReaderErrorKind = "invalid-path"
	ReaderErrorPathEscape        ReaderErrorKind = "path-escape"
	ReaderErrorSymlinkEscape     ReaderErrorKind = "symlink-escape"
	ReaderErrorOutsideWorkspace  ReaderErrorKind = "outside-workspace"
	ReaderErrorUnsupportedFile   ReaderErrorKind = "unsupported-file"
	ReaderErrorReadLimit         ReaderErrorKind = "read-limit"
	ReaderErrorEvidenceFileLimit ReaderErrorKind = "evidence-file-limit"
)

// ReaderError is intentionally redaction-safe. The underlying host error is
// retained for errors.Is but never rendered by the feature engine.
type ReaderError struct {
	Kind ReaderErrorKind
	err  error
}

func (err *ReaderError) Error() string { return "feature reader: " + string(err.Kind) }
func (err *ReaderError) Unwrap() error { return err.err }

func newReaderError(kind ReaderErrorKind, err error) error {
	return &ReaderError{Kind: kind, err: err}
}

func readerErrorKind(err error) (ReaderErrorKind, bool) {
	var target *ReaderError
	if errors.As(err, &target) {
		return target.Kind, true
	}
	return "", false
}

// OSReader reads the current worktree beneath one canonical workspace root.
type OSReader struct {
	root string
}

// NewOSReader constructs a contained current-worktree reader.
func NewOSReader(root string) (*OSReader, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("inspect workspace root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root is not a directory")
	}
	return &OSReader{root: filepath.Clean(resolved)}, nil
}

// ReadFile opens one bounded regular file after resolving containment against
// the exact discovery root.
func (reader *OSReader) ReadFile(root, relative string) ([]byte, error) {
	resolved, err := reader.resolve(root, relative)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(resolved) //nolint:gosec // resolve enforces the selected root
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, newReaderError(ReaderErrorUnsupportedFile, nil)
	}
	if info.Size() > MaxReadBytes {
		return nil, newReaderError(ReaderErrorReadLimit, nil)
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxReadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxReadBytes {
		return nil, newReaderError(ReaderErrorReadLimit, nil)
	}
	return data, nil
}

// ReadDir lists only the requested exact directory and never walks descendants.
func (reader *OSReader) ReadDir(root, relative string) ([]DirEntry, error) {
	resolved, err := reader.resolve(root, relative)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return nil, err
	}
	if len(entries) > MaxEvidenceFiles {
		return nil, newReaderError(ReaderErrorEvidenceFileLimit, nil)
	}
	result := make([]DirEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, DirEntry{Name: entry.Name(), IsDirectory: entry.IsDir()})
	}
	return result, nil
}

// SourceBinding recomputes source-v1 over one exact workspace-relative root.
func (reader *OSReader) SourceBinding(root string) (string, error) {
	resolved := reader.root
	if root != "" {
		var err error
		resolved, err = reader.resolve("", root)
		if err != nil {
			return "", err
		}
	}
	return putnamigit.ProjectSourceBinding(reader.root, resolved)
}

func (reader *OSReader) resolve(root, relative string) (string, error) {
	if code := validateRelativePath(root, true); code != "" {
		return "", newReaderError(code, nil)
	}
	if code := validateRelativePath(relative, false); code != "" {
		return "", newReaderError(code, nil)
	}
	discoveryRoot := reader.root
	if root != "" {
		discoveryRoot = filepath.Join(reader.root, filepath.FromSlash(root))
	}
	resolvedRoot, err := filepath.EvalSymlinks(discoveryRoot)
	if err != nil {
		return "", err
	}
	if !pathContained(reader.root, resolvedRoot) {
		return "", newReaderError(ReaderErrorOutsideWorkspace, nil)
	}
	target := filepath.Join(discoveryRoot, filepath.FromSlash(relative))
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		// A present leaf whose symlink target vanished is malformed rather than
		// an optional absent artifact.
		if errors.Is(err, fs.ErrNotExist) {
			if _, statErr := os.Lstat(target); statErr == nil {
				return "", newReaderError(ReaderErrorUnsupportedFile, err)
			}
		}
		return "", err
	}
	if !pathContained(resolvedRoot, resolvedTarget) {
		return "", newReaderError(ReaderErrorSymlinkEscape, nil)
	}
	return resolvedTarget, nil
}

func pathContained(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

var pathSchemePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

func validateRelativePath(value string, allowEmpty bool) ReaderErrorKind {
	if value == "" {
		if allowEmpty {
			return ""
		}
		return ReaderErrorInvalidPath
	}
	if len(value) > maxProtocolPathBytes {
		return ReaderErrorInvalidPath
	}
	if strings.ContainsAny(value, "\\\x00") || strings.HasPrefix(value, "/") || pathSchemePattern.MatchString(value) {
		return ReaderErrorPathEscape
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return ReaderErrorInvalidPath
		}
	}
	if path.Clean(value) != value || value == "." {
		for _, segment := range strings.Split(value, "/") {
			if segment == ".." {
				return ReaderErrorPathEscape
			}
		}
		return ReaderErrorInvalidPath
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return ReaderErrorPathEscape
		}
		if segment == "" || segment == "." {
			return ReaderErrorInvalidPath
		}
	}
	return ""
}
