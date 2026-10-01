package depsupgrade

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"go.putnami.dev/go/extension/internal/workspacejob"
)

// joinPath joins path elements with the platform separator without cleaning
// them: the go.mod paths the job prints and compares keep the spelling
// go.work gave them ("<root>/./app/go.mod"), as the script's did.
func joinPath(elements ...string) string {
	return joinPathWith(filepath.Separator, elements...)
}

// joinPathWith is joinPath on a platform whose separator is separator. go.work
// spells a use directory with slashes on every platform, so each element's
// slashes become separator: one path never mixes two separators.
func joinPathWith(separator byte, elements ...string) string {
	sep := string(separator)
	converted := make([]string, len(elements))
	for i, element := range elements {
		converted[i] = strings.ReplaceAll(element, "/", sep)
	}
	return strings.Join(converted, sep)
}

// sortedUnique sorts values in byte order and drops duplicates and empty
// strings: the script's `sed '/^$/d' | sort -u`.
func sortedUnique(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	unique := out[:0]
	for i, value := range out {
		if i > 0 && value == out[i-1] {
			continue
		}
		unique = append(unique, value)
	}
	return unique
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// sortModules sorts modules as the script sorted "path<TAB>version" lines.
func sortModules(modules []workspacejob.Module) {
	sort.Slice(modules, func(a, b int) bool {
		return modules[a].Path+"\t"+modules[a].Version < modules[b].Path+"\t"+modules[b].Version
	})
}

// containsGoFile reports whether dir, or an entry directly inside it, is named
// *.go: the script's `find dir -maxdepth 1 -name '*.go'`.
func containsGoFile(dir string) bool {
	if matched, _ := filepath.Match("*.go", filepath.Base(dir)); matched {
		return true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if matched, _ := filepath.Match("*.go", entry.Name()); matched {
			return true
		}
	}
	return false
}

// readWork parses go.work, or returns an empty one when it does not exist.
func (u *upgrade) readWork() (workspacejob.WorkFile, bool) {
	if !workspacejob.FileExists(u.goWork) {
		return workspacejob.WorkFile{}, true
	}
	work, err := u.ReadWorkFile(u.goWork)
	return work, err == nil
}

// ensureGoWork creates a go.work naming the resolved Go version when the
// workspace has none. It reports false, after an error, when it cannot write
// it.
func (u *upgrade) ensureGoWork() bool {
	if workspacejob.FileExists(u.goWork) {
		return true
	}
	if u.DryRun {
		u.Emit.Log("info", "Would create go.work")
		return true
	}
	version := u.GoVersion()
	if version == "" {
		version = "1.22"
	}
	if err := os.WriteFile(u.goWork, []byte("go "+version+"\n\n"), 0o644); err != nil {
		u.Emit.Log("error", "Cannot create "+u.goWork+": "+err.Error())
		return false
	}
	u.Emit.Log("info", "Created go.work")
	return true
}

// useDirGoMods returns the go.mod of every module go.work uses, in go.work
// order.
func (u *upgrade) useDirGoMods(work workspacejob.WorkFile) []string {
	var goMods []string
	for _, use := range work.Use {
		dir := use.DiskPath
		if !filepath.IsAbs(dir) {
			dir = joinPath(u.WorkspaceRoot, dir)
		}
		goMod := joinPath(dir, "go.mod")
		if workspacejob.FileExists(goMod) {
			goMods = append(goMods, goMod)
		}
	}
	return goMods
}

// configuredGoMods returns the go.mod of every declared Go project. A
// consumer's go.work normally mirrors this set, but the job owns every
// declared project, including one whose use entry is being repaired.
func (u *upgrade) configuredGoMods() []string {
	var goMods []string
	for _, project := range u.projects {
		if project == "" {
			continue
		}
		if goMod := u.projectGoMod(project); workspacejob.FileExists(goMod) {
			goMods = append(goMods, goMod)
		}
	}
	return goMods
}

// workspaceGoMods is every Go module the job considers: the go.work members
// and the declared Go projects.
func (u *upgrade) workspaceGoMods(work workspacejob.WorkFile) []string {
	return sortedUnique(append(u.useDirGoMods(work), u.configuredGoMods()...))
}

// standaloneGoMods is the members whose own go.mod and go.sum are release-lock
// surfaces. Every other member is workspace-only: go.work and go.work.sum
// select its release while its authored go.mod stays a reusable baseline.
func (u *upgrade) standaloneGoMods(work workspacejob.WorkFile) []string {
	var goMods []string
	for _, goMod := range u.workspaceGoMods(work) {
		if workspacejob.UsesStandaloneMetadata(goMod) {
			goMods = append(goMods, goMod)
		}
	}
	return goMods
}

// begin snapshots every Go metadata surface the job may rewrite, so a failure
// restores the exact pre-upgrade state rather than a partially repinned
// workspace.
func (u *upgrade) begin() bool {
	work, ok := u.readWork()
	if !ok {
		u.Emit.Log("error", "Cannot parse "+u.goWork)
		return false
	}
	paths := []string{u.goWork, joinPath(u.WorkspaceRoot, "go.work.sum")}
	for _, goMod := range u.workspaceGoMods(work) {
		paths = append(paths, goMod, strings.TrimSuffix(goMod, ".mod")+".sum")
	}
	for _, path := range sortedUnique(paths) {
		if err := u.snapshot.Add(path); err != nil {
			u.snapshot = workspacejob.Snapshot{}
			return false
		}
	}
	u.active = true
	// A SIGINT or a SIGTERM rolls back too, then ends the job.
	u.Trap().Arm(u.rollback)
	return true
}

// rollback restores the snapshot when the transaction is still open, and
// emits an error naming every surface it could not put back.
func (u *upgrade) rollback() {
	if !u.active {
		return
	}
	u.active = false
	if err := u.snapshot.Restore(nil, nil); err != nil {
		u.Emit.Diagnostic("error", "Cannot roll back the Go metadata: "+err.Error(), "", 0)
	}
}

// commit keeps the rewritten surfaces.
func (u *upgrade) commit() {
	u.active = false
	u.snapshot = workspacejob.Snapshot{}
	u.Trap().Disarm()
}

// localGoWorkModules returns the module path of every module go.work uses.
func (u *upgrade) localGoWorkModules(work workspacejob.WorkFile) []string {
	var modules []string
	for _, goMod := range u.useDirGoMods(work) {
		if mod, err := u.ReadModFile(goMod); err == nil && mod.Module.Path != "" {
			modules = append(modules, mod.Module.Path)
		}
	}
	return modules
}

// dirOverrideModules returns the go.putnami.dev modules replaced by a local
// directory (a replacement without a version): they resolve from the file
// system, so no version pin applies to them.
func dirOverrideModules(replaces []workspacejob.Replace) []string {
	var modules []string
	for _, replace := range replaces {
		if strings.HasPrefix(replace.Old.Path, managedPrefix) && replace.New.Version == "" {
			modules = append(modules, replace.Old.Path)
		}
	}
	return modules
}

// directoryReplaced returns every module replaced by a local directory.
func directoryReplaced(replaces []workspacejob.Replace) []string {
	var modules []string
	for _, replace := range replaces {
		if replace.New.Version == "" {
			modules = append(modules, replace.Old.Path)
		}
	}
	return modules
}

// workspaceLocalModules returns the modules this workspace supplies the
// source of, as a go.work member or a directory replacement.
func (u *upgrade) workspaceLocalModules(work workspacejob.WorkFile) []string {
	return sortedUnique(append(u.localGoWorkModules(work), directoryReplaced(work.Replace)...))
}

// requiresWorkspaceMode reports whether goMod requires a module that resolves
// locally only because go.work supplies it. A directory replacement in the
// member itself makes that module resolvable standalone.
func (u *upgrade) requiresWorkspaceMode(goMod string, workspaceLocals []string) bool {
	if len(workspaceLocals) == 0 {
		return false
	}
	mod, err := u.ReadModFile(goMod)
	if err != nil {
		return true
	}
	memberReplaces := directoryReplaced(mod.Replace)
	for _, require := range mod.Require {
		if require.Path == "" || !contains(workspaceLocals, require.Path) {
			continue
		}
		if !contains(memberReplaces, require.Path) {
			return true
		}
	}
	return false
}

// pinModules returns every go.putnami.dev module the workspace pins: the
// published base list, the modules go.work already pins at a version, and
// the go.putnami.dev requirements of every workspace module.
func (u *upgrade) pinModules(work workspacejob.WorkFile) []string {
	modules := append([]string(nil), u.publishedPinModules()...)
	for _, replace := range work.Replace {
		if strings.HasPrefix(replace.Old.Path, managedPrefix) && replace.New.Version != "" {
			modules = append(modules, replace.Old.Path)
		}
	}
	for _, goMod := range u.workspaceGoMods(work) {
		mod, err := u.ReadModFile(goMod)
		if err != nil {
			continue
		}
		for _, require := range mod.Require {
			if strings.HasPrefix(require.Path, managedPrefix) {
				modules = append(modules, require.Path)
			}
		}
	}
	return sortedUnique(modules)
}

// managedEntries returns the go.putnami.dev entries of a go.mod list, in file
// order.
func managedEntries(entries []workspacejob.Module) []workspacejob.Module {
	var managed []workspacejob.Module
	for _, entry := range entries {
		if strings.HasPrefix(entry.Path, managedPrefix) {
			managed = append(managed, entry)
		}
	}
	return managed
}

func requiresAsModules(requires []workspacejob.Require) []workspacejob.Module {
	modules := make([]workspacejob.Module, 0, len(requires))
	for _, require := range requires {
		modules = append(modules, workspacejob.Module{Path: require.Path, Version: require.Version})
	}
	return modules
}

// reconcileExcludes drops the go.mod excludes of a selected member version:
// such an exclude makes the release impossible to select. Unrelated excludes
// are the user's and stay.
func (u *upgrade) reconcileExcludes(goMod string) bool {
	var excludes []workspacejob.Module
	if mod, err := u.ReadModFile(goMod); err == nil {
		excludes = managedEntries(mod.Exclude)
	}
	for _, exclude := range excludes {
		target, ok := u.targetVersion(exclude.Path)
		if !ok {
			u.emitMissing(exclude.Path)
			return false
		}
		if exclude.Version != target {
			continue
		}
		if u.DryRun {
			u.Emit.Log("info", "Would remove go.mod exclude "+exclude.Path+" "+exclude.Version+" from "+goMod)
			continue
		}
		if err := u.Inherit(true, nil, u.GoBinary, "mod", "edit", "-dropexclude="+exclude.Path+"@"+exclude.Version, goMod); err != nil {
			u.Emit.Log("error", "Cannot remove self-excluding "+exclude.Path+" "+exclude.Version+" from "+goMod)
			return false
		}
		u.Emit.Log("info", "Removed go.mod exclude "+exclude.Path+" "+exclude.Version+" from "+goMod)
	}
	return true
}

// normalizePins rewrites the requirements an older upgrade left at
// <target>-<old tail>, an invalid mixed release, to the exact target.
func (u *upgrade) normalizePins(goMod string) bool {
	var requires []workspacejob.Module
	if mod, err := u.ReadModFile(goMod); err == nil {
		requires = managedEntries(requiresAsModules(mod.Require))
	}
	for _, require := range requires {
		target, ok := u.targetVersion(require.Path)
		if !ok {
			u.emitMissing(require.Path)
			return false
		}
		if !strings.HasPrefix(require.Version, target+"-") {
			continue
		}
		if u.DryRun {
			u.Emit.Log("info", "Would normalize "+require.Path+" "+require.Version+" to "+target+" in "+goMod)
			continue
		}
		if err := u.Inherit(true, nil, u.GoBinary, "mod", "edit", "-require="+require.Path+"@"+target, goMod); err != nil {
			u.Emit.Log("error", "Cannot normalize "+require.Path+" "+require.Version+" to "+target+" in "+goMod)
			return false
		}
		u.Emit.Log("info", "Normalized "+require.Path+" "+require.Version+" to "+target+" in "+goMod)
	}
	return true
}

func (u *upgrade) reconcileProject(goMod string) bool {
	return u.reconcileExcludes(goMod) && u.normalizePins(goMod)
}

// pinGoWork rewrites every managed go.putnami.dev replace in go.work as one
// block pinned at each module's selected version. Local workspace modules and
// directory replacements stay untouched.
func (u *upgrade) pinGoWork() bool {
	if !u.ensureGoWork() {
		return false
	}
	if !workspacejob.FileExists(u.goWork) {
		return true
	}
	work, err := u.ReadWorkFile(u.goWork)
	if err != nil {
		u.Emit.Log("error", "Cannot parse "+u.goWork)
		return false
	}

	localModules := u.localGoWorkModules(work)
	dirOverrides := dirOverrideModules(work.Replace)
	var modules []string
	for _, module := range u.pinModules(work) {
		if contains(localModules, module) {
			u.Emit.Log("info", "Skipping local workspace module "+module)
			continue
		}
		if contains(dirOverrides, module) {
			u.Emit.Log("info", "Skipping "+module+" (replaced by a local directory)")
			continue
		}
		modules = append(modules, module)
	}
	if len(modules) == 0 {
		return true
	}

	// Fail closed before staging anything.
	targets := make([]string, len(modules))
	for i, module := range modules {
		target, ok := u.targetVersion(module)
		if !ok {
			u.emitMissing(module)
			return false
		}
		targets[i] = target
	}

	if u.DryRun {
		for i, module := range modules {
			u.Emit.Log("info", "Would pin go.work replace "+module+" => "+module+" "+targets[i])
		}
		return true
	}

	// Stage the rewrite in a sibling file so a failure cannot corrupt go.work.
	staged := u.goWork + ".putnami-upgrade." + strconv.Itoa(os.Getpid())
	if err := copyFile(u.goWork, staged); err != nil {
		_ = os.Remove(staged)
		u.Emit.Log("error", "Cannot stage "+u.goWork+" for editing")
		return false
	}

	var drops []string
	for _, module := range modules {
		for _, replace := range work.Replace {
			if replace.Old.Path == module {
				drops = append(drops, "-dropreplace="+replace.Old.Spec())
			}
		}
	}
	if drops = sortedUnique(drops); len(drops) > 0 {
		args := append(append([]string{"work", "edit"}, drops...), staged)
		if err := u.Inherit(false, nil, u.GoBinary, args...); err != nil {
			_ = os.Remove(staged)
			u.Emit.Log("error", "go work edit failed while dropping stale go.putnami.dev replaces")
			return false
		}
	}

	var block strings.Builder
	block.WriteString("\nreplace (\n")
	for i, module := range modules {
		block.WriteString("\t" + module + " => " + module + " " + targets[i] + "\n")
	}
	block.WriteString(")\n")
	if err := appendFile(staged, block.String()); err != nil {
		_ = os.Remove(staged)
		u.Emit.Log("error", "go.work is invalid after pinning go.putnami.dev replaces")
		return false
	}

	if err := u.Inherit(false, nil, u.GoBinary, "work", "edit", "-fmt", staged); err != nil {
		_ = os.Remove(staged)
		u.Emit.Log("error", "go.work is invalid after pinning go.putnami.dev replaces")
		return false
	}
	if err := os.Rename(staged, u.goWork); err != nil {
		_ = os.Remove(staged)
		u.Emit.Log("error", "Cannot write "+u.goWork)
		return false
	}
	for i, module := range modules {
		u.Emit.Log("info", "Pinned go.work replace "+module+" => "+module+" "+targets[i])
	}
	return true
}

// copyFile copies src to dest with the permissions of src, as cp does.
func copyFile(src, dest string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	content, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, content, info.Mode().Perm())
}

