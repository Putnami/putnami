package depsedit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"

	"go.putnami.dev/go/extension/internal/toolchain"
	"go.putnami.dev/go/extension/internal/workspacejob"
)

// goWorkEnv points GOWORK at the go.work that governs dir, so a go command
// that reads go.work resolves the workspace's use and replace directives and
// an inherited GOWORK=off cannot force single-module resolution.
func goWorkEnv(dir string) []string {
	return []string{"GOWORK=" + toolchain.FindGoWork(dir)}
}

// tidyModule runs `go mod tidy` on the Go module in dir as the workspace
// build resolves it, and writes the result to goModPath and the go.sum beside
// it: the module's own go.mod, or the copy a dry run edits (planEdit).
//
// `go mod tidy` never reads go.work, so a requirement only a go.work replace
// directive satisfies sends it to the module proxy: the v0.0.1 placeholder a
// generated client requires of each go.putnami.dev module, which go.work
// replaces with the release the workspace pins, is one no proxy serves. tidy
// therefore runs with -modfile on a copy of go.mod that also states the
// go.work replace directives the module does not replace itself
// (withGoWorkReplaces). go.mod and go.sum take the tidied copy, without those
// directives, only when tidy succeeds. It returns one line per go.work pin
// the tidy read for a version no proxy serves, which the job reports.
func tidyModule(j *workspacejob.Job, dir, goModPath string) ([]string, error) {
	goSumPath := filepath.Join(filepath.Dir(goModPath), "go.sum")
	goMod, err := os.ReadFile(goModPath)
	if err != nil {
		return nil, err
	}
	unserved := func(candidates []module.Version) ([]module.Version, error) {
		return unservedVersions(j, dir, candidates)
	}
	copied, added, pins, err := withGoWorkReplaces(dir, goModPath, goMod, toolchain.FindGoWork(dir), unserved)
	if err != nil {
		return nil, fmt.Errorf("state the go.work replace directives: %w", err)
	}

	scratch, err := os.MkdirTemp("", "putnami-tidy-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	copyMod := filepath.Join(scratch, "go.mod")
	copySum := filepath.Join(scratch, "go.sum")
	if err := os.WriteFile(copyMod, copied, 0o600); err != nil {
		return nil, err
	}
	if sum, err := os.ReadFile(goSumPath); err == nil {
		if err := os.WriteFile(copySum, sum, 0o600); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	if out, err := j.Combined(goWorkEnv(dir), j.GoBinary, "-C", dir, "mod", "tidy", "-modfile="+copyMod); err != nil {
		return nil, fmt.Errorf("go mod tidy: %w%s", err, workspacejob.OutputExcerpt(out))
	}

	tidied, err := os.ReadFile(copyMod)
	if err != nil {
		return nil, err
	}
	if tidied, err = withoutReplaces(copyMod, tidied, added); err != nil {
		return nil, err
	}
	if err := writeFileKeepingMode(goModPath, tidied); err != nil {
		return nil, err
	}
	sum, err := os.ReadFile(copySum)
	if errors.Is(err, fs.ErrNotExist) {
		return pins, nil
	}
	if err != nil {
		return nil, err
	}
	return pins, writeFileKeepingMode(goSumPath, sum)
}

// withGoWorkReplaces returns goMod, the go.mod of the module in dir read from
// goModPath, with the replace directives of the go.work at goWorkPath for
// module versions goMod does not replace itself, and the
// module versions those directives replace (replacesVersion). A directory a
// go.work directive names is relative to go.work, so it is stated as an
// absolute path.
//
// A directive that pins every version of a module to a module version, such as
// `go.putnami.dev/app => go.putnami.dev/app v0.4.0`, is stated only for the
// versions of its module a workspace go.mod requires (requiredVersions) and
// unserved reports no proxy serves, one directive per version; pins names each
// of them. Every other directive is stated as written.
//
// When go.work states no replace directive, or when go.work or goMod is a file
// this parser does not read, goMod is returned as it is and tidy runs on it
// unchanged.
func withGoWorkReplaces(dir, goModPath string, goMod []byte, goWorkPath string,
	unserved func([]module.Version) ([]module.Version, error)) ([]byte, []module.Version, []string, error) {
	if goWorkPath == "" {
		return goMod, nil, nil, nil
	}
	workData, err := os.ReadFile(goWorkPath)
	if err != nil {
		return nil, nil, nil, err
	}
	work, err := modfile.ParseWork(goWorkPath, workData, nil)
	if err != nil || len(work.Replace) == 0 {
		return goMod, nil, nil, nil
	}
	file, err := modfile.Parse(goModPath, goMod, nil)
	if err != nil {
		return goMod, nil, nil, nil
	}
	var added []module.Version
	state := func(old module.Version, replace *modfile.Replace) error {
		target := replace.New.Path
		if replace.New.Version == "" && !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(goWorkPath), filepath.FromSlash(target))
		}
		if err := file.AddReplace(old.Path, old.Version, target, replace.New.Version); err != nil {
			return fmt.Errorf("state the go.work replace of %s: %w", old.Path, err)
		}
		added = append(added, old)
		return nil
	}
	pins := map[string]*modfile.Replace{}
	for _, replace := range work.Replace {
		if replacesVersion(file.Replace, replace.Old) {
			continue
		}
		if replace.Old.Version == "" && replace.New.Version != "" {
			pins[replace.Old.Path] = replace
			continue
		}
		if err := state(replace.Old, replace); err != nil {
			return nil, nil, nil, err
		}
	}
	var stated []string
	if len(pins) > 0 {
		candidates := requiredVersions(dir, file, goWorkPath, work, pins)
		var pinned []module.Version
		if len(candidates) > 0 {
			if pinned, err = unserved(candidates); err != nil {
				return nil, nil, nil, err
			}
		}
		for _, old := range pinned {
			pin := pins[old.Path]
			if err := state(old, pin); err != nil {
				return nil, nil, nil, err
			}
			stated = append(stated, fmt.Sprintf("go.work pins %s to %s %s: the tidy reads that release for %s %s, which no proxy serves, and applies its requirements",
				old.Path, pin.New.Path, pin.New.Version, old.Path, old.Version))
		}
	}
	if len(added) == 0 {
		return goMod, nil, nil, nil
	}
	file.Cleanup()
	formatted, err := file.Format()
	if err != nil {
		return nil, nil, nil, err
	}
	return formatted, added, stated, nil
}

