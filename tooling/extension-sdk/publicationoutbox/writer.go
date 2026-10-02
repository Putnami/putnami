// Package publicationoutbox writes and reads a publication outbox: the private
// directory where a publication job leaves the artifacts it packed and the
// outbox.json descriptor that lists them (go.putnami.dev/protocol/extension).
//
// A job writes with Writer and uploads nothing. The engine reads with Read
// after the job exits: every artifact is read once, hashed, and refused unless
// its size and digest equal the descriptor's, so the bytes it uploads are the
// bytes it verified.
package publicationoutbox

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/internal/regularfile"
)

// Writer stages one job's artifacts in an outbox directory and writes the
// descriptor last. One Writer owns an outbox; it is not safe for concurrent
// use.
type Writer struct {
	root      string
	files     map[string]extproto.OutboxFile
	layouts   map[string]bool
	members   []extproto.OutboxMember
	committed bool
}

// WriterFromEnv returns a Writer for the outbox that PUTNAMI_PUBLICATION_OUTBOX
// names. It is an error when the variable is unset or not an absolute path.
func WriterFromEnv() (*Writer, error) {
	root := os.Getenv(extproto.PublicationOutboxEnv)
	if root == "" {
		return nil, fmt.Errorf("%s is not set", extproto.PublicationOutboxEnv)
	}
	return NewWriter(root)
}

// NewWriter returns a Writer for the outbox at root, an absolute path. root is
// created with mode 0700 when it does not exist. It must not be a symbolic
// link and must not hold a descriptor yet.
func NewWriter(root string) (*Writer, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("publication outbox %q is not an absolute path", root)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create publication outbox: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("publication outbox: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("publication outbox %s is not a directory", root)
	}
	if _, err := os.Lstat(filepath.Join(root, extproto.PublicationOutboxDescriptor)); !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("publication outbox %s already holds a descriptor", root)
	}
	return &Writer{root: root, files: map[string]extproto.OutboxFile{}, layouts: map[string]bool{}}, nil
}

// Root returns the outbox directory.
func (w *Writer) Root() string { return w.root }

// WriteFile writes data as a new file at rel, relative to the outbox root, and
// returns its descriptor entry.
func (w *Writer) WriteFile(rel string, data []byte) (extproto.OutboxFile, error) {
	return w.createFile(rel, func(out io.Writer) error {
		_, err := out.Write(data)
		return err
	})
}

// CopyFile copies the regular file at src to a new file at rel and returns its
// descriptor entry. A src that is a symbolic link is refused.
func (w *Writer) CopyFile(rel, src string) (extproto.OutboxFile, error) {
	in, err := regularfile.Open(src)
	if err != nil {
		return extproto.OutboxFile{}, err
	}
	defer func() { _ = in.Close() }()
	return w.createFile(rel, func(out io.Writer) error {
		_, err := io.Copy(out, in)
		return err
	})
}

// CopyLayout copies the OCI image layout directory src to a new directory at
// rel. A src that is, or holds, a symbolic link or another entry that is not a
// regular file or a directory is refused.
func (w *Writer) CopyLayout(rel, src string) error {
	if err := w.checkNewPath(rel); err != nil {
		return err
	}
	if err := checkTree(src); err != nil {
		return err
	}
	target := filepath.Join(w.root, filepath.FromSlash(rel))
	if err := w.makeParents(rel); err != nil {
		return err
	}
	err := filepath.WalkDir(src, func(entryPath string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(src, entryPath)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		if entry.IsDir() {
			return os.Mkdir(destination, 0o700)
		}
		in, err := regularfile.Open(entryPath)
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	})
	if err != nil {
		return fmt.Errorf("copy OCI layout to %q: %w", rel, err)
	}
	w.layouts[rel] = true
	return nil
}

// Add records member. Every file and layout it names must have been written by
// this Writer, with the digest and size the Writer returned.
func (w *Writer) Add(member extproto.OutboxMember) error {
	if w.committed {
		return errors.New("publication outbox is already committed")
	}
	for _, file := range memberFiles(member) {
		if written, ok := w.files[file.Path]; !ok || written != file {
			return fmt.Errorf("outbox member %s %s names %q, which this outbox did not write with that digest and size", member.Ecosystem, member.Coordinate, file.Path)
		}
	}
	if member.OCI != nil && !w.layouts[member.OCI.Layout] {
		return fmt.Errorf("outbox member %s %s names layout %q, which this outbox did not write", member.Ecosystem, member.Coordinate, member.OCI.Layout)
	}
	candidate := extproto.PublicationOutbox{
		ProtocolVersion: extproto.PublicationOutboxVersion,
		Members:         append(append([]extproto.OutboxMember{}, w.members...), member),
	}
	if diagnostics := extproto.ValidatePublicationOutbox(&candidate); len(diagnostics) > 0 {
		return descriptorError(diagnostics)
	}
	w.members = candidate.Members
	return nil
}