func appendFile(path, content string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// propagate rewrites each standalone member's go.putnami.dev requirements to
// the selected versions, so its go.work-less resolutions never ask the proxy
// for a v0.0.0 placeholder. It is a metadata-only rewrite (`go mod edit`,
// never `go get`), so it holds whatever the proxy answers, and it keeps the
// indirect markers and the unrelated requirements.
//
// Modules the workspace supplies locally stay at their placeholder: those
// go.work uses or replaces by a directory, and those the member itself
// replaces by a directory (a fork go.work cannot see).
func (u *upgrade) propagate() bool {
	work, ok := u.readWork()
	if !ok {
		u.Emit.Log("error", "Cannot parse "+u.goWork)
		return false
	}
	skip := sortedUnique(append(u.localGoWorkModules(work), dirOverrideModules(work.Replace)...))

	for _, goMod := range u.standaloneGoMods(work) {
		mod, err := u.ReadModFile(goMod)
		if err != nil {
			continue
		}
		localReplaced := dirOverrideModules(mod.Replace)
		for _, require := range managedEntries(requiresAsModules(mod.Require)) {
			if contains(skip, require.Path) || contains(localReplaced, require.Path) {
				continue
			}
			target, ok := u.targetVersion(require.Path)
			if !ok {
				u.emitMissing(require.Path)
				return false
			}
			if require.Version == target {
				continue
			}
			if u.DryRun {
				u.Emit.Log("info", "Would pin require "+require.Path+" "+target+" in "+goMod)
				continue
			}
			if err := u.Inherit(true, nil, u.GoBinary, "mod", "edit", "-require="+require.Path+"@"+target, goMod); err != nil {
				u.Emit.Log("error", "Cannot pin require "+require.Path+" "+target+" in "+goMod)
				return false
			}
			u.Emit.Log("info", "Pinned require "+require.Path+" "+target+" in "+goMod)
		}
	}
	return true
}

// syncAndReconcile closes the checksum surfaces as one phase. `go work sync`
// is deliberately absent: it would copy the workspace build list into every
// member go.mod and go.sum, turning authored baselines into duplicate lock
// files.
func (u *upgrade) syncAndReconcile() bool {
	u.Emit.PhaseStart("checksums")
	if !u.reconcileWorkspaceChecksums() || !u.reconcileStandaloneChecksums() {
		u.Emit.PhaseEnd("checksums", "failed")
		return false
	}
	u.Emit.PhaseEnd("checksums", "success")
	return true
}

// reconcileWorkspaceChecksums downloads the upgraded workspace graph, which
// writes go.work.sum.
func (u *upgrade) reconcileWorkspaceChecksums() bool {
	if !workspacejob.FileExists(u.goWork) {
		return true
	}
	output, err := u.Combined([]string{"GOWORK=" + u.goWork}, u.GoBinary, "-C", u.WorkspaceRoot, "mod", "download", "all")
	if err != nil {
		u.Emit.Diagnostic("error", "Workspace checksum reconciliation failed (go mod download all): "+output, u.goWork, 0)
		return false
	}
	u.Emit.Log("info", "Reconciled go.work.sum")
	return true
}

// reconcileStandaloneChecksums downloads every standalone member with
// GOWORK=off, so its own go.sum supports publish, standalone builds and
// extension jobs.
func (u *upgrade) reconcileStandaloneChecksums() bool {
	work, ok := u.readWork()
	if !ok {
		u.Emit.Diagnostic("error", "Cannot parse "+u.goWork+" while reconciling checksums", u.goWork, 0)
		return false
	}
	locals := u.workspaceLocalModules(work)
	for _, goMod := range u.standaloneGoMods(work) {
		if u.requiresWorkspaceMode(goMod, locals) {
			u.Emit.Log("info", "Skipping standalone checksum reconciliation for "+goMod+" (requires workspace-local modules)")
			continue
		}
		output, err := u.Combined([]string{"GOWORK=off"}, u.GoBinary, "-C", filepath.Dir(goMod), "mod", "download", "all")
		if err != nil {
			u.Emit.Diagnostic("error", "Standalone checksum reconciliation failed for "+goMod+
				" (GOWORK=off go mod download all): "+output, goMod, 0)
			return false
		}
		u.Emit.Log("info", "Reconciled standalone checksums for "+goMod)
	}
	return true
}