// requiredVersions returns, sorted, each version of a module of pins that a
// workspace go.mod requires: file, the go.mod of the module in dir, the go.mod
// of each use directive of work, and the go.mod of each directory a replace
// directive of work or file names.
// A go.mod that is missing or does not parse requires none.
func requiredVersions(dir string, file *modfile.File, goWorkPath string, work *modfile.WorkFile, pins map[string]*modfile.Replace) []module.Version {
	seen := map[module.Version]bool{}
	collect := func(requires []*modfile.Require) {
		for _, require := range requires {
			if _, ok := pins[require.Mod.Path]; ok {
				seen[require.Mod] = true
			}
		}
	}
	collect(file.Require)
	workDir := filepath.Dir(goWorkPath)
	var dirs []string
	for _, use := range work.Use {
		dirs = append(dirs, filepath.Join(workDir, filepath.FromSlash(use.Path)))
	}
	for _, replace := range work.Replace {
		if replace.New.Version == "" {
			dirs = append(dirs, filepath.Join(workDir, filepath.FromSlash(replace.New.Path)))
		}
	}
	for _, replace := range file.Replace {
		if replace.New.Version == "" {
			dirs = append(dirs, filepath.Join(dir, filepath.FromSlash(replace.New.Path)))
		}
	}
	for _, member := range dirs {
		goModPath := filepath.Join(member, "go.mod")
		data, err := os.ReadFile(goModPath)
		if err != nil {
			continue
		}
		if parsed, err := modfile.Parse(goModPath, data, nil); err == nil {
			collect(parsed.Require)
		}
	}
	versions := make([]module.Version, 0, len(seen))
	for version := range seen {
		versions = append(versions, version)
	}
	module.Sort(versions)
	return versions
}

