package agentctx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// Host hooks: the hooks an extension declares (the manifest's `hostHooks`
// member), written into the agent hosts' own hook files (ADR 0081).
//
// Only an explicit `putnami install`, `init` or `upgrade` writes them. An
// extension's hooks are written when the workspace opts into its agent
// content, binds the collaboration contract the hooks require, and has the
// extension installed. When one of the three goes away, the next explicit run
// removes them.
//
// The files are shared with the person who edits them, and no record says
// which entries Putnami wrote. A hook handler is Putnami's when its command
// runs a known launcher followed by the command path an installed extension
// declares. Every other entry, key and group is the person's and keeps its
// text and its place.

// hostHookFile is the hook file of one agent host.
type hostHookFile struct {
	host string
	// rel is the workspace-relative path, in slash form.
	rel string
}

// hostHookFiles lists the hook file of every host, in the order they are
// written.
var hostHookFiles = []hostHookFile{
	{host: extproto.HostClaudeCode, rel: ".claude/settings.json"},
	{host: extproto.HostCodex, rel: ".codex/hooks.json"},
}

// hostHookMCPServer is the server name a host puts in every MCP tool name. A
// hook entry's mcpTools field (a protocol field) is matched under it. Putnami
// registers no MCP server of its own.
const hostHookMCPServer = "putnami"

// The guards a rendered hook command ends with. A host reads exit code 2 from
// a Stop hook as "continue the turn", and from a wake hook as "start a turn".
// The CLI itself exits 2 on a usage or bootstrap error, so no exit code of the
// command may reach the host as is:
//
//   - an ordinary hook always exits 0;
//   - a wake hook exits 2 only when the command exits 75, which no CLI or
//     wrapper failure produces, and 0 otherwise.
const (
	hostHookGuard     = " || true"
	hostHookWakeGuard = `; s=$?; [ "$s" -eq 75 ] && exit 2; exit 0`
)

// hostHookEnvironment starts every hook command: a hook never runs the
// first-use install. That install can outlast the host's timeout, and it
// prints on standard output, which the host reads as the hook's answer.
const hostHookEnvironment = shared.NoAutoInstallEnv + "=1"

// hostHookLaunchers are the launchers a hook command may start with. Each one
// is recognized whatever the workspace uses now, so a hook written with an
// earlier launcher is replaced, not kept beside the new one.
var hostHookLaunchers = []string{
	"putnami",
	"./putnamiw",
	hostHookSourceLauncher[extproto.HostClaudeCode],
	hostHookSourceLauncher[extproto.HostCodex],
}

// hostHookSourceLauncher is the launcher of a source workspace per host. A
// hook may run from any directory of the checkout, so it names the wrapper
// from the workspace root the host provides: Claude Code sets
// CLAUDE_PROJECT_DIR, and Codex runs in the Git checkout.
var hostHookSourceLauncher = map[string]string{
	extproto.HostClaudeCode: `"$CLAUDE_PROJECT_DIR"/putnamiw`,
	extproto.HostCodex:      `"$(git rev-parse --show-toplevel)"/putnamiw`,
}

// HostHookAction says what RegisterHostHooks did to a hook file.
type HostHookAction string

const (
	// HostHookFileCreated is a hook file that did not exist.
	HostHookFileCreated HostHookAction = "created"
	// HostHookFileUpdated is a hook file whose hooks changed.
	HostHookFileUpdated HostHookAction = "updated"
	// HostHookFileRemoved is a hook file that held nothing but the removed
	// hooks, and was deleted.
	HostHookFileRemoved HostHookAction = "removed"
)

// HostHookChange is one hook file a run changed, or would change.
type HostHookChange struct {
	// Host is the agent host the file belongs to.
	Host string
	// File is the workspace-relative path, in slash form.
	File string
	// Action is what the run did to the file.
	Action HostHookAction
}

