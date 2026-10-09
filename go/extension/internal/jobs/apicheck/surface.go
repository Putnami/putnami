package apicheck

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	pctx "go.putnami.dev/sdk/extension/context"
)

// commandSurfaceOption is the validate command's flag, and project option,
// that names a project's command-surface document.
const commandSurfaceOption = "command-surface"

// Options are a project's options for one check.
type Options struct {
	// CommandSurface is the path of the project's command-surface document
	// (go.putnami.dev/protocol/cli CommandSurface), relative to the project
	// directory; empty when the project declares none. When it is set, the
	// check also compares the document with the one at the line's last tag.
	CommandSurface string
}

// optionsFromParams reads a check's options from the task parameters: the
// validate command's flags over the project's and the workspace's options,
// under the kebab-case or the camelCase spelling. A value that is not a
// string is invalid configuration; null is no value.
func optionsFromParams(params pctx.Params) (Options, error) {
	for _, key := range []string{commandSurfaceOption, "commandSurface"} {
		raw, ok := params[key]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return Options{}, protocolcli.InvalidConfigf("option %s is %s, want the path of a file of the project, relative to it", commandSurfaceOption, raw)
		}
		return Options{CommandSurface: value}, nil
	}
	return Options{}, nil
}

// surfaceDocumentPath returns the slash path, relative to the project, of the
// document an option names: a file of the project, never the project
// directory, an absolute path or a path out of it.
func surfaceDocumentPath(option string) (string, error) {
	rel := path.Clean(filepath.ToSlash(option))
	if !filepath.IsLocal(option) || rel == "." {
		return "", protocolcli.InvalidConfigf("option %s is %q, want the path of a file of the project, relative to it", commandSurfaceOption, option)
	}
	return rel, nil
}

// readSurfaceInTree reads and validates the command-surface document at rel
// in the working tree of the project. The document must be a regular file
// reached through directories, which is what git stores and what the reading
// at a tag finds: a symbolic link on the way is refused rather than followed.
func readSurfaceInTree(projectDir, rel string) (*protocolcli.CommandSurface, error) {
	root, err := os.OpenRoot(projectDir)
	if err != nil {
		return nil, fmt.Errorf("open the project %s: %w", projectDir, err)
	}
	defer func() { _ = root.Close() }()

	parts := strings.Split(rel, "/")
	for i := range parts {
		info, err := root.Lstat(filepath.Join(parts[:i+1]...))
		if errors.Is(err, fs.ErrNotExist) {
			return nil, protocolcli.InvalidConfigf("the command surface %s that option %s names does not exist in the project", rel, commandSurfaceOption)
		}
		if err != nil {
			return nil, fmt.Errorf("read the command surface %s: %w", rel, err)
		}
		last := i == len(parts)-1
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, protocolcli.InvalidConfigf("the command surface %s goes through the symbolic link %s; name the file itself", rel, path.Join(parts[:i+1]...))
		}
		if (last && !info.Mode().IsRegular()) || (!last && !info.IsDir()) {
			return nil, protocolcli.InvalidConfigf("the command surface %s is not a regular file", rel)
		}
	}

	file, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, fmt.Errorf("read the command surface %s: %w", rel, err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, protocolcli.CommandSurfaceMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read the command surface %s: %w", rel, err)
	}
	surface, err := protocolcli.ParseCommandSurface(data)
	if err != nil {
		return nil, protocolcli.InvalidConfigf("the command surface %s is invalid: %w", rel, err)
	}
	return surface, nil
}

// readSurfaceAtTag reads the command-surface document at rel, relative to
// dir, as tag holds it. found is false when the tag has no entry at that
// path. An entry that is not a regular file, a document over the size bound
// and a document that does not parse are errors: the released document is
// what the current one is held to.
func readSurfaceAtTag(dir, tag, rel string) (surface *protocolcli.CommandSurface, found bool, err error) {
	env := append(os.Environ(), "GIT_LITERAL_PATHSPECS=1")
	listing, stderr, err := gitCapture(dir, env, nil, "ls-tree", "-l", "-z", "refs/tags/"+tag, "--", rel)
	if err != nil {
		return nil, false, fmt.Errorf("git ls-tree (in %s): %w: %s", dir, err, strings.TrimSpace(stderr))
	}
	for entry := range strings.SplitSeq(listing, "\x00") {
		meta, name, ok := strings.Cut(entry, "\t")
		if !ok || name != rel {
			continue
		}
		// A long listing is "<mode> <type> <object> <size>".
		fields := strings.Fields(meta)
		if len(fields) != 4 || fields[1] != "blob" || fields[0] == "120000" {
			return nil, false, fmt.Errorf("the command surface %s at %s is not a regular file", rel, tag)
		}
		size, err := strconv.Atoi(fields[3])
		if err != nil {
			return nil, false, fmt.Errorf("git ls-tree: unexpected size in %q", meta)
		}
		if size > protocolcli.CommandSurfaceMaxBytes {
			return nil, false, fmt.Errorf("the command surface %s at %s is %d bytes, over the limit of %d", rel, tag, size, protocolcli.CommandSurfaceMaxBytes)
		}
		contents, err := readBlobs(dir, []string{fields[2]})
		if err != nil {
			return nil, false, err
		}
		surface, err := protocolcli.ParseCommandSurface(contents[0])
		if err != nil {
			return nil, false, fmt.Errorf("the command surface %s at %s is invalid: %w", rel, tag, err)
		}
		return surface, true, nil
	}
	return nil, false, nil
}