// unservedVersions returns the module versions of candidates no proxy serves,
// from one `go list -m -e` in dir, which reports each version it cannot fetch
// in its Error member. A version is unserved only when the answer is that it
// does not exist (notFound); any other lookup error, such as GOPROXY=off or an
// unreachable proxy, fails with the module version and the go error.
// GOWORK=off and -mod=readonly keep the query out of the workspace build list
// and keep go list from writing go.mod or go.sum, as in resolveModules.
func unservedVersions(j *workspacejob.Job, dir string, candidates []module.Version) ([]module.Version, error) {
	args := make([]string, 0, 7+len(candidates))
	args = append(args, "-C", dir, "list", "-m", "-e", "-json", "-mod=readonly")
	for _, candidate := range candidates {
		args = append(args, candidate.Path+"@"+candidate.Version)
	}
	out, errOut, err := j.Split([]string{"GOWORK=off"}, j.GoBinary, args...)
	if err != nil {
		return nil, fmt.Errorf("go list -m: %w%s", err, workspacejob.OutputExcerpt(errOut))
	}
	var unserved []module.Version
	for decoder := json.NewDecoder(strings.NewReader(out)); decoder.More(); {
		var listed struct {
			Path    string
			Version string
			Error   *struct{ Err string }
		}
		if err := decoder.Decode(&listed); err != nil {
			return nil, fmt.Errorf("go list -m: read the go list output: %w", err)
		}
		if listed.Error == nil {
			continue
		}
		if !notFound(listed.Error.Err) {
			return nil, fmt.Errorf("go list -m: cannot tell whether a proxy serves %s@%s: %s", listed.Path, listed.Version, listed.Error.Err)
		}
		unserved = append(unserved, module.Version{Path: listed.Path, Version: listed.Version})
	}
	return unserved, nil
}

// notFound reports whether a go module lookup error says the version does not
// exist: a 404 or 410 status of a proxy, whatever reason phrase follows it, a
// missing file of a file:// proxy, or an unknown revision of a direct fetch.
func notFound(message string) bool {
	message = strings.ToLower(message)
	if notFoundStatus.MatchString(message) {
		return true
	}
	for _, answer := range []string{"no such file or directory", "cannot find the file specified", "cannot find the path specified", "unknown revision"} {
		if strings.Contains(message, answer) {
			return true
		}
	}
	return false
}

// notFoundStatus matches the HTTP status a go lookup error states right after
// `reading <url>: ` when it is 404 or 410, not a status a server response
// quotes later.
var notFoundStatus = regexp.MustCompile(`reading \S+: (404|410)(\s|$)`)

// replacesVersion reports whether a directive of replaces covers some version
// old covers: a directive for every version of old's module, a directive for
// one version when old covers every version, or a directive for old's version.
func replacesVersion(replaces []*modfile.Replace, old module.Version) bool {
	for _, replace := range replaces {
		if replace.Old.Path != old.Path {
			continue
		}
		if replace.Old.Version == "" || old.Version == "" || replace.Old.Version == old.Version {
			return true
		}
	}
	return false
}

// withoutReplaces returns goMod without the replace directives of the module
// versions in drop.
func withoutReplaces(goModPath string, goMod []byte, drop []module.Version) ([]byte, error) {
	if len(drop) == 0 {
		return goMod, nil
	}
	file, err := modfile.Parse(goModPath, goMod, nil)
	if err != nil {
		return nil, err
	}
	for _, old := range drop {
		if err := file.DropReplace(old.Path, old.Version); err != nil {
			return nil, err
		}
	}
	file.Cleanup()
	return file.Format()
}

// writeFileKeepingMode writes data to path with the permissions path has, or
// 0o644 when it does not exist.
func writeFileKeepingMode(path string, data []byte) error {
	mode := fs.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return workspacejob.WriteFileAtomically(path, data, mode)
}