// Description says what changed, and for Codex, the step that makes the
// change take effect: Codex runs a new or changed hook only after the person
// reviews and trusts it with /hooks.
func (c HostHookChange) Description() string {
	text := fmt.Sprintf("Host hooks %s in %s", c.Action, c.File)
	if c.Host == extproto.HostCodex && c.Action != HostHookFileRemoved {
		text += "; in Codex, run /hooks to review and trust them"
	}
	return text
}

// RegisterHostHooks writes the host hooks the workspace's extensions declare
// into .claude/settings.json and .codex/hooks.json, and removes the ones that
// no longer apply. It plans every file before it writes, writes each changed
// file once, and returns what it changed.
//
// A file that cannot be read, parsed or edited safely is left untouched and
// reported on warn, as is an extension whose declaration this CLI cannot use;
// the calling command carries on. A nil warn discards the warnings.
//
// Only the explicit install, init and upgrade call it. The implicit bootstrap,
// the first-run pass and context generate never do.
func RegisterHostHooks(wsRoot string, cfg *wsproto.Config, warn io.Writer) []HostHookChange {
	return applyHostHooks(wsRoot, cfg, warn, true)
}

// PlanHostHooks is RegisterHostHooks without the writes, for a dry run.
func PlanHostHooks(wsRoot string, cfg *wsproto.Config, warn io.Writer) []HostHookChange {
	return applyHostHooks(wsRoot, cfg, warn, false)
}

func applyHostHooks(wsRoot string, cfg *wsproto.Config, warn io.Writer, write bool) []HostHookChange {
	warnf := func(format string, args ...any) {
		if warn != nil {
			iox.Fprintf(warn, "putnami: warning: "+format+"\n", args...)
		}
	}
	set, err := collectHostHooks(wsRoot, cfg, warnf)
	if err != nil {
		warnf("host hooks are not written: %v", err)
		return nil
	}
	if len(set.paths) == 0 {
		// No installed extension declares hooks, so no entry can be ours:
		// the files are not even read.
		return nil
	}
	var changes []HostHookChange
	for _, file := range hostHookFiles {
		plan, err := planHostHookFile(wsRoot, file, set)
		if err != nil {
			// A file with nothing to add is skipped without a word: a stale
			// entry in it cannot be found, and the host cannot read it either.
			if len(set.desired[file.host]) > 0 {
				warnf("host hooks are not written to %s: %v. Fix the file, then run `putnami install`.", file.rel, err)
			}
			continue
		}
		if plan == nil {
			continue
		}
		if write {
			if err := plan.apply(); err != nil {
				warnf("host hooks are not written to %s: %v", file.rel, err)
				continue
			}
		}
		changes = append(changes, HostHookChange{Host: file.host, File: file.rel, Action: plan.action})
	}
	return changes
}

// hostHookSet is what the workspace's extensions ask of the hook files.
type hostHookSet struct {
	// paths are the command paths of every installed extension that declares
	// hooks, active or not. A handler that runs one of them is Putnami's.
	paths [][]string
	// desired maps a host and an event to the groups the active extensions
	// declare there, in extension name order, then declared order.
	desired map[string]map[string][]hostHookGroupJSON
}

