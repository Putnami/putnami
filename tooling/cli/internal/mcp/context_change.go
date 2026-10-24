package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	ciproto "go.putnami.dev/protocol/ci"
	featureproto "go.putnami.dev/protocol/features"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The server notices a change of the agent context between two requests.
//
// Before it answers a tools/call or a resources/read, it hashes the guidance
// inputs: the three root documents, the manifest and AI.md of each extension
// loaded by path, and the decisions.json files its last workspace load found.
// No file watcher runs. When the digest differs from the one the session last
// heard about, the next tool result ends with one more text block that says
// what changed and what to read. That block is the one way a change reaches
// the model: no host shows a notification to it. A resources/read notices the
// change and reloads for it, but leaves the notice for the next tool result,
// because a resource has no free-text slot. A tools/call that ends in a
// JSON-RPC error leaves it pending too.
//
// When the extension set changed, the server reloads its extension selection,
// guides and tools through Options.ReloadExtensions, then writes
// notifications/tools/list_changed and notifications/resources/list_changed,
// in that order, before the response of the request that noticed the change.

const (
	workspaceConfigInput = wsproto.WorkspaceConfigFilename
	lockInput            = lockfile.LockFilename
	ciInput              = ciproto.Filename
	extensionGuideName   = "AI.md"

	// maxRetainedInputBytes bounds the bytes a snapshot keeps of one input it
	// parses to describe a change. A larger input is still hashed; its change
	// is described as "<file> changed".
	maxRetainedInputBytes = 1 << 20
	// maxNoticeItemsPerLine bounds the changes one notice line names before
	// it counts the rest.
	maxNoticeItemsPerLine = 8
	// hashBufferBytes is the size of the one buffer a snapshot hashes its
	// inputs through.
	hashBufferBytes = 32 << 10
)

type contextInputKind int

const (
	inputWorkspaceConfig contextInputKind = iota
	inputLock
	inputCI
	inputDecisions
	inputExtensionManifest
	inputExtensionGuide
)

// contextInput is one present input of a snapshot.
type contextInput struct {
	kind contextInputKind
	// owner is the project id of a project decision registry, and the label of
	// the extension an extension file belongs to.
	owner string
	sum   [sha256.Size]byte
	// data holds the bytes of an input the notice parses: the workspace
	// configuration, the lock and a decision registry, up to
	// maxRetainedInputBytes. It is nil for every other input.
	data []byte
}

// contextSnapshot is the content of the inputs at one moment. files holds the
// present inputs by slash-separated path, workspace-relative inside the
// workspace. An absent input has no entry, so an input that leaves the input
// set and an input that is deleted read the same.
type contextSnapshot struct {
	files  map[string]contextInput
	digest [sha256.Size]byte
}

// pathExtension is an extension loaded by path. label is its directory,
// slash-separated and workspace-relative inside the workspace; dir is the same
// directory on disk.
type pathExtension struct {
	label string
	dir   string
}

// decisionFile is a project decision registry the last workspace load found.
type decisionFile struct {
	path      string
	projectID string
}

// contextState is the change-tracking state contextMu guards.
type contextState struct {
	// reported is what the session last heard about: the inputs at session
	// start, then the inputs of each notice a tool result carried.
	reported contextSnapshot
	// loaded is what the extension selection and the tools were built from.
	loaded         contextSnapshot
	pathExtensions []pathExtension
	decisionFiles  []decisionFile
}

// contextNotice is a notice a tool result may carry: its text, the digest it
// was described from, and the snapshot it reports.
type contextNotice struct {
	text string
	from [sha256.Size]byte
	to   contextSnapshot
}

// startContextTracking reads the inputs the session starts from.
func (s *Server) startContextTracking() {
	snapshot := s.readContextInputs()
	s.contextMu.Lock()
	defer s.contextMu.Unlock()
	s.changes.reported = snapshot
	s.changes.loaded = snapshot
}

