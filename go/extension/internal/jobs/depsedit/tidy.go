package depsedit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

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
// build resolves it.
//
// `go mod tidy` never reads go.work, so a requirement only a go.work replace
// directive satisfies sends it to the module proxy: the v0.0.1 placeholder a
// generated client requires of each go.putnami.dev module, which go.work
// replaces with the release the workspace pins, is one no proxy serves. tidy
// therefore runs with -modfile on a copy of go.mod that also states each
// go.work replace directive the module does not replace itself. go.mod and
// go.sum take the tidied copy, without those directives, only when tidy
// succeeds.
func tidyModule(j *workspacejob.Job, dir string) error {
	goModPath := filepath.Join(dir, "go.mod")
	goMod, err := os.ReadFile(goModPath)
	if err != nil {
		return err
	}
	copied, added, err := withGoWorkReplaces(goModPath, goMod, toolchain.FindGoWork(dir))
	if err != nil {
		return err
	}

	scratch, err := os.MkdirTemp("", "putnami-tidy-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	copyMod := filepath.Join(scratch, "go.mod")
	copySum := filepath.Join(scratch, "go.sum")
	if err := os.WriteFile(copyMod, copied, 0o600); err != nil {
		return err
	}
	if sum, err := os.ReadFile(filepath.Join(dir, "go.sum")); err == nil {
		if err := os.WriteFile(copySum, sum, 0o600); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	if out, err := j.Combined(goWorkEnv(dir), j.GoBinary, "-C", dir, "mod", "tidy", "-modfile="+copyMod); err != nil {
		return fmt.Errorf("%w%s", err, workspacejob.OutputExcerpt(out))
	}

	tidied, err := os.ReadFile(copyMod)
	if err != nil {
		return err
	}
	if tidied, err = withoutReplaces(copyMod, tidied, added); err != nil {
		return err
	}
	if err := writeFileKeepingMode(goModPath, tidied); err != nil {
		return err
	}
	sum, err := os.ReadFile(copySum)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return writeFileKeepingMode(filepath.Join(dir, "go.sum"), sum)
}

// withGoWorkReplaces returns goMod with each replace directive of the go.work
// at goWorkPath for a module version goMod does not replace itself, and the
// module versions those directives replace (replacesVersion). A directory a
// go.work directive names is relative to go.work, so it is stated as an
// absolute path. When go.work states no replace directive, or when go.work or
// goMod is a file this parser does not read, goMod is returned as it is and
// tidy runs on it unchanged.
func withGoWorkReplaces(goModPath string, goMod []byte, goWorkPath string) ([]byte, []module.Version, error) {
	if goWorkPath == "" {
		return goMod, nil, nil
	}
	workData, err := os.ReadFile(goWorkPath)
	if err != nil {
		return nil, nil, err
	}
	work, err := modfile.ParseWork(goWorkPath, workData, nil)
	if err != nil || len(work.Replace) == 0 {
		return goMod, nil, nil
	}
	file, err := modfile.Parse(goModPath, goMod, nil)
	if err != nil {
		return goMod, nil, nil
	}
	var added []module.Version
	for _, replace := range work.Replace {
		if replacesVersion(file.Replace, replace.Old) {
			continue
		}
		target := replace.New.Path
		if replace.New.Version == "" && !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(goWorkPath), filepath.FromSlash(target))
		}
		if err := file.AddReplace(replace.Old.Path, replace.Old.Version, target, replace.New.Version); err != nil {
			return nil, nil, fmt.Errorf("state the go.work replace of %s: %w", replace.Old.Path, err)
		}
		added = append(added, replace.Old)
	}
	if len(added) == 0 {
		return goMod, nil, nil
	}
	file.Cleanup()
	formatted, err := file.Format()
	if err != nil {
		return nil, nil, err
	}
	return formatted, added, nil
}

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
