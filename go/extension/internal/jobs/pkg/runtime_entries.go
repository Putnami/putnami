package pkg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/sdk/extension/pkgmeta"
)

// validateStagedRuntimeEntries is the archive-time half of the runtime
// entries contract (protocol/extension runtime.entries): a platform archive
// for goos must carry each entry's executable, as a regular file under its
// file name for goos, with no symbolic link or non-directory on its path,
// exactly as validateStagedRuntimeExecutable requires of runtime.executable.
// A task that names an entry runs it, so an archive without it would fail that
// task in every consumer workspace. It returns the files' slash-separated
// archive paths, sorted, so the archive marks each one executable.
func validateStagedRuntimeEntries(stageDir, goos string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(stageDir, "putnami.extension.json"))
	if err != nil {
		return nil, fmt.Errorf("read staged extension manifest: %w", err)
	}
	var manifest struct {
		Runtime *struct {
			Entries map[string]struct {
				Executable string `json:"executable"`
			} `json:"entries"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse staged extension manifest: %w", err)
	}
	if manifest.Runtime == nil || len(manifest.Runtime.Entries) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(manifest.Runtime.Entries))
	for name := range manifest.Runtime.Entries {
		names = append(names, name)
	}
	sort.Strings(names)
	paths := make([]string, 0, len(names))
	for _, entry := range names {
		declared := filepath.FromSlash(manifest.Runtime.Entries[entry].Executable)
		if declared == "" || !filepath.IsLocal(declared) || filepath.Clean(declared) == "." {
			return nil, fmt.Errorf("staged extension runtime entry %q executable %q is not a local archive path", entry, manifest.Runtime.Entries[entry].Executable)
		}
		executable := filepath.Clean(filepath.FromSlash(pkgmeta.ExecutableName(goos, manifest.Runtime.Entries[entry].Executable)))
		current := stageDir
		components := strings.Split(executable, string(filepath.Separator))
		for index, component := range components {
			current = filepath.Join(current, component)
			info, err := os.Lstat(current)
			if err != nil {
				return nil, fmt.Errorf("staged extension runtime entry %q executable %q is unavailable: %w", entry, filepath.ToSlash(executable), err)
			}
			last := index == len(components)-1
			if info.Mode()&os.ModeSymlink != 0 || (!last && !info.IsDir()) || (last && !info.Mode().IsRegular()) {
				return nil, fmt.Errorf("staged extension runtime entry %q executable %q is not a regular file under real directories", entry, filepath.ToSlash(executable))
			}
		}
		paths = append(paths, filepath.ToSlash(executable))
	}
	return paths, nil
}

// appendMissing appends to list each of values it does not hold yet.
func appendMissing(list []string, values ...string) []string {
	for _, value := range values {
		found := false
		for _, held := range list {
			if held == value {
				found = true
				break
			}
		}
		if !found {
			list = append(list, value)
		}
	}
	return list
}