// collectHostHooks reads the declarations of the workspace's extensions and
// decides which are active. It fails only when the workspace document cannot
// be read: without it, no binding can be confirmed or ruled out, and removing
// hooks on a guess would be wrong.
func collectHostHooks(wsRoot string, cfg *wsproto.Config, warnf func(string, ...any)) (hostHookSet, error) {
	set := hostHookSet{desired: map[string]map[string][]hostHookGroupJSON{}}
	if cfg == nil {
		return set, nil
	}
	optedIn := map[string]bool{}
	for _, declared := range cfg.AgentArtifacts {
		if name := AgentArtifactDeclarationName(declared); name != "" {
			optedIn[name] = true
		}
	}
	candidates := extension.DeclaredExtensionNames(wsRoot, cfg)
	for name := range optedIn {
		if !slices.Contains(candidates, name) {
			candidates = append(candidates, name)
		}
	}
	sort.Strings(candidates)

	var sources []*extension.HostHooksSource
	for _, name := range candidates {
		source, err := extension.LocateHostHooks(wsRoot, cfg, name)
		switch {
		case err == nil:
			sources = append(sources, source)
		case errors.Is(err, extension.ErrNoHostHooks):
		case optedIn[name]:
			// An extension whose hooks cannot be read keeps the entries it
			// has: nothing here can tell them apart from the person's.
			warnf("the host hooks of %s are not written: %v", name, err)
		}
	}
	if len(sources) == 0 {
		return set, nil
	}

	bound, err := boundCollaborationContracts(wsRoot)
	if err != nil {
		return hostHookSet{}, err
	}
	base, err := workspaceLauncher(wsRoot)
	if err != nil {
		return hostHookSet{}, err
	}
	for _, source := range sources {
		declaration := source.Declaration
		set.paths = append(set.paths, declaration.Command)
		if !optedIn[source.Name] || !bound[declaration.Requires.Contract] {
			continue
		}
		for _, file := range hostHookFiles {
			for _, entry := range declaration.Hosts[file.host] {
				events := set.desired[file.host]
				if events == nil {
					events = map[string][]hostHookGroupJSON{}
					set.desired[file.host] = events
				}
				events[entry.Event] = append(events[entry.Event], renderHostHookGroup(file.host, base, declaration.Command, entry))
			}
		}
	}
	return set, nil
}

// boundCollaborationContracts returns the contracts the workspace document
// (putnami.workspace.json) binds validly. A block that repeats options or
// options.collaboration binds nothing, as for the collaboration routing. A contract this
// CLI does not know is not bound.
func boundCollaborationContracts(wsRoot string) (map[string]bool, error) {
	data, err := os.ReadFile(wsproto.ResolveFile(wsRoot, wsproto.WorkspaceConfigFilename))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", wsproto.WorkspaceConfigFilename, err)
	}
	root, err := decodeJSONObject(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", wsproto.WorkspaceConfigFilename, err)
	}
	options, ok := singleJSONMember(root, "options")
	if !ok {
		return nil, nil
	}
	members, err := decodeJSONObject(options)
	if err != nil {
		return nil, nil
	}
	block, ok := singleJSONMember(members, collab.BindingOptionNamespace)
	if !ok {
		return nil, nil
	}
	bindings, _ := collab.ParseBindings(block)
	bound := make(map[string]bool, len(bindings))
	for contract := range bindings {
		bound[contract] = true
	}
	return bound, nil
}

// hostHookGroupJSON is one matcher group of a hook file, as Putnami writes it.
type hostHookGroupJSON struct {
	Matcher string                `json:"matcher,omitempty"`
	Hooks   []hostHookHandlerJSON `json:"hooks"`
}

// hostHookHandlerJSON is one command handler of a matcher group.
type hostHookHandlerJSON struct {
	Type        string `json:"type"`
	Command     string `json:"command"`
	Timeout     int    `json:"timeout"`
	AsyncRewake bool   `json:"asyncRewake,omitempty"`
}

// renderHostHookGroup renders one declared hook for host, started with the
// workspace's launcher (base, from workspaceLauncher), with the first-use
// install turned off.
func renderHostHookGroup(host, base string, path []string, entry extproto.HostHookEntry) hostHookGroupJSON {
	words := append([]string{hostHookEnvironment, hostHookLauncher(host, base)}, path...)
	words = append(words, entry.Args...)
	command := strings.Join(words, " ")
	if entry.Wake {
		command += hostHookWakeGuard
	} else {
		command += hostHookGuard
	}
	return hostHookGroupJSON{
		Matcher: hostHookMatcher(host, entry),
		Hooks: []hostHookHandlerJSON{{
			Type:        "command",
			Command:     command,
			Timeout:     entry.TimeoutSeconds,
			AsyncRewake: entry.Wake,
		}},
	}
}

// hostHookLauncher is the launcher a hook starts with: the bare putnami, or in
// a source workspace, the wrapper named from the root the host provides.
func hostHookLauncher(host, base string) string {
	if base == "./putnamiw" {
		return hostHookSourceLauncher[host]
	}
	return base
}

