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
	defer func() { _ = in.Close() }()
	data, err := io.ReadAll(io.LimitReader(in, file.Size+1))
	if err != nil {
		return nil, fmt.Errorf("read outbox file %q: %w", file.Path, err)
	}
	if int64(len(data)) > file.Size {
		return nil, fmt.Errorf("outbox file %q size mismatch: it is longer than the descriptor's %d bytes", file.Path, file.Size)
	}
	if int64(len(data)) < file.Size {
		return nil, fmt.Errorf("outbox file %q size mismatch: descriptor %d bytes, file %d", file.Path, file.Size, len(data))
	}
	if digest := fmt.Sprintf("sha256:%x", sha256.Sum256(data)); digest != file.Digest {
		return nil, fmt.Errorf("outbox file %q digest mismatch: descriptor %s, file %s", file.Path, file.Digest, digest)
	}
	return data, nil
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
