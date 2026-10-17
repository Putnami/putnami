package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"

	"go.putnami.dev/tooling/cli/internal/commands/shared"
)

// tidyGoModule runs `go mod tidy` on the Go module in moduleDir as the
// workspace build resolves it.
//
// `go mod tidy` never reads go.work, so a requirement only a go.work replace
// directive satisfies sends it to the module proxy: the v0.0.1 placeholder
// that a generated client requires of each go.putnami.dev module, which go.work
// replaces with the release the workspace pins, is one no proxy serves. tidy
// therefore runs with -modfile on a copy of go.mod that also states each
// go.work replace directive the module does not replace itself. go.mod and
// go.sum take the tidied copy, without those directives, only when tidy
// succeeds.
func tidyGoModule(ctx context.Context, goPath, moduleDir string, env []string) error {
	goModPath := filepath.Join(moduleDir, "go.mod")
	goMod, err := os.ReadFile(goModPath)
	if err != nil {
		return err
	}
	copied, added, err := withGoWorkReplaces(goModPath, goMod, shared.FindGoWork(moduleDir))
	if err != nil {
		return err
	}

	scratch, err := os.MkdirTemp("", "putnami-tidy-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	copyMod := filepath.Join(scratch, "go.mod")
	copySum := filepath.Join(scratch, "go.sum")
	if err := os.WriteFile(copyMod, copied, 0o600); err != nil {
		return err
	}
	if sum, err := os.ReadFile(filepath.Join(moduleDir, "go.sum")); err == nil {
		if err := os.WriteFile(copySum, sum, 0o600); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	if err := runGoInModule(ctx, goPath, moduleDir, env, []string{"mod", "tidy", "-modfile=" + copyMod}); err != nil {
		return err
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
	return writeFileKeepingMode(filepath.Join(moduleDir, "go.sum"), sum)
}

// withGoWorkReplaces returns goMod with each replace directive of the go.work
// at goWorkPath whose module goMod does not replace, and the module versions
// those directives replace. A directory a go.work directive names is relative
// to go.work, so it is stated as an absolute path. Without a go.work, goMod is
// returned as it is.
func withGoWorkReplaces(goModPath string, goMod []byte, goWorkPath string) ([]byte, []module.Version, error) {
	if goWorkPath == "" {
		return goMod, nil, nil
	}
	workData, err := os.ReadFile(goWorkPath)
	if err != nil {
		return nil, nil, err
	}
	work, err := modfile.ParseWork(goWorkPath, workData, nil)
	if err != nil {
		return nil, nil, err
	}
	file, err := modfile.Parse(goModPath, goMod, nil)
	if err != nil {
		return nil, nil, err
	}
	replaced := make(map[string]bool, len(file.Replace))
	for _, replace := range file.Replace {
		replaced[replace.Old.Path] = true
	}
	var added []module.Version
	for _, replace := range work.Replace {
		if replaced[replace.Old.Path] {
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

// goModulePath returns the module path the go.mod in dir declares, or "" when
// dir holds no readable go.mod.
func goModulePath(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return ""
	}
	return modfile.ModulePath(data)
}

// goModuleFiles are the files a Go dependency edit of the module in dir
// writes.
func goModuleFiles(dir string) []string {
	return []string{filepath.Join(dir, "go.mod"), filepath.Join(dir, "go.sum")}
}

// writeFileKeepingMode writes data to path with the permissions path has, or
// 0o644 when it does not exist.
func writeFileKeepingMode(path string, data []byte) error {
	mode := fs.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return os.WriteFile(path, data, mode)
}

// fileSnapshot holds the content of files a command is about to edit, so a
// failed edit puts each one back as it was.
type fileSnapshot struct {
	files []snapshotFile
}

// snapshotFile is one file of a fileSnapshot. A file that did not exist is
// removed again on restore.
type snapshotFile struct {
	path    string
	data    []byte
	mode    fs.FileMode
	existed bool
}

// snapshotFiles records the content of each path.
func snapshotFiles(paths ...string) (*fileSnapshot, error) {
	snapshot := &fileSnapshot{}
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			snapshot.files = append(snapshot.files, snapshotFile{path: path})
			continue
		}
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		snapshot.files = append(snapshot.files, snapshotFile{path: path, data: data, mode: info.Mode().Perm(), existed: true})
	}
	return snapshot, nil
}

// restoreAfter puts every recorded file back as it was and returns cause,
// joined with each file it could not put back.
func (s *fileSnapshot) restoreAfter(cause error) error {
	failures := []error{cause}
	for _, file := range s.files {
		var err error
		if file.existed {
			err = os.WriteFile(file.path, file.data, file.mode)
		} else if removeErr := os.Remove(file.path); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			err = removeErr
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("restore %s: %w", file.path, err))
		}
	}
	return errors.Join(failures...)
}