// hostHookMatcher renders the tools a PostToolUse hook runs after. Claude Code
// reads `A|B`. Codex reads a regular expression and matches a substring, so
// its matcher is anchored: `^(A|B)$`. A Putnami MCP tool is named as the host
// names it: mcp__<server>__<tool>, with the dot of the tool name turned into
// an underscore.
func hostHookMatcher(host string, entry extproto.HostHookEntry) string {
	names := slices.Clone(entry.Tools)
	for _, tool := range entry.MCPTools {
		names = append(names, "mcp__"+hostHookMCPServer+"__"+strings.ReplaceAll(tool, ".", "_"))
	}
	if len(names) == 0 {
		return ""
	}
	joined := strings.Join(names, "|")
	if host == extproto.HostCodex {
		return "^(" + joined + ")$"
	}
	return joined
}

// ownsHostHookCommand reports whether a handler command runs one of paths
// through a known launcher: the launcher and the path, word for word, as a
// command the shell runs (see runsShellCommand).
func ownsHostHookCommand(command string, paths [][]string) bool {
	for _, path := range paths {
		tail := " " + strings.Join(path, " ")
		for _, launcher := range hostHookLaunchers {
			if runsShellCommand(command, launcher+tail) {
				return true
			}
		}
	}
	return false
}

// shellAssignment is one NAME=value word before a command, with a value
// without quotes, blanks or shell operators.
var shellAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=[^\s"'$;&|()<>]*$`)

// shellCommandStarts are the reserved words a command may follow.
var shellCommandStarts = []string{"!", "{", "do", "elif", "else", "if", "then", "until", "while"}

// runsShellCommand reports whether needle occurs in command as the words a
// command starts with. The text after needle ends a word: the end of the
// command, a blank or a shell operator. The text before it, without the
// NAME=value assignments that lead the command, is empty or ends with a shell
// operator or a reserved word, so needle is the command, never an argument
// such as the text an echo prints.
func runsShellCommand(command, needle string) bool {
	for offset := 0; offset <= len(command)-len(needle); {
		index := strings.Index(command[offset:], needle)
		if index < 0 {
			return false
		}
		start := offset + index
		end := start + len(needle)
		after := end == len(command) || strings.ContainsRune(" \t\n;&|)", rune(command[end]))
		if after && startsShellCommand(command[:start]) {
			return true
		}
		offset = start + 1
	}
	return false
}

// startsShellCommand reports whether a command may start right after prefix:
// prefix, without its trailing NAME=value words, is blank or ends with a shell
// operator or a reserved word.
func startsShellCommand(prefix string) bool {
	for {
		trimmed := strings.TrimRight(prefix, " \t")
		if trimmed == "" {
			return true
		}
		if strings.ContainsRune("\n;&|(", rune(trimmed[len(trimmed)-1])) {
			return true
		}
		if trimmed == prefix {
			return false
		}
		cut := strings.LastIndexAny(trimmed, " \t\n;&|(")
		word := trimmed[cut+1:]
		if slices.Contains(shellCommandStarts, word) {
			return true
		}
		if !shellAssignment.MatchString(word) {
			return false
		}
		prefix = trimmed[:cut+1]
	}
}

// hostHookFilePlan is the change a run makes to one hook file.
type hostHookFilePlan struct {
	path   string
	action HostHookAction
	// data is the new content; nil with HostHookFileRemoved.
	data []byte
}

func (p *hostHookFilePlan) apply() error {
	if p.action == HostHookFileRemoved {
		return os.Remove(p.path)
	}
	return shared.AtomicWriteFile(p.path, p.data)
}

// planHostHookFile plans the change to one host's file. It returns nil when
// the file already says what the workspace asks.
func planHostHookFile(wsRoot string, file hostHookFile, set hostHookSet) (*hostHookFilePlan, error) {
	path := filepath.Join(wsRoot, filepath.FromSlash(file.rel))
	var data []byte
	exists := false
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	case !info.Mode().IsRegular():
		// A link may point at a file another checkout shares: writing
		// through it, or replacing it, is not this run's call.
		return nil, errors.New("it is not a regular file")
	default:
		exists = true
		if data, err = os.ReadFile(path); err != nil {
			return nil, err
		}
	}
	owns := func(command string) bool { return ownsHostHookCommand(command, set.paths) }
	out, remove, changed, err := mergeHostHookFile(data, exists, file.host, owns, set.desired[file.host])
	if err != nil || !changed {
		return nil, err
	}
	switch {
	case remove:
		return &hostHookFilePlan{path: path, action: HostHookFileRemoved}, nil
	case !exists:
		return &hostHookFilePlan{path: path, action: HostHookFileCreated, data: out}, nil
	default:
		return &hostHookFilePlan{path: path, action: HostHookFileUpdated, data: out}, nil
	}
}

