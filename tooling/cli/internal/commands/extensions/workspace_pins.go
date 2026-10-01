package extensions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// Workspace pins.
//
// A workspace can pin an artifact to one release in putnami.workspace.json
// ("@putnami/go": "1.2.3", or the template entry "@scope/tpl:1.2.3"). An update
// that moves a pinned artifact to another release writes that release to the
// pin as well as to the lock.
//
// Only an exact release is a pin. A range ("^1.0.0"), a channel ("canary"),
// "latest", an unpinned entry ("" or a list entry) and a workspace-local path
// are never rewritten. When a name is declared more than once, only the
// declaration the config reader keeps is rewritten. Only the workspace
// file is read and written: a pin the global putnami config declares is
// reported, not edited.
//
// The rewrite replaces the bytes of each moved string literal and nothing
// else, so the rest of the file keeps its formatting.

// isExactPin reports whether constraint names one release rather than a range,
// a channel or "latest".
func isExactPin(constraint string) bool {
	_, err := extension.ParseVersion(constraint)
	return err == nil
}

// sameRelease reports whether two exact releases name the same version, so
// "v1.2.3" and "1.2.3", or "1.2" and "1.2.0", are not a move.
func sameRelease(a, b string) bool {
	va, errA := extension.ParseVersion(a)
	vb, errB := extension.ParseVersion(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return va.String() == vb.String()
}

// movesPin reports whether resolving to version moves the exact release pin.
// The "0.0.0" a registry answer without a version falls back to is not a
// release, so it never replaces a pin.
func movesPin(pin, version string) bool {
	return isExactPin(pin) && version != "0.0.0" && isExactPin(version) && !sameRelease(pin, version)
}

// movesWorkspacePin reports whether resolving name to version moves a pin the
// workspace file declares for name. declared is the constraint the config
// reader keeps for name after merging the global putnami config: the pin
// moves only when that constraint is itself an exact release version moves
// away from.
func movesWorkspacePin(pins map[string]string, name, declared, version string) bool {
	_, pinned := pins[name]
	return pinned && movesPin(declared, version)
}

// workspacePin is the exact release the workspace file pins an artifact to,
// and the string literal that declares it.
type workspacePin struct {
	release string
	literal memberValue
}

// extensionWorkspacePins returns the exact release each extension pins in the
// effective `extensions` object of the workspace config. The last member for
// a name declares it. The list form and local paths pin nothing.
func extensionWorkspacePins(config []byte) (map[string]workspacePin, error) {
	values, isArray, err := effectiveMember(config, "extensions")
	if err != nil || isArray {
		return nil, err
	}
	declared := map[string]memberValue{}
	for _, v := range values {
		declared[v.key] = v
	}
	pins := map[string]workspacePin{}
	for name, v := range declared {
		if v.isString && !isLocalExtensionRef(name) && isExactPin(v.value) {
			pins[name] = workspacePin{release: v.value, literal: v}
		}
	}
	return pins, nil
}

// extensionPinLiteral is the `extensions` member value that pins version.
func extensionPinLiteral(_, version string) string {
	return version
}

// templateWorkspacePins returns the exact release each "name:constraint" entry
// of the effective `templates` array pins. An entry identical to an earlier
// one is dropped, then the last entry for a name declares it.
func templateWorkspacePins(config []byte) (map[string]workspacePin, error) {
	values, isArray, err := effectiveMember(config, "templates")
	if err != nil || !isArray {
		return nil, err
	}
	seen := map[string]bool{}
	declared := map[string]memberValue{}
	for _, v := range values {
		if !v.isString || seen[v.value] {
			continue
		}
		seen[v.value] = true
		name, _, _ := strings.Cut(v.value, ":")
		declared[name] = v
	}
	pins := map[string]workspacePin{}
	for name, v := range declared {
		if _, release, found := strings.Cut(v.value, ":"); found && isExactPin(release) {
			pins[name] = workspacePin{release: release, literal: v}
		}
	}
	return pins, nil
}

// templatePinLiteral is the `templates` entry that pins name to version.
func templatePinLiteral(name, version string) string {
	return name + ":" + version
}

// memberValue is one value directly under a top-level member of a JSON object
// document: its key ("" for an array element), its string content when it is
// a string, and the byte span of its literal.
type memberValue struct {
	key, value string
	isString   bool
	start, end int
}

// effectiveMember returns the values directly under the effective top-level
// member of a JSON document, in document order, and whether that
// member is an array. As when the config is decoded, the member name matches
// without regard to case and the last matching member is effective. A member
// that is neither an object nor an array has no values.
func effectiveMember(config []byte, member string) ([]memberValue, bool, error) {
	parseErr := func(err error) error {
		return fmt.Errorf("parse %s: %w", wsproto.WorkspaceConfigFilename, err)
	}
	dec := json.NewDecoder(bytes.NewReader(config))
	tok, err := dec.Token()
	if err != nil {
		return nil, false, parseErr(err)
	}
	if tok != json.Delim('{') {
		// A document that is not an object (null) declares nothing.
		return nil, false, nil
	}
	var values []memberValue
	isArray := false
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false, parseErr(err)
		}
		if key, _ := tok.(string); !strings.EqualFold(key, member) {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, false, parseErr(err)
			}
			continue
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, false, parseErr(err)
		}
		values, isArray, err = containerValues(raw, int(dec.InputOffset())-len(raw))
		if err != nil {
			return nil, false, parseErr(err)
		}
	}
	return values, isArray, nil
}

