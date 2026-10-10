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
	"slices"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
)

// commandSurfaceOption is the validate command's flag, and project option,
// that names a project's command-surface document.
const commandSurfaceOption = "command-surface"

// commandSurfaceAlias is the camelCase spelling of the option, which the CLI
// also delivers and a project may write.
const commandSurfaceAlias = "commandSurface"

// optionLayers are the keys of a project's putnami.json options that reach
// the validate task of this extension, lowest precedence first: the command,
// the extension, and the extension's command. It is the project part of the
// CLI's parameter merge (tooling/cli/internal/jobs mergeParamLayers).
var optionLayers = []string{"validate", "@putnami/go", "@putnami/go:validate"}

// projectConfigMaxBytes bounds the putnami.json the check reads at a tag.
const projectConfigMaxBytes = 1 << 20

// Options are a project's options for one check.
type Options struct {
	// CommandSurface is the path of the project's command-surface document
	// (go.putnami.dev/protocol/cli CommandSurface), relative to the project
	// directory; empty when the project declares none. When it is set, the
	// check also compares the document with the one at the line's last tag.
	CommandSurface string
}

// optionsFromParams reads a check's options from the task parameters: the
// validate command's flags over the project's and the workspace's options, as
// the CLI merged them, read by surfaceOptionIn. A value that is not a string is
// invalid configuration; null is no value.
func optionsFromParams(params pctx.Params) (Options, error) {
	raw, ok := surfaceOptionIn(params)
	if !ok {
		return Options{}, nil
	}
	value, err := surfaceOptionPath(raw)
	if err != nil {
		return Options{}, protocolcli.InvalidConfigf("option %s is %s, want the path of a file of the project, relative to it", commandSurfaceOption, raw)
	}
	return Options{CommandSurface: value}, nil
}

// surfaceOptionIn returns the raw value of the command-surface option in one
// set of parameters: the camelCase key's value when it is set, else the
// kebab-case key's. It is the one rule both sides of the check read the option
// by. The working tree's side applies it to the parameters the CLI merged, and
// the tag's side to each option layer, which surfaceOptionOfLayers then merges
// as the CLI does. The rule follows the CLI's: it writes every kebab-case key
// of a layer also under its camelCase alias, unless the layer sets that alias
// itself (tooling/cli/internal/jobs projectParamAliases), so the merged
// camelCase key holds the winning layer's value whatever spelling each layer
// used.
func surfaceOptionIn(params map[string]json.RawMessage) (json.RawMessage, bool) {
	if raw, ok := params[commandSurfaceAlias]; ok {
		return raw, true
	}
	raw, ok := params[commandSurfaceOption]
	return raw, ok
}

// surfaceOptionOfLayers returns the raw value of the command-surface option a
// project's option layers give, merged as the CLI merges them
// (tooling/cli/internal/jobs mergeParamLayers): a layer later in optionLayers
// replaces what an earlier one gave.
func surfaceOptionOfLayers(options map[string]map[string]json.RawMessage) (raw json.RawMessage, ok bool) {
	for _, layer := range optionLayers {
		if value, set := surfaceOptionIn(options[layer]); set {
			raw, ok = value, true
		}
	}
	return raw, ok
}