// errHostHookFileUnparseable marks a hook file that is not JSON.
var errHostHookFileUnparseable = errors.New("it is not valid JSON")

// mergeHostHookFile replaces the handlers owns accepts in a hook file with the
// desired groups, event by event, for the events host runs.
//
// The new groups take the places of the ones they replace, in order. A group
// that also holds a handler of the person keeps it, and the new group follows
// it. New groups beyond the places available follow the event's other groups;
// places beyond the new groups are dropped. An event, the hooks object or the
// file that only the removed handlers filled is removed with them.
//
// Every value the merge does not edit keeps its exact bytes. Edited values are
// written with the indentation the file already uses, two spaces by default,
// so a file in the format the hosts write changes only where the hooks do.
func mergeHostHookFile(data []byte, exists bool, host string, owns func(string) bool, desired map[string][]hostHookGroupJSON) (out []byte, remove, changed bool, err error) {
	unit := "  "
	trailing := "\n"
	var root []jsonMember
	if exists {
		if !json.Valid(data) {
			return nil, false, false, errHostHookFileUnparseable
		}
		if root, err = decodeJSONObject(data); err != nil {
			return nil, false, false, errors.New("it is not a JSON object")
		}
		unit = jsonIndentUnit(data)
		if !bytes.HasSuffix(data, []byte("\n")) {
			trailing = ""
		}
	}

	hooksIndex, count := jsonMemberIndex(root, "hooks")
	if count > 1 {
		return nil, false, false, errors.New("it declares hooks more than once")
	}
	var hooks []jsonMember
	if hooksIndex >= 0 {
		if hooks, err = decodeJSONObject(root[hooksIndex].value); err != nil {
			return nil, false, false, errors.New("its hooks member is not an object")
		}
	}

	for _, event := range extproto.HostHookEvents(host) {
		eventIndex, count := jsonMemberIndex(hooks, event)
		if count > 1 {
			return nil, false, false, fmt.Errorf("it declares hooks.%s more than once", event)
		}
		var groups [][]byte
		if eventIndex >= 0 {
			if groups, err = decodeJSONArray(hooks[eventIndex].value); err != nil {
				return nil, false, false, fmt.Errorf("its hooks.%s member is not an array", event)
			}
		}
		merged := mergeHostHookGroups(groups, owns, desired[event], unit)
		switch {
		case eventIndex < 0 && len(merged) == 0:
		case eventIndex < 0:
			hooks = append(hooks, jsonMember{key: event, value: renderJSONArray(merged, 2, unit)})
			changed = true
		case jsonEqual(hooks[eventIndex].value, renderJSONArray(merged, 2, unit)):
		case len(merged) == 0:
			hooks = slices.Delete(hooks, eventIndex, eventIndex+1)
			changed = true
		default:
			hooks[eventIndex].value = renderJSONArray(merged, 2, unit)
			changed = true
		}
	}
	if !changed {
		return nil, false, false, nil
	}
	switch {
	case hooksIndex < 0:
		root = append(root, jsonMember{key: "hooks", value: renderJSONObject(hooks, 1, unit)})
	case len(hooks) == 0:
		root = slices.Delete(root, hooksIndex, hooksIndex+1)
	default:
		root[hooksIndex].value = renderJSONObject(hooks, 1, unit)
	}
	if len(root) == 0 {
		return nil, true, true, nil
	}
	return append(renderJSONObject(root, 0, unit), trailing...), false, true, nil
}