// Commit writes outbox.json with every added member. The descriptor appears
// whole or not at all: it is written to a temporary file in the outbox and
// renamed into place. A Writer commits once.
func (w *Writer) Commit() error {
	if w.committed {
		return errors.New("publication outbox is already committed")
	}
	descriptor := extproto.PublicationOutbox{
		ProtocolVersion: extproto.PublicationOutboxVersion,
		Members:         append([]extproto.OutboxMember{}, w.members...),
	}
	data, err := json.MarshalIndent(descriptor, "", "  ")
	if err != nil {
		return fmt.Errorf("encode publication outbox: %w", err)
	}
	data = append(data, '\n')
	// The engine parses with ParsePublicationOutbox; a descriptor it would
	// refuse is never written.
	if _, diagnostics := extproto.ParsePublicationOutbox(data); len(diagnostics) > 0 {
		return descriptorError(diagnostics)
	}
	temporary, err := os.CreateTemp(w.root, ".outbox-*.json")
	if err != nil {
		return fmt.Errorf("write publication outbox: %w", err)
	}
	defer func() { _ = os.Remove(temporary.Name()) }()
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write publication outbox: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write publication outbox: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("write publication outbox: %w", err)
	}
	if err := os.Rename(temporary.Name(), filepath.Join(w.root, extproto.PublicationOutboxDescriptor)); err != nil {
		return fmt.Errorf("write publication outbox: %w", err)
	}
	w.committed = true
	return nil
}

// createFile creates rel exclusively, fills it with write, and returns its
// descriptor entry, hashed from the bytes written.
func (w *Writer) createFile(rel string, write func(io.Writer) error) (extproto.OutboxFile, error) {
	if err := w.checkNewPath(rel); err != nil {
		return extproto.OutboxFile{}, err
	}
	if err := w.makeParents(rel); err != nil {
		return extproto.OutboxFile{}, err
	}
	out, err := os.OpenFile(filepath.Join(w.root, filepath.FromSlash(rel)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return extproto.OutboxFile{}, fmt.Errorf("write outbox file %q: %w", rel, err)
	}
	digest := sha256.New()
	counter := &countingWriter{}
	if err := write(io.MultiWriter(out, digest, counter)); err != nil {
		_ = out.Close()
		return extproto.OutboxFile{}, fmt.Errorf("write outbox file %q: %w", rel, err)
	}
	if err := out.Close(); err != nil {
		return extproto.OutboxFile{}, fmt.Errorf("write outbox file %q: %w", rel, err)
	}
	file := extproto.OutboxFile{Path: rel, Digest: digestString(digest), Size: counter.n}
	w.files[rel] = file
	return file, nil
}

// checkNewPath refuses rel when it is not a valid outbox path, when the Writer
// is committed, or when it equals or nests with a path already written.
func (w *Writer) checkNewPath(rel string) error {
	if w.committed {
		return errors.New("publication outbox is already committed")
	}
	if err := extproto.ValidOutboxPath(rel); err != nil {
		return err
	}
	for written := range w.files {
		if pathsOverlap(written, rel) {
			return fmt.Errorf("outbox path %q overlaps %q", rel, written)
		}
	}
	for written := range w.layouts {
		if pathsOverlap(written, rel) {
			return fmt.Errorf("outbox path %q overlaps %q", rel, written)
		}
	}
	return nil
}

// makeParents creates the directories above rel with mode 0700 and refuses a
// parent that is a symbolic link or not a directory.
func (w *Writer) makeParents(rel string) error {
	parent := path.Dir(rel)
	if parent == "." {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(w.root, filepath.FromSlash(parent)), 0o700); err != nil {
		return fmt.Errorf("create outbox directory %q: %w", parent, err)
	}
	full, err := extproto.ResolveOutboxPath(w.root, parent)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(full); err != nil || !info.IsDir() {
		return fmt.Errorf("outbox path %q is not a directory", parent)
	}
	return nil
}

func pathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func memberFiles(member extproto.OutboxMember) []extproto.OutboxFile {
	var files []extproto.OutboxFile
	if member.NPM != nil {
		files = append(files, member.NPM.Tarball, member.NPM.Manifest)
	}
	if member.Go != nil {
		files = append(files, member.Go.Zip, member.Go.Mod, member.Go.Info)
	}
	if member.Put != nil {
		files = append(files, member.Put.Manifest)
		for _, blob := range member.Put.Blobs {
			files = append(files, blob.File())
		}
	}
	return files
}

// checkTree refuses a directory that is a symbolic link or holds an entry that
// is not a regular file or a directory.
func checkTree(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	return filepath.WalkDir(dir, func(entryPath string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Type().IsRegular() {
			return nil
		}
		return fmt.Errorf("%s is not a regular file or a directory", entryPath)
	})
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

func digestString(h hash.Hash) string {
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}

func descriptorError(diagnostics []diag.Diagnostic) error {
	messages := make([]string, 0, len(diagnostics))
	for _, d := range diagnostics {
		messages = append(messages, d.String())
	}
	return fmt.Errorf("invalid publication outbox: %s", strings.Join(messages, "; "))
}
