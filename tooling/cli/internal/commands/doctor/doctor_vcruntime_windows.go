//go:build windows

package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// vcRuntimeInstalled reports whether the Microsoft Visual C++ runtime is
// installed: vcruntime140.dll is a regular file in the Windows system
// directory. It never loads the DLL, so a copy in the working directory or on
// PATH neither runs nor counts.
var vcRuntimeInstalled = func() (bool, error) {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return false, fmt.Errorf("find the Windows system directory: %w", err)
	}
	info, err := os.Stat(filepath.Join(system, vcRuntimeDLL))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", filepath.Join(system, vcRuntimeDLL), err)
	}
	return info.Mode().IsRegular(), nil
}