// mergeHostHookGroups merges one event's groups. A group whose handlers are
// all Putnami's becomes a place for a new group; a group that also holds the
// person's handlers keeps them, and a place follows it.
func mergeHostHookGroups(groups [][]byte, owns func(string) bool, desired []hostHookGroupJSON, unit string) [][]byte {
	var places [][]byte // nil marks a place for a new group
	for _, group := range groups {
		kept, ours := splitHostHookGroup(group, owns, unit)
		if !ours {
			places = append(places, group)
			continue
		}
		if kept != nil {
			places = append(places, kept)
		}
		places = append(places, nil)
	}
	merged := make([][]byte, 0, len(places)+len(desired))
	next := 0
	for _, place := range places {
		switch {
		case place != nil:
			merged = append(merged, place)
		case next < len(desired):
			merged = append(merged, renderJSONValue(desired[next], 3, unit))
			next++
		}
	}
	for ; next < len(desired); next++ {
		merged = append(merged, renderJSONValue(desired[next], 3, unit))
	}
	return merged
}

// splitHostHookGroup removes Putnami's handlers from one group. ours reports
// whether it held any; kept is the group without them, or nil when nothing of
// the person's is left. A group of any other shape is the person's.
func splitHostHookGroup(group []byte, owns func(string) bool, unit string) (kept []byte, ours bool) {
	members, err := decodeJSONObject(group)
	if err != nil {
		return group, false
	}
	index, count := jsonMemberIndex(members, "hooks")
	if count != 1 {
		return group, false
	}
	handlers, err := decodeJSONArray(members[index].value)
	if err != nil {
		return group, false
	}
	var remaining [][]byte
	for _, handler := range handlers {
		if hostHookHandlerIsOurs(handler, owns) {
			ours = true
			continue
		}
		remaining = append(remaining, handler)
	}
	if !ours {
		return group, false
	}
	if len(remaining) == 0 {
		return nil, true
	}
	members[index].value = renderJSONArray(remaining, 4, unit)
	return renderJSONObject(members, 3, unit), true
}

// hostHookHandlerIsOurs reports whether a handler is an object whose one
// command member is a string owns accepts.
func hostHookHandlerIsOurs(handler []byte, owns func(string) bool) bool {
	members, err := decodeJSONObject(handler)
	if err != nil {
		return false
	}
	raw, ok := singleJSONMember(members, "command")
	if !ok {
		return false
	}
	var command string
	if json.Unmarshal(raw, &command) != nil {
		return false
	}
	return owns(command)
}

// jsonMember is one member of a JSON object, with the value's exact bytes.
type jsonMember struct {
	key   string
	value []byte
}

// decodeJSONObject splits a JSON object into its members, in order, repeated
// keys included. Each value keeps its exact bytes.
func decodeJSONObject(data []byte) ([]jsonMember, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := expectJSONDelim(decoder, '{'); err != nil {
		return nil, err
	}
	members := []jsonMember{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, _ := token.(string)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		members = append(members, jsonMember{key: key, value: value})
	}
	return members, endJSONValue(decoder)
}

// decodeJSONArray splits a JSON array into its items, each with its exact
// bytes.
func decodeJSONArray(data []byte) ([][]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := expectJSONDelim(decoder, '['); err != nil {
		return nil, err
	}
	items := [][]byte{}
	for decoder.More() {
		var item json.RawMessage
		if err := decoder.Decode(&item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, endJSONValue(decoder)
}

func expectJSONDelim(decoder *json.Decoder, want json.Delim) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); !ok || delim != want {
		return fmt.Errorf("not a JSON %s", map[json.Delim]string{'{': "object", '[': "array"}[want])
	}
	return nil
}

