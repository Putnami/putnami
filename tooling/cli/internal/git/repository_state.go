package git

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	protocoljob "go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/gitread"
	"go.putnami.dev/sdk/extension/procstat"
	"go.putnami.dev/sdk/extension/treefacts"
)

// The repository states RepositoryState distinguishes, the vocabulary of the
// job context's `tree.repository.state` (go.putnami.dev/protocol/job).
const (
	RepositoryUnmanaged = protocoljob.RepositoryUnmanaged
	RepositoryNoCommit  = protocoljob.RepositoryNoCommit
	RepositoryShallow   = protocoljob.RepositoryShallow
	RepositoryComplete  = protocoljob.RepositoryComplete
)

// RepositoryState is the state of the repository holding dir and the
// absolute root of its work tree (treefacts.RepositoryState, the answer a job
// gets from Git when its context carries none).
func RepositoryState(dir string) (state, root string, err error) {
	return treefacts.RepositoryState(dir)
}

// WriteFilesAtCommit writes under target the regular files commit holds at
// or below paths (slash paths relative to the work tree's root; "." is the
// whole tree), at the same relative paths. It reads raw blobs, as the commit's
// tree names them: no export attribute, filter or conversion applies, so the
// files are the content the tree object identifies, and a file the tree marks
// executable (100755) is executable. A path commit does not
// hold writes nothing. Two files whose names differ only in case are refused,
// as are a name that is not valid UTF-8 and a name holding `\` or `:`. Symbolic links and submodules are not files and are
// left out. It starts two processes, `ls-tree` and `cat-file --batch`, in
// root, the work tree's root.
func WriteFilesAtCommit(root, commit string, paths []string, target string) error {
	args := append([]string{"ls-tree", "-r", "-z", "--full-tree", commit, "--"}, paths...)
	listing, stderr, err := gitread.RunCaptureEnv(root, append(os.Environ(), "GIT_LITERAL_PATHSPECS=1"), args...)
	if err != nil {
		return fmt.Errorf("git ls-tree (in %s): %w: %s", root, err, strings.TrimSpace(stderr))
	}
	type file struct {
		object, name string
		executable   bool
	}
	var files []file
	folded := map[string]string{}
	for record := range strings.SplitSeq(listing, "\x00") {
		meta, name, found := strings.Cut(record, "\t")
		if !found {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
			continue
		}
		local := filepath.FromSlash(name)
		if strings.ContainsAny(name, "\\:") || !filepath.IsLocal(local) {
			return fmt.Errorf("git ls-tree (in %s): %q leaves the target directory", root, name)
		}
		// A name that is not valid UTF-8 is written under another name where
		// the platform converts it (Windows replaces each invalid byte), so two
		// such names can meet: refuse it on every platform.
		if !utf8.ValidString(name) {
			return fmt.Errorf("git ls-tree (in %s): %q is not valid UTF-8", root, name)
		}
		// On a file system that ignores case, the second of two names that
		// differ only in case would overwrite the first: refuse both, on every
		// platform. Unicode normalization (NFC, NFD), which APFS and HFS+ also
		// ignore, is not compared.
		key := strings.ToLower(name)
		if other, ok := folded[key]; ok {
			return fmt.Errorf("git ls-tree (in %s): %q and %q differ only in case", root, other, name)
		}
		folded[key] = name
		files = append(files, file{object: fields[2], name: local, executable: fields[0] == "100755"})
	}
	if len(files) == 0 {
		return nil
	}
	var request bytes.Buffer
	for _, f := range files {
		request.WriteString(f.object + "\n")
	}
	cmd := exec.Command(gitread.Executable(), "cat-file", "--batch")
	cmd.Dir = root
	cmd.Stdin = &request
	var catErr bytes.Buffer
	cmd.Stderr = &catErr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := procstat.Start(cmd, procstat.Git); err != nil {
		return fmt.Errorf("git cat-file (in %s): %w", root, err)
	}
	readErr := writeBatch(bufio.NewReader(stdout), target, len(files), func(i int) string { return files[i].name })
	if readErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := procstat.Wait(cmd, procstat.Git)
	if readErr != nil {
		return fmt.Errorf("git cat-file (in %s): %w", root, readErr)
	}
	if waitErr != nil {
		return fmt.Errorf("git cat-file (in %s): %w: %s", root, waitErr, strings.TrimSpace(catErr.String()))
	}
	for _, f := range files {
		if !f.executable {
			continue
		}
		if err := os.Chmod(filepath.Join(target, f.name), 0o755); err != nil { //nolint:gosec // the tree marks the file executable
			return fmt.Errorf("mark %s executable: %w", f.name, err)
		}
	}
	return nil
}

// writeBatch reads count `cat-file --batch` answers and writes the i-th to
// target joined with name(i).
func writeBatch(reader *bufio.Reader, target string, count int, name func(int) string) error {
	for i := range count {
		header, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[1] != "blob" {
			return fmt.Errorf("unexpected answer %q", strings.TrimSpace(header))
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return fmt.Errorf("unexpected answer %q", strings.TrimSpace(header))
		}
		path := filepath.Join(target, name(i))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		out, err := os.Create(path)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(out, reader, size)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if _, err := reader.Discard(1); err != nil {
			return err
		}
	}
	return nil
}

// IgnoredDirectories lists the directories Git ignores entirely under root,
// relative to it in slash form, sorted: what IgnoredDirs answers, with an
// error for a failure. Outside every repository the list is empty; a git that
// cannot run, or any other failure, is an error, never an empty answer.
func IgnoredDirectories(root string) ([]string, error) {
	output, stderr, err := gitread.RunCapture(root, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	if err != nil {
		if OutsideRepository(gitread.Unmanaged(root)) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("list the directories Git ignores in %s: %w: %s", root, err, strings.TrimSpace(stderr))
	}
	dirs := []string{}
	for entry := range strings.SplitSeq(output, "\x00") {
		if dir, ok := strings.CutSuffix(entry, "/"); ok && dir != "" {
			dirs = append(dirs, dir)
		}
	}
	slices.Sort(dirs)
	return slices.Compact(dirs), nil
}

// OutsideRepository reports whether err says a directory is outside every Git
// repository: an *gitread.UnmanagedError whose cause is
// gitread.ErrNotRepository. A missing git program is not that answer.
func OutsideRepository(err error) bool {
	var unmanaged *gitread.UnmanagedError
	return errors.As(err, &unmanaged) && errors.Is(unmanaged.Cause, gitread.ErrNotRepository)
}
