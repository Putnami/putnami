package stacks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
)

// DeclareExtensions adds to the workspace config's extensions each of the
// extensions it does not declare yet, in the list or object form
// the config uses, and reports whether it changed the file; in a dry run it
// answers the change instead of writing it. It never removes one: a workspace
// may run an extension for its own projects.
func DeclareExtensions(root string, extensions []string, dry bool) (bool, *FileChange, error) {
	if len(extensions) == 0 {
		return false, nil, nil
	}
	file := filepath.Join(root, wsproto.WorkspaceConfigFilename)
	data, err := os.ReadFile(file) //nolint:gosec // the workspace config under the root
	if errors.Is(err, os.ErrNotExist) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	keys, members, err := objectMembers(data)
	if err != nil {
		return false, nil, err
	}
	var declared []string
	raw, isSet := members["extensions"]
	object := false
	if isSet {
		if err := json.Unmarshal(raw, &declared); err != nil {
			names, _, objErr := objectMembers(raw)
			if objErr != nil {
				return false, nil, errors.New("extensions is neither a list of names nor an object of name to version")
			}
			declared, object = names, true
		}
	}
	names := declaredNames(root, declared)
	missing := slices.DeleteFunc(slices.Clone(extensions), func(e string) bool { return slices.Contains(names, e) })
	if len(missing) == 0 {
		return false, nil, nil
	}
	var value []byte
	if object {
		value, err = appendMembers(raw, missing)
	} else {
		value, err = json.Marshal(append(declared, missing...))
	}
	if err != nil {
		return false, nil, err
	}
	if !isSet {
		keys = append(keys, "extensions")
	}
	members["extensions"] = value
	rendered, err := renderObject(keys, members)
	if err != nil {
		return false, nil, err
	}
	after, err := wsproto.RenderConfigFile(json.RawMessage(rendered), data)
	if err != nil {
		return false, nil, err
	}
	if dry {
		return true, &FileChange{File: wsproto.WorkspaceConfigFilename, Before: data, After: after}, nil
	}
	return true, nil, replaceKeepingMode(file, after)
}

// objectMembers reads a JSON object's keys, in order, and their raw values.
func objectMembers(data []byte) ([]string, map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, nil, errors.New("not a JSON object")
	}
	var keys []string
	members := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, nil, err
		}
		if _, seen := members[key]; !seen {
			keys = append(keys, key)
		}
		members[key] = raw
	}
	return keys, members, nil
}

// appendMembers adds to an object of extension name to version constraint a
// member with no constraint for each name, keeping the others in order.
func appendMembers(object json.RawMessage, names []string) ([]byte, error) {
	keys, members, err := objectMembers(object)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		keys = append(keys, name)
		members[name] = json.RawMessage(`""`)
	}
	out, err := renderObject(keys, members)
	return bytes.TrimSpace(out), err
}

// renderObject writes the members in the order of keys, two-space indented,
// with a final newline.
func renderObject(keys []string, members map[string]json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, key := range keys {
		name, _ := json.Marshal(key)
		var value bytes.Buffer
		if err := json.Indent(&value, members[key], "  ", "  "); err != nil {
			return nil, err
		}
		fmt.Fprintf(&buf, "  %s: %s", name, value.Bytes())
		if i < len(keys)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString("}\n")
	return buf.Bytes(), nil
}

// replaceKeepingMode writes data at file through a temporary file beside it, so a
// reader never sees half a file, keeping the file's permissions.
func replaceKeepingMode(file string, data []byte) error {
	info, err := os.Stat(file)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), "."+filepath.Base(file)+".*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp.Name(), info.Mode().Perm())
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), file)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
	}
	return werr
}

// declaredNames are the names of the declared extensions: an entry that names
// a workspace directory, such as /tools/ts-extension, by the name its
// manifest gives, and any other entry as it is written.
func declaredNames(root string, declared []string) []string {
	names := slices.Clone(declared)
	for _, entry := range declared {
		rel, ok := strings.CutPrefix(entry, "/")
		if !ok {
			rel, ok = strings.CutPrefix(entry, "./")
		}
		if !ok {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel), "putnami.extension.json")) //nolint:gosec // a manifest under the workspace root
		if err != nil {
			continue
		}
		var manifest struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &manifest) == nil && manifest.Name != "" {
			names = append(names, manifest.Name)
		}
	}
	return names
}