// endJSONValue reads the closing delimiter and refuses anything after it.
func endJSONValue(decoder *json.Decoder) error {
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the value")
	}
	return nil
}

// jsonMemberIndex returns the index of the last member named key and how
// many members carry that name.
func jsonMemberIndex(members []jsonMember, key string) (int, int) {
	index, count := -1, 0
	for i, member := range members {
		if member.key == key {
			index = i
			count++
		}
	}
	return index, count
}

// singleJSONMember returns the value of key when exactly one member carries it.
func singleJSONMember(members []jsonMember, key string) ([]byte, bool) {
	index, count := jsonMemberIndex(members, key)
	if count != 1 {
		return nil, false
	}
	return members[index].value, true
}

// jsonIndentUnit returns the indentation of the first member of the document's
// root object: the unit its writer used. A compact or empty document, or one
// that mixes tabs and spaces, gets two spaces.
func jsonIndentUnit(data []byte) string {
	open := bytes.IndexByte(data, '{')
	if open < 0 {
		return "  "
	}
	rest := data[open+1:]
	end := 0
	for end < len(rest) && strings.ContainsRune(" \t\r\n", rune(rest[end])) {
		end++
	}
	lead := rest[:end]
	newline := bytes.LastIndexByte(lead, '\n')
	if newline < 0 || end == len(rest) || rest[end] == '}' {
		return "  "
	}
	unit := string(lead[newline+1:])
	if unit == "" || (strings.Trim(unit, " ") != "" && strings.Trim(unit, "\t") != "") {
		return "  "
	}
	return unit
}

// renderJSONObject writes members as an object whose closing brace sits at
// depth, one member per line. Each value must already be valid at depth+1.
func renderJSONObject(members []jsonMember, depth int, unit string) []byte {
	if len(members) == 0 {
		return []byte("{}")
	}
	var out bytes.Buffer
	out.WriteByte('{')
	for i, member := range members {
		if i > 0 {
			out.WriteByte(',')
		}
		out.WriteByte('\n')
		out.WriteString(strings.Repeat(unit, depth+1))
		out.Write(encodeJSONString(member.key))
		out.WriteString(": ")
		out.Write(member.value)
	}
	out.WriteByte('\n')
	out.WriteString(strings.Repeat(unit, depth))
	out.WriteByte('}')
	return out.Bytes()
}

// renderJSONArray writes items as an array whose closing bracket sits at
// depth, one item per line. Each item must already be valid at depth+1.
func renderJSONArray(items [][]byte, depth int, unit string) []byte {
	if len(items) == 0 {
		return []byte("[]")
	}
	var out bytes.Buffer
	out.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			out.WriteByte(',')
		}
		out.WriteByte('\n')
		out.WriteString(strings.Repeat(unit, depth+1))
		out.Write(item)
	}
	out.WriteByte('\n')
	out.WriteString(strings.Repeat(unit, depth))
	out.WriteByte(']')
	return out.Bytes()
}

// renderJSONValue encodes a new value at depth. It escapes no HTML character,
// so a guard's `&&` reads as written.
func renderJSONValue(value any, depth int, unit string) []byte {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		// Every value rendered here is a literal of JSON types.
		panic("agentctx: host hook value is not encodable: " + err.Error())
	}
	var out bytes.Buffer
	if err := json.Indent(&out, bytes.TrimRight(encoded.Bytes(), "\n"), strings.Repeat(unit, depth), unit); err != nil {
		panic("agentctx: host hook value does not indent: " + err.Error())
	}
	return out.Bytes()
}

func encodeJSONString(value string) []byte {
	return bytes.TrimRight(renderJSONValue(value, 0, ""), "\n")
}

// jsonEqual compares two JSON texts without their insignificant whitespace.
func jsonEqual(a, b []byte) bool {
	var left, right bytes.Buffer
	if json.Compact(&left, a) != nil || json.Compact(&right, b) != nil {
		return false
	}
	return bytes.Equal(left.Bytes(), right.Bytes())
}
