//go:build windows

package doctor

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows/registry"
)

// longPathsKey is the registry key that holds LongPathsEnabled.
const longPathsKey = `SYSTEM\CurrentControlSet\Control\FileSystem`

// readLongPathsEnabled reports whether Win32 long paths are on: the
// LongPathsEnabled DWORD under HKLM is 1. An absent value means off, the
// Windows default.
var readLongPathsEnabled = func() (bool, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, longPathsKey, registry.QUERY_VALUE)
	if err != nil {
		return false, fmt.Errorf(`open HKLM\%s: %w`, longPathsKey, err)
	}
	defer func() { _ = key.Close() }()
	value, _, err := key.GetIntegerValue("LongPathsEnabled")
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf(`read HKLM\%s\LongPathsEnabled: %w`, longPathsKey, err)
	}
	return value == 1, nil
}