// containerValues returns the values directly under raw, a JSON object or
// array that starts at offset in its document, with spans in that document.
func containerValues(raw json.RawMessage, offset int) ([]memberValue, bool, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	open, err := dec.Token()
	if err != nil || (open != json.Delim('{') && open != json.Delim('[')) {
		return nil, false, err
	}
	isArray := open == json.Delim('[')
	var values []memberValue
	for dec.More() {
		key := ""
		if !isArray {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, false, err
			}
			key, _ = keyTok.(string)
		}
		// A json.RawMessage holds the value's exact bytes, which end at the
		// decoder's offset.
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, false, err
		}
		end := offset + int(dec.InputOffset())
		v := memberValue{key: key, start: end - len(value), end: end}
		v.isString = json.Unmarshal(value, &v.value) == nil
		values = append(values, v)
	}
	return values, isArray, nil
}

// jsonStringLiteral encodes s without escaping '&', '<' or '>', as the rest of
// the CLI writes its config files.
func jsonStringLiteral(s string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// workspacePinsOf returns the pins ops' kind declares in config. A file the
// config reader cannot decode is ignored when the config is read, so it pins
// nothing.
func workspacePinsOf(config []byte, ops artifactOps) (map[string]workspacePin, error) {
	if ops.workspacePins == nil || json.Unmarshal(config, &wsproto.Config{}) != nil {
		return nil, nil
	}
	return ops.workspacePins(config)
}

// rewritePins returns config with each moved release written to the literal
// that pins it. Every other byte is kept.
func rewritePins(config []byte, ops artifactOps, moved map[string]string) ([]byte, error) {
	pins, err := workspacePinsOf(config, ops)
	if err != nil {
		return nil, err
	}
	type edit struct {
		start, end int
		literal    []byte
	}
	var edits []edit
	for name, version := range moved {
		pin, pinned := pins[name]
		if !pinned {
			continue
		}
		literal, err := jsonStringLiteral(ops.pinLiteral(name, version))
		if err != nil {
			return nil, err
		}
		edits = append(edits, edit{start: pin.literal.start, end: pin.literal.end, literal: literal})
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var out bytes.Buffer
	last := 0
	for _, e := range edits {
		out.Write(config[last:e.start])
		out.Write(e.literal)
		last = e.end
	}
	out.Write(config[last:])
	return out.Bytes(), nil
}

// readWorkspacePins returns the exact release each name of ops' kind pins in
// the workspace file. A workspace without the file, or a kind that keeps no
// pin there, pins nothing.
func readWorkspacePins(wsRoot string, ops artifactOps) (map[string]string, error) {
	if ops.workspacePins == nil {
		return nil, nil
	}
	data, err := os.ReadFile(filepath.Join(wsRoot, wsproto.WorkspaceConfigFilename))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", wsproto.WorkspaceConfigFilename, err)
	}
	pins, err := workspacePinsOf(data, ops)
	if err != nil {
		return nil, err
	}
	releases := make(map[string]string, len(pins))
	for name, pin := range pins {
		releases[name] = pin.release
	}
	return releases, nil
}

// renderWorkspacePins returns the workspace file with the moved releases
// written to their pins, or nil when nothing moved. It writes nothing.
func renderWorkspacePins(wsRoot string, ops artifactOps, moved map[string]string) ([]byte, error) {
	if len(moved) == 0 || ops.workspacePins == nil {
		return nil, nil
	}
	original, err := os.ReadFile(filepath.Join(wsRoot, wsproto.WorkspaceConfigFilename))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", wsproto.WorkspaceConfigFilename, err)
	}
	rendered, err := rewritePins(original, ops, moved)
	if err != nil {
		return nil, err
	}
	if err := confirmWorkspacePins(rendered, ops, moved); err != nil {
		return nil, err
	}
	return rendered, nil
}