// surfaceOptionPath decodes the option's value: a path, or null for none.
func surfaceOptionPath(raw json.RawMessage) (string, error) {
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	if value == nil {
		return "", nil
	}
	return *value, nil
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

// unkeyedDirectories are the directory names the CLI's file-pattern walk never
// enters (tooling/cli/internal/store globDoubleStar), beside every directory
// whose name starts with a dot, which the task's inputs leave out because a run
// writes its generated files there.
var unkeyedDirectories = []string{"node_modules", "out", "dist", "vendor"}

// surfaceDocumentKeyed refuses a working-tree document the task's cache key
// does not read, which could change and leave a stored verdict in place. The
// key reads the project's .json files outside the directories a dot starts
// and outside unkeyedDirectories (putnami.extension.json, validate-api
// inputs). rel is a surfaceDocumentPath result. The document at the tag needs
// no such rule: the key reads the whole tree the tag holds at the project.
func surfaceDocumentKeyed(rel string) error {
	if path.Ext(rel) != ".json" {
		return protocolcli.InvalidConfigf("option %s is %q, want a .json file: the task's cache key reads the project's .json files",
			commandSurfaceOption, rel)
	}
	dirs := strings.Split(rel, "/")
	for _, dir := range dirs[:len(dirs)-1] {
		if strings.HasPrefix(dir, ".") || slices.Contains(unkeyedDirectories, dir) {
			return protocolcli.InvalidConfigf("option %s is %q, which is inside %s: the task's cache key does not read that directory; move the document out of it",
				commandSurfaceOption, rel, dir)
		}
	}
	return nil
}

// readSurfaceInTree reads and validates the command-surface document at rel
// in the working tree of the project. The document must be a regular file
// reached through directories, which is what git stores and what the reading
// at a tag finds: a symbolic link on the way is refused rather than followed.
// Each name must match its directory entry exactly, case included, because
// git matches it exactly at the tag while a case-insensitive file system would
// find a differently spelled file here.
func readSurfaceInTree(projectDir, rel string) (*protocolcli.CommandSurface, error) {
	root, err := os.OpenRoot(projectDir)
	if err != nil {
		return nil, fmt.Errorf("open the project %s: %w", projectDir, err)
	}
	defer func() { _ = root.Close() }()

	parts := strings.Split(rel, "/")
	for i, part := range parts {
		dir := path.Join(append([]string{"."}, parts[:i]...)...)
		entries, err := fs.ReadDir(root.FS(), dir)
		if err != nil {
			return nil, fmt.Errorf("read the command surface %s: %w", rel, err)
		}
		entry, spelled := directoryEntry(entries, part)
		if entry == nil && spelled != "" {
			return nil, protocolcli.InvalidConfigf("the command surface %s that option %s names differs in case from %s on disk; name it with its exact case",
				rel, commandSurfaceOption, path.Join(append(parts[:i:i], spelled)...))
		}
		if entry == nil {
			return nil, protocolcli.InvalidConfigf("the command surface %s that option %s names does not exist in the project", rel, commandSurfaceOption)
		}
		last := i == len(parts)-1
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil, protocolcli.InvalidConfigf("the command surface %s goes through the symbolic link %s; name the file itself", rel, path.Join(parts[:i+1]...))
		}
		if (last && !entry.Type().IsRegular()) || (!last && !entry.IsDir()) {
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

// directoryEntry returns the entry named exactly name. When there is none, it
// returns the name of an entry that differs from name in case only, if any.
func directoryEntry(entries []fs.DirEntry, name string) (fs.DirEntry, string) {
	spelled := ""
	for _, entry := range entries {
		if entry.Name() == name {
			return entry, ""
		}
		if spelled == "" && strings.EqualFold(entry.Name(), name) {
			spelled = entry.Name()
		}
	}
	return nil, spelled
}

// releasedSurface is what the line's last tag holds for the comparison: the
// document at the path the project declared at that tag, or why there is none
// to compare with.
type releasedSurface struct {
	// path is the slash path, relative to the project, the tag declares.
	path string
	// surface is the released document; nil when nothing is compared.
	surface *protocolcli.CommandSurface
	// note says that the tag declares no document: the project had not
	// released a command surface yet.
	note string
	// warning says why a document the tag declares cannot be compared.
	warning string
}

// readReleasedSurface reads the command surface the project released at tag.
// The path comes from the project's putnami.json at tag, not from the current
// option, so a document that moves in the same change is still compared with
// the released one. A document the tag declares but does not hold, or holds
// in a later protocol version than this reader knows, is a warning: nothing
// can be compared, and no marker could make it comparable. A released
// document that is not a regular file, is over the bound or is malformed is an
// error.
func readReleasedSurface(dir, tag string) (releasedSurface, error) {
	option, warning, err := surfaceOptionAtTag(dir, tag)
	if err != nil || warning != "" {
		return releasedSurface{warning: warning}, err
	}
	if option == "" {
		return releasedSurface{note: fmt.Sprintf("the project declares no command surface at %s: there is no released command surface to compare with", tag)}, nil
	}
	rel, err := surfaceDocumentPath(option)
	if err != nil {
		return releasedSurface{warning: fmt.Sprintf("option %s is %q at %s, not the path of a file of the project, so the command surface is not compared",
			commandSurfaceOption, option, tag)}, nil
	}
	released := releasedSurface{path: rel}
	entry, found, err := entryAtTag(dir, tag, rel)
	if err != nil {
		return releasedSurface{}, err
	}
	if !found {
		released.warning = fmt.Sprintf("the project declares the command surface %s at %s, but the tag does not hold it, so the command surface is not compared", rel, tag)
		return released, nil
	}
	if !entry.regular {
		return releasedSurface{}, fmt.Errorf("the command surface %s at %s is not a regular file", rel, tag)
	}
	if entry.size > protocolcli.CommandSurfaceMaxBytes {
		return releasedSurface{}, fmt.Errorf("the command surface %s at %s is %d bytes, over the limit of %d", rel, tag, entry.size, protocolcli.CommandSurfaceMaxBytes)
	}
	contents, err := readBlobs(dir, []string{entry.object})
	if err != nil {
		return releasedSurface{}, err
	}
	surface, err := protocolcli.ParseCommandSurface(contents[0])
	if errors.Is(err, protocolcli.ErrUnknownCommandSurfaceVersion) {
		released.warning = fmt.Sprintf("the command surface %s at %s is not compared: %v; update @putnami/go to read it", rel, tag, err)
		return released, nil
	}
	if err != nil {
		return releasedSurface{}, fmt.Errorf("the command surface %s at %s is invalid: %w", rel, tag, err)
	}
	released.surface = surface
	return released, nil
}

// droppedSurface answers for a project that declares no command surface now.
// When its putnami.json declared one at tag, a warning says the command
// surface is not compared, so removing the option in the change that breaks
// the surface does not end the comparison without a trace. rel is the slash
// path, relative to the project, that the tag declares, when it is the path of
// a file of the project. A putnami.json at tag that the check cannot read
// gives its warning too, since the check cannot tell what it declared. A tag
// that declares no document gives neither.
func droppedSurface(dir, tag string) (rel, warning string, err error) {
	option, warning, err := surfaceOptionAtTag(dir, tag)
	if err != nil || warning != "" || option == "" {
		return "", warning, err
	}
	declared := fmt.Sprintf("%q", option)
	if clean, invalid := surfaceDocumentPath(option); invalid == nil {
		rel, declared = clean, clean
	}
	return rel, fmt.Sprintf("the project declared the command surface %s at %s and declares none now, so the command surface is not compared; "+
		"declare it with option %s to compare it", declared, tag, commandSurfaceOption), nil
}

// surfaceOptionAtTag returns the command-surface option the project's
// putnami.json declares at tag; empty when it declares none or the tag has no
// putnami.json. A putnami.json the check cannot read gives a warning.
func surfaceOptionAtTag(dir, tag string) (option, warning string, err error) {
	entry, found, err := entryAtTag(dir, tag, wsproto.ConfigFilename)
	if err != nil || !found {
		return "", "", err
	}
	unreadable := func(reason string) (string, string, error) {
		return "", fmt.Sprintf("the project's %s at %s %s, so the command surface is not compared", wsproto.ConfigFilename, tag, reason), nil
	}
	if !entry.regular {
		return unreadable("is not a regular file")
	}
	if entry.size > projectConfigMaxBytes {
		return unreadable(fmt.Sprintf("is %d bytes, over the limit of %d", entry.size, projectConfigMaxBytes))
	}
	contents, err := readBlobs(dir, []string{entry.object})
	if err != nil {
		return "", "", err
	}
	option, err = declaredSurfaceOption(contents[0])
	if err != nil {
		return unreadable(err.Error())
	}
	return option, "", nil
}

// declaredSurfaceOption returns the command-surface option a putnami.json
// declares in the option layers the CLI merges for this task, read by the same
// rule as the working tree's side (surfaceOptionOfLayers); empty when no layer
// sets it or the winning value is null.
func declaredSurfaceOption(data []byte) (string, error) {
	var config struct {
		Options map[string]map[string]json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return "", fmt.Errorf("does not parse (%w)", err)
	}
	raw, ok := surfaceOptionOfLayers(config.Options)
	if !ok {
		return "", nil
	}
	value, err := surfaceOptionPath(raw)
	if err != nil {
		return "", fmt.Errorf("sets option %s to %s, not a path", commandSurfaceOption, raw)
	}
	return value, nil
}

// entryAtTag returns the entry at rel, relative to dir, as tag holds it, with
// its size. It returns false when the tag has no entry at that path.
func entryAtTag(dir, tag, rel string) (treeEntry, bool, error) {
	entries, err := treeAtTag(dir, tag, rel, treeListing{sized: true})
	if err != nil {
		return treeEntry{}, false, err
	}
	for _, entry := range entries {
		if entry.name == rel {
			return entry, true, nil
		}
	}
	return treeEntry{}, false, nil
}