// noteWorkspaceLoad records the project decision registries a workspace load
// finds. The root registry is always an input, so it is not recorded here.
func (s *Server) noteWorkspaceLoad(ws *workspace.Workspace) {
	if ws == nil || s.opts.WorkspaceRoot == "" {
		return
	}
	var found []decisionFile
	seen := make(map[string]bool)
	for _, project := range ws.Projects {
		if project == nil {
			continue
		}
		registry := featureproto.DecisionRegistryPath(filepath.ToSlash(project.Path))
		if registry == featureproto.DecisionsFilename || seen[registry] {
			continue
		}
		info, err := os.Stat(filepath.Join(s.opts.WorkspaceRoot, filepath.FromSlash(registry)))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		seen[registry] = true
		found = append(found, decisionFile{path: registry, projectID: project.ID})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].path < found[j].path })
	s.contextMu.Lock()
	defer s.contextMu.Unlock()
	s.changes.decisionFiles = found
}

// notePathExtensions records the extensions loaded by path: the discovered
// extensions loaded from a directory, and every path-shaped entry of the
// configuration's extensions, whose manifest may not load today and may load
// after an edit.
func (s *Server) notePathExtensions(exts []*extension.ExtensionDescription) {
	root := s.opts.WorkspaceRoot
	if root == "" {
		return
	}
	byLabel := make(map[string]pathExtension)
	addRelative := func(rel string) {
		rel = filepath.Clean(rel)
		if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return
		}
		label := filepath.ToSlash(rel)
		byLabel[label] = pathExtension{label: label, dir: filepath.Join(root, rel)}
	}
	for _, ext := range exts {
		if ext == nil || !ext.LocalSource {
			continue
		}
		switch {
		case ext.RelPath != "":
			addRelative(ext.RelPath)
		case ext.Path != "":
			label := filepath.ToSlash(filepath.Clean(ext.Path))
			byLabel[label] = pathExtension{label: label, dir: ext.Path}
		}
	}
	if cfg := s.currentSelection().Config; cfg != nil {
		for _, ref := range cfg.Extensions.Names() {
			if strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "./") {
				addRelative(filepath.FromSlash(strings.TrimLeft(ref, "/")))
			}
		}
	}
	recorded := make([]pathExtension, 0, len(byLabel))
	for _, item := range byLabel {
		recorded = append(recorded, item)
	}
	sort.Slice(recorded, func(i, j int) bool { return recorded[i].label < recorded[j].label })
	s.contextMu.Lock()
	defer s.contextMu.Unlock()
	s.changes.pathExtensions = recorded
}

// readContextInputs reads and hashes every input. A server without a
// workspace root has none.
func (s *Server) readContextInputs() contextSnapshot {
	snapshot := contextSnapshot{files: make(map[string]contextInput)}
	root := s.opts.WorkspaceRoot
	if root == "" {
		snapshot.digest = snapshotDigest(snapshot.files)
		return snapshot
	}
	s.contextMu.Lock()
	extensions := append([]pathExtension(nil), s.changes.pathExtensions...)
	decisions := append([]decisionFile(nil), s.changes.decisionFiles...)
	s.contextMu.Unlock()

	buf := make([]byte, hashBufferBytes)
	add := func(key, file string, kind contextInputKind, owner string, retain bool) {
		sum, data, ok := hashContextInput(file, retain, buf)
		if ok {
			snapshot.files[key] = contextInput{kind: kind, owner: owner, sum: sum, data: data}
		}
	}
	add(workspaceConfigInput, filepath.Join(root, workspaceConfigInput), inputWorkspaceConfig, "", true)
	add(lockInput, filepath.Join(root, lockInput), inputLock, "", true)
	add(ciInput, filepath.Join(root, ciInput), inputCI, "", false)
	add(featureproto.DecisionsFilename, filepath.Join(root, featureproto.DecisionsFilename), inputDecisions, "", true)
	for _, registry := range decisions {
		add(registry.path, filepath.Join(root, filepath.FromSlash(registry.path)), inputDecisions, registry.projectID, true)
	}
	for _, ext := range extensions {
		add(ext.label+"/"+extension.ManifestFilename, filepath.Join(ext.dir, extension.ManifestFilename), inputExtensionManifest, ext.label, false)
		add(ext.label+"/"+extensionGuideName, filepath.Join(ext.dir, extensionGuideName), inputExtensionGuide, ext.label, false)
	}
	snapshot.digest = snapshotDigest(snapshot.files)
	return snapshot
}