// confirmWorkspacePins reads the rendered workspace file with the global
// putnami config, as the config reader does, and fails unless each moved name
// now declares its new release. A rewrite that duplicates another declaration
// can leave another constraint effective.
func confirmWorkspacePins(rendered []byte, ops artifactOps, moved map[string]string) error {
	var workspace wsproto.Config
	if err := json.Unmarshal(rendered, &workspace); err != nil {
		return fmt.Errorf("parse %s: %w", wsproto.WorkspaceConfigFilename, err)
	}
	merged := wsproto.Load("")
	wsproto.MergeWorkspaceConfig(merged, &workspace)
	declared := ops.configMap(merged)
	var stale []string
	for _, name := range sortedPinNames(moved) {
		if !sameRelease(declared[name], moved[name]) {
			stale = append(stale, fmt.Sprintf("%s would declare %s, not %s", name, declared[name], moved[name]))
		}
	}
	if len(stale) > 0 {
		return fmt.Errorf("these %s are declared more than once across %s and the global putnami config, so their pins cannot move: %s; keep one declaration of each in those two files and rerun the update",
			ops.plural, wsproto.WorkspaceConfigFilename, strings.Join(stale, ", "))
	}
	return nil
}

// writeWorkspacePins publishes a rendered workspace file atomically. A failure
// lists each pin to set by hand, old and new release.
func writeWorkspacePins(wsRoot string, rendered []byte, replaced, moved map[string]string) error {
	if rendered == nil {
		return nil
	}
	if err := shared.AtomicWriteFile(filepath.Join(wsRoot, wsproto.WorkspaceConfigFilename), rendered); err != nil {
		changes := make([]string, 0, len(moved))
		for _, name := range sortedPinNames(moved) {
			changes = append(changes, fmt.Sprintf("%s %s → %s", name, replaced[name], moved[name]))
		}
		return fmt.Errorf("write %s: %w; %s already records the new releases, so set these pins in %s by hand: %s",
			wsproto.WorkspaceConfigFilename, err, lockfile.LockFilename, wsproto.WorkspaceConfigFilename, strings.Join(changes, ", "))
	}
	return nil
}

// sortedPinNames returns the names of moved in a stable order for reporting.
func sortedPinNames(moved map[string]string) []string {
	names := make([]string, 0, len(moved))
	for name := range moved {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
