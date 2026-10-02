package publicationoutbox

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/internal/regularfile"
)

// Outbox is a publication outbox whose descriptor has been read and
// validated. It reads only the paths the descriptor names.
type Outbox struct {
	root string
	// Descriptor is the validated outbox.json.
	Descriptor *extproto.PublicationOutbox
}

// Read reads and validates the descriptor of the outbox at root. root must be
// a directory, not a symbolic link, and outbox.json a regular file of at most
// MaxPublicationOutboxBytes.
func Read(root string) (*Outbox, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("publication outbox: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("publication outbox %s is not a directory", root)
	}
	file, err := regularfile.Open(filepath.Join(root, extproto.PublicationOutboxDescriptor))
	if err != nil {
		return nil, fmt.Errorf("read publication outbox: %w", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, extproto.MaxPublicationOutboxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read publication outbox: %w", err)
	}
	if len(data) > extproto.MaxPublicationOutboxBytes {
		return nil, fmt.Errorf("publication outbox descriptor exceeds %d bytes", extproto.MaxPublicationOutboxBytes)
	}
	descriptor, diagnostics := extproto.ParsePublicationOutbox(data)
	if len(diagnostics) > 0 {
		return nil, descriptorError(diagnostics)
	}
	return &Outbox{root: root, Descriptor: descriptor}, nil
}

// Root returns the outbox directory.
func (o *Outbox) Root() string { return o.root }

// ReadFile returns the bytes of the artifact file names, read once. It refuses
// a path that leaves the outbox or crosses a symbolic link, a file that is not
// regular, and bytes whose size or digest differ from file's. A file replaced
// by a FIFO is refused without waiting for a writer. The returned bytes are
// the bytes hashed, so a caller uploads exactly what was verified.
func (o *Outbox) ReadFile(file extproto.OutboxFile) ([]byte, error) {
	in, err := o.open(file)
	if err != nil {
		return nil, err
	}
	defer func() { _ = in.Close() }()
	data, err := io.ReadAll(io.LimitReader(in, file.Size+1))
	if err != nil {
		return nil, fmt.Errorf("read outbox file %q: %w", file.Path, err)
	}
	if err := checkContent(file, int64(len(data)), sha256.Sum256(data)); err != nil {
		return nil, err
	}
	return data, nil
}

// VerifyFile refuses the artifact file names on the same grounds as ReadFile,
// streaming the bytes through the digest without holding them in memory. A
// file that passes may still change before it is read, so the bytes uploaded
// are the bytes ReadFile returns, verified again.
func (o *Outbox) VerifyFile(file extproto.OutboxFile) error {
	in, err := o.open(file)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(in, file.Size+1))
	if err != nil {
		return fmt.Errorf("read outbox file %q: %w", file.Path, err)
	}
	return checkContent(file, size, [sha256.Size]byte(hash.Sum(nil)))
}

// open opens the regular file the descriptor file names, inside the outbox.
func (o *Outbox) open(file extproto.OutboxFile) (*os.File, error) {
	full, err := extproto.ResolveOutboxPath(o.root, file.Path)
	if err != nil {
		return nil, err
	}
	if file.Size < 1 {
		return nil, fmt.Errorf("outbox file %q has no size", file.Path)
	}
	in, err := regularfile.Open(full)
	if err != nil {
		return nil, fmt.Errorf("outbox file %q: %w", file.Path, err)
	}
	return in, nil
}

// checkContent refuses size bytes of digest sum, read from file through a
// limit of one byte past its size, unless they are the bytes file names.
func checkContent(file extproto.OutboxFile, size int64, sum [sha256.Size]byte) error {
	if size > file.Size {
		return fmt.Errorf("outbox file %q size mismatch: it is longer than the descriptor's %d bytes", file.Path, file.Size)
	}
	if size < file.Size {
		return fmt.Errorf("outbox file %q size mismatch: descriptor %d bytes, file %d", file.Path, file.Size, size)
	}
	if digest := fmt.Sprintf("sha256:%x", sum); digest != file.Digest {
		return fmt.Errorf("outbox file %q digest mismatch: descriptor %s, file %s", file.Path, file.Digest, digest)
	}
	return nil
}

// Layout returns the absolute path of the OCI layout directory rel names. It
// refuses a path that leaves the outbox, and a layout that is, or holds, a
// symbolic link or another entry that is not a regular file or a directory.
// The manifest digest is checked where the layout is pushed.
func (o *Outbox) Layout(rel string) (string, error) {
	full, err := extproto.ResolveOutboxPath(o.root, rel)
	if err != nil {
		return "", err
	}
	if err := checkTree(full); err != nil {
		return "", fmt.Errorf("outbox layout %q: %w", rel, err)
	}
	return full, nil
}