// hashContextInput hashes one input file, copying it through buf. ok is false
// when the file is absent or unreadable, which the snapshot records as absent.
// With retain, it returns the bytes too, up to maxRetainedInputBytes.
func hashContextInput(file string, retain bool, buf []byte) (sum [sha256.Size]byte, data []byte, ok bool) {
	handle, err := os.Open(file)
	if err != nil {
		return sum, nil, false
	}
	defer handle.Close()
	hash := sha256.New()
	if retain {
		data, err = io.ReadAll(io.LimitReader(handle, maxRetainedInputBytes+1))
		if err != nil {
			return sum, nil, false
		}
		hash.Write(data)
		if len(data) > maxRetainedInputBytes {
			data = nil
		}
	}
	// The wrapper hides the file's WriteTo, which allocates a buffer on each
	// call on some platforms, so every input reuses buf.
	if _, err := io.CopyBuffer(hash, struct{ io.Reader }{handle}, buf); err != nil {
		return sum, nil, false
	}
	copy(sum[:], hash.Sum(nil))
	return sum, data, true
}

// snapshotDigest is the digest of the present inputs, by path.
func snapshotDigest(files map[string]contextInput) [sha256.Size]byte {
	keys := make([]string, 0, len(files))
	for key := range files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	hash := sha256.New()
	for _, key := range keys {
		sum := files[key].sum
		hash.Write([]byte(key))
		hash.Write([]byte{0})
		hash.Write(sum[:])
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}

// observeContext hashes the inputs before a request is answered. When the
// extension set changed since the last load, it reloads and notifies first.
// With describe, it returns the notice of what changed since the session last
// heard about it; the notice is reported only when withNotice attaches it.
func (s *Server) observeContext(ctx context.Context, describe bool) contextNotice {
	if s.opts.WorkspaceRoot == "" {
		return contextNotice{}
	}
	s.observeMu.Lock()
	defer s.observeMu.Unlock()
	current := s.readContextInputs()
	s.contextMu.Lock()
	loaded, reported := s.changes.loaded, s.changes.reported
	s.contextMu.Unlock()
	if current.digest != loaded.digest {
		if s.opts.ReloadExtensions != nil && extensionSetKey(loaded) != extensionSetKey(current) {
			s.reloadExtensions(ctx)
			current = s.readContextInputs()
		}
		s.contextMu.Lock()
		s.changes.loaded = current
		s.contextMu.Unlock()
	}
	if !describe || current.digest == reported.digest {
		return contextNotice{}
	}
	targets := s.noticeTargets()
	return contextNotice{
		text: renderContextNotice(describeContextChanges(reported, current, targets), reloadedBetween(reported, current, targets)),
		from: reported.digest,
		to:   current,
	}
}

// reloadExtensions replaces the extension selection, rebuilds the tools, and
// tells the client that both lists changed.
func (s *Server) reloadExtensions(ctx context.Context) {
	selection := s.opts.ReloadExtensions(ctx)
	s.selectionMu.Lock()
	s.selection = selection
	s.selectionMu.Unlock()
	s.rebuildTools()
	s.notify(notificationToolsListChanged)
	s.notify(notificationResourcesListChanged)
}

// withNotice appends the notice to a tool result as its last text block, and
// marks the change reported. A JSON-RPC error carries no notice and leaves it
// pending. A notice described from a state another request already reported
// is dropped, so one change is reported once.
func (s *Server) withNotice(resp *rpcResponse, notice contextNotice) *rpcResponse {
	if notice.text == "" || resp == nil {
		return resp
	}
	result, ok := resp.Result.(callToolResult)
	if !ok {
		return resp
	}
	s.contextMu.Lock()
	defer s.contextMu.Unlock()
	if s.changes.reported.digest != notice.from {
		return resp
	}
	s.changes.reported = notice.to
	content := make([]toolContent, 0, len(result.Content)+1)
	content = append(content, result.Content...)
	content = append(content, toolContent{Type: "text", Text: notice.text})
	result.Content = content
	resp.Result = result
	return resp
}

// noticeTargets names what a notice may point at: a resource only when this
// server lists it.
type noticeTargets struct {
	guidance       bool
	projectContext bool
	reload         bool
}

func (s *Server) noticeTargets() noticeTargets {
	return noticeTargets{
		guidance:       s.opts.WorkspaceGuidance != nil,
		projectContext: s.opts.AgentContext != nil,
		reload:         s.opts.ReloadExtensions != nil,
	}
}

// extensionSetKey identifies the extension set of a snapshot: the declared
// extensions and their constraints, the disabled extensions, the locked
// version and manifest hash of each extension, and the manifest of each
// extension loaded by path. An input that does not parse counts by its hash.
func extensionSetKey(snapshot contextSnapshot) string {
	var parts []string
	if input, ok := snapshot.files[workspaceConfigInput]; ok {
		if declared, ok := parseWorkspaceExtensions(input); ok {
			for _, ref := range sortedKeys(declared.refs) {
				parts = append(parts, "declared "+ref+" "+declared.refs[ref])
			}
			for _, name := range sortedKeys(declared.disabled) {
				parts = append(parts, "disabled "+name)
			}
		} else {
			parts = append(parts, "workspace "+hex.EncodeToString(input.sum[:]))
		}
	}
	if input, ok := snapshot.files[lockInput]; ok {
		if locked, ok := parseLockedExtensions(input); ok {
			for _, name := range sortedKeys(locked) {
				parts = append(parts, "locked "+name+" "+locked[name].Version+" "+locked[name].ManifestHash)
			}
		} else {
			parts = append(parts, "lock "+hex.EncodeToString(input.sum[:]))
		}
	}
	for _, key := range sortedKeys(snapshot.files) {
		if input := snapshot.files[key]; input.kind == inputExtensionManifest {
			parts = append(parts, "manifest "+key+" "+hex.EncodeToString(input.sum[:]))
		}
	}
	return strings.Join(parts, "\n")
}

// workspaceExtensions is the extension part of putnami.workspace.json.
type workspaceExtensions struct {
	refs     map[string]string
	disabled map[string]bool
}

func parseWorkspaceExtensions(input contextInput) (workspaceExtensions, bool) {
	if input.data == nil {
		return workspaceExtensions{}, false
	}
	var document struct {
		Extensions wsproto.ExtensionsConfig `json:"extensions"`
		Disable    *wsproto.DisableConfig   `json:"disable"`
	}
	if err := json.Unmarshal(input.data, &document); err != nil {
		return workspaceExtensions{}, false
	}
	declared := workspaceExtensions{refs: document.Extensions.List, disabled: make(map[string]bool)}
	if declared.refs == nil {
		declared.refs = make(map[string]string)
	}
	if document.Disable != nil {
		for _, name := range document.Disable.Extensions {
			declared.disabled[name] = true
		}
	}
	return declared, true
}

// lockedExtension is the part of a lock entry that identifies an installed
// extension.
type lockedExtension struct {
	Version      string `json:"version"`
	ManifestHash string `json:"manifestHash"`
}

func parseLockedExtensions(input contextInput) (map[string]lockedExtension, bool) {
	if input.data == nil {
		return nil, false
	}
	var document struct {
		Extensions map[string]lockedExtension `json:"extensions"`
	}
	if err := json.Unmarshal(input.data, &document); err != nil {
		return nil, false
	}
	if document.Extensions == nil {
		document.Extensions = make(map[string]lockedExtension)
	}
	return document.Extensions, true
}

// contextChange is one change in plain words, with the resource URI or file
// to read for it. read is empty when nothing remains to read, for a removed
// extension for example.
type contextChange struct {
	text string
	read string
}

// describeContextChanges lists what changed between two snapshots, in a fixed
// order: the workspace guidance, the project decisions, the extensions, then
// the rest of the workspace configuration and lock.
func describeContextChanges(from, to contextSnapshot, targets noticeTargets) []contextChange {
	guidanceRead := func(file string) string {
		if targets.guidance {
			return workspaceGuidanceURI
		}
		return file
	}
	var changes []contextChange
	changes = append(changes, decisionChanges(from, to, featureproto.DecisionsFilename, guidanceRead(featureproto.DecisionsFilename), false)...)
	if inputChanged(from, to, ciInput) {
		changes = append(changes, contextChange{text: ciInput + " changed", read: guidanceRead(ciInput)})
	}
	for _, key := range unionKeys(from, to) {
		if key == featureproto.DecisionsFilename || inputOf(from, to, key).kind != inputDecisions {
			continue
		}
		read := key
		if owner := inputOf(from, to, key).owner; owner != "" && targets.projectContext {
			read = projectContextURI(owner)
		}
		changes = append(changes, decisionChanges(from, to, key, read, true)...)
	}

	mentioned := make(map[string]bool)
	lockChanges := lockedExtensionChanges(from, to, mentioned)
	changes = append(changes, lockChanges...)
	changes = append(changes, pathExtensionChanges(from, to, mentioned)...)
	workspaceChanges, declaredDiffers := declaredExtensionChanges(from, to, mentioned)
	changes = append(changes, workspaceChanges...)
	if inputChanged(from, to, workspaceConfigInput) && !declaredDiffers {
		changes = append(changes, contextChange{text: workspaceConfigInput + " changed", read: workspaceContextURI})
	}
	if inputChanged(from, to, lockInput) && len(lockChanges) == 0 {
		changes = append(changes, contextChange{text: lockInput + " changed", read: workspaceContextURI})
	}
	return changes
}

// reloadedBetween reports whether the server reloaded its extensions for the
// change between two snapshots.
func reloadedBetween(from, to contextSnapshot, targets noticeTargets) bool {
	return targets.reload && extensionSetKey(from) != extensionSetKey(to)
}

// decisionChanges names each decision a registry added, changed or removed.
// A registry that does not parse on either side is named as a whole, and so
// is one whose bytes changed with no decision changing.
func decisionChanges(from, to contextSnapshot, key, read string, project bool) []contextChange {
	if !inputChanged(from, to, key) {
		return nil
	}
	before, beforeOK := parseDecisions(from, key)
	after, afterOK := parseDecisions(to, key)
	whole := func() []contextChange {
		verb := " changed"
		if _, present := to.files[key]; !present {
			verb = " removed"
		} else if _, present := from.files[key]; !present {
			verb = " added"
		}
		return []contextChange{{text: key + verb, read: read}}
	}
	if !beforeOK || !afterOK {
		return whole()
	}
	suffix := ""
	if project {
		suffix = " in " + key
	}
	var changes []contextChange
	for _, id := range sortedKeys(mergeKeys(before, after)) {
		old, inBefore := before[id]
		current, inAfter := after[id]
		switch {
		case !inBefore:
			changes = append(changes, contextChange{text: id + " added" + suffix, read: read})
		case !inAfter:
			changes = append(changes, contextChange{text: id + " removed" + suffix, read: read})
		case old != current:
			changes = append(changes, contextChange{text: id + " changed" + suffix, read: read})
		}
	}
	if len(changes) == 0 {
		return whole()
	}
	return changes
}

// parseDecisions maps each decision id of a registry to its encoded form. An
// absent registry has no decisions.
func parseDecisions(snapshot contextSnapshot, key string) (map[string]string, bool) {
	input, present := snapshot.files[key]
	if !present {
		return map[string]string{}, true
	}
	if input.data == nil {
		return nil, false
	}
	registry, _ := featureproto.ParseAndValidateDecisionRegistry(input.data)
	if registry == nil {
		return nil, false
	}
	decisions := make(map[string]string, len(registry.Decisions))
	for _, decision := range registry.Decisions {
		encoded, _ := json.Marshal(decision)
		decisions[decision.ID] = string(encoded)
	}
	return decisions, true
}

// lockedExtensionChanges names each extension the lock installed, replaced or
// removed. It records each name it mentions.
func lockedExtensionChanges(from, to contextSnapshot, mentioned map[string]bool) []contextChange {
	if !inputChanged(from, to, lockInput) {
		return nil
	}
	before, beforeOK := lockedExtensionsOf(from)
	after, afterOK := lockedExtensionsOf(to)
	if !beforeOK || !afterOK {
		return nil
	}
	var changes []contextChange
	for _, name := range sortedKeys(mergeKeys(before, after)) {
		old, inBefore := before[name]
		current, inAfter := after[name]
		switch {
		case !inBefore:
			changes = append(changes, contextChange{text: nameAtVersion(name, current.Version) + " installed", read: extensionGuidanceURI(name, current.Version)})
		case !inAfter:
			changes = append(changes, contextChange{text: name + " removed"})
		case old.Version != current.Version:
			changes = append(changes, contextChange{
				text: nameAtVersion(name, current.Version) + " installed, replacing " + old.Version,
				read: extensionGuidanceURI(name, current.Version),
			})
		case old.ManifestHash != current.ManifestHash:
			changes = append(changes, contextChange{text: nameAtVersion(name, current.Version) + " installed again with another manifest", read: extensionGuidanceURI(name, current.Version)})
		default:
			continue
		}
		mentioned[name] = true
	}
	return changes
}

func lockedExtensionsOf(snapshot contextSnapshot) (map[string]lockedExtension, bool) {
	input, present := snapshot.files[lockInput]
	if !present {
		return map[string]lockedExtension{}, true
	}
	return parseLockedExtensions(input)
}

func nameAtVersion(name, version string) string {
	if version == "" {
		return name
	}
	return name + " " + version
}

// pathExtensionChanges names each extension loaded by path whose manifest or
// AI.md changed, and each one that started or stopped loading. It records
// each directory it mentions.
func pathExtensionChanges(from, to contextSnapshot, mentioned map[string]bool) []contextChange {
	labels := make(map[string]bool)
	for _, key := range unionKeys(from, to) {
		if input := inputOf(from, to, key); input.kind == inputExtensionManifest || input.kind == inputExtensionGuide {
			labels[input.owner] = true
		}
	}
	var changes []contextChange
	for _, label := range sortedKeys(labels) {
		manifest := label + "/" + extension.ManifestFilename
		guide := label + "/" + extensionGuideName
		_, manifestBefore := from.files[manifest]
		_, manifestAfter := to.files[manifest]
		_, guideAfter := to.files[guide]
		read := manifest
		if guideAfter {
			read = guide
		}
		switch {
		case !manifestBefore && !manifestAfter:
			continue
		case !manifestAfter:
			changes = append(changes, contextChange{text: "extension " + label + " no longer loads"})
			mentioned[extensionRefKey(label)] = true
			continue
		case !manifestBefore:
			changes = append(changes, contextChange{text: "extension " + label + " now loads", read: read})
			mentioned[extensionRefKey(label)] = true
			continue
		}
		if inputChanged(from, to, manifest) {
			changes = append(changes, contextChange{text: manifest + " changed", read: manifest})
			mentioned[extensionRefKey(label)] = true
		}
		if inputChanged(from, to, guide) {
			verb := " changed"
			target := guide
			if !guideAfter {
				verb, target = " removed", ""
			} else if _, guideBefore := from.files[guide]; !guideBefore {
				verb = " added"
			}
			changes = append(changes, contextChange{text: guide + verb, read: target})
			mentioned[extensionRefKey(label)] = true
		}
	}
	return changes
}

// declaredExtensionChanges names each extension the workspace configuration
// declared, dropped, disabled or enabled, except one a lock or path line
// already named. differs reports whether the declared extensions changed at
// all, named or not.
func declaredExtensionChanges(from, to contextSnapshot, mentioned map[string]bool) (changes []contextChange, differs bool) {
	if !inputChanged(from, to, workspaceConfigInput) {
		return nil, false
	}
	before, beforeOK := declaredExtensionsOf(from)
	after, afterOK := declaredExtensionsOf(to)
	if !beforeOK || !afterOK {
		return nil, false
	}
	locked, _ := lockedExtensionsOf(to)
	for _, ref := range sortedKeys(mergeKeys(before.refs, after.refs)) {
		old, inBefore := before.refs[ref]
		current, inAfter := after.refs[ref]
		if inBefore && inAfter && old == current {
			continue
		}
		differs = true
		if mentioned[ref] || mentioned[extensionRefKey(ref)] {
			continue
		}
		switch {
		case !inBefore:
			read := ""
			if entry, ok := locked[ref]; ok {
				read = extensionGuidanceURI(ref, entry.Version)
			}
			changes = append(changes, contextChange{text: ref + " added to the workspace extensions", read: read})
		case !inAfter:
			changes = append(changes, contextChange{text: ref + " removed from the workspace extensions"})
		case old != current:
			changes = append(changes, contextChange{text: "the " + ref + " version constraint changed", read: workspaceContextURI})
		}
	}
	for _, name := range sortedKeys(mergeKeys(before.disabled, after.disabled)) {
		switch {
		case !before.disabled[name]:
			changes = append(changes, contextChange{text: name + " disabled"})
		case !after.disabled[name]:
			changes = append(changes, contextChange{text: name + " enabled"})
		default:
			continue
		}
		differs = true
	}
	return changes, differs
}

func declaredExtensionsOf(snapshot contextSnapshot) (workspaceExtensions, bool) {
	input, present := snapshot.files[workspaceConfigInput]
	if !present {
		return workspaceExtensions{refs: map[string]string{}, disabled: map[string]bool{}}, true
	}
	return parseWorkspaceExtensions(input)
}

// extensionRefKey reduces a path-shaped extension reference and a path
// extension label to one key: "/tools/x", "./tools/x" and "tools/x" match.
func extensionRefKey(ref string) string {
	return strings.Trim(path.Clean("/"+strings.TrimPrefix(filepath.ToSlash(ref), "./")), "/")
}

// renderContextNotice writes the notice: a header, one line per thing to read
// naming the changes that point at it, and a last line when the server
// reloaded its extensions.
func renderContextNotice(changes []contextChange, reloaded bool) string {
	if len(changes) == 0 && !reloaded {
		return ""
	}
	type line struct {
		read  string
		texts []string
	}
	var lines []*line
	byRead := make(map[string]*line)
	for _, change := range changes {
		current, ok := byRead[change.read]
		if !ok {
			current = &line{read: change.read}
			byRead[change.read] = current
			lines = append(lines, current)
		}
		current.texts = append(current.texts, change.text)
	}
	var b strings.Builder
	b.WriteString("Putnami notice: the workspace context changed.")
	for _, item := range lines {
		texts := item.texts
		if len(texts) > maxNoticeItemsPerLine {
			capped := make([]string, 0, maxNoticeItemsPerLine+1)
			capped = append(capped, texts[:maxNoticeItemsPerLine]...)
			capped = append(capped, fmt.Sprintf("%d more", len(texts)-maxNoticeItemsPerLine))
			texts = capped
		}
		b.WriteString("\n- ")
		b.WriteString(strings.Join(texts, "; "))
		b.WriteString(".")
		if item.read != "" {
			b.WriteString(" Read ")
			b.WriteString(item.read)
		}
	}
	if reloaded {
		b.WriteString("\n- Putnami reloaded its extension tools and guides; list the tools again.")
	}
	return b.String()
}

func inputChanged(from, to contextSnapshot, key string) bool {
	before, inBefore := from.files[key]
	after, inAfter := to.files[key]
	return inBefore != inAfter || before.sum != after.sum
}

// inputOf returns the input of key from the newer snapshot, else the older.
func inputOf(from, to contextSnapshot, key string) contextInput {
	if input, ok := to.files[key]; ok {
		return input
	}
	return from.files[key]
}

func unionKeys(from, to contextSnapshot) []string {
	return sortedKeys(mergeKeys(from.files, to.files))
}

func mergeKeys[V any](a, b map[string]V) map[string]bool {
	keys := make(map[string]bool, len(a)+len(b))
	for key := range a {
		keys[key] = true
	}
	for key := range b {
		keys[key] = true
	}
	return keys
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
