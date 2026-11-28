package machineproc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The user's setting values.
const (
	SettingOn  = "on"
	SettingOff = "off"
)

// DefaultOn is the machine process's default where neither PUTNAMI_MACHINE
// nor the user's setting says: on, on Linux and macOS (CLI ADR 0116 §12).
// `putnami machine off` or PUTNAMI_MACHINE=off turns it off; a run in CI, a
// hosted run and a nested run run in their own process whatever it says
// (Decide).
const DefaultOn = true

// settingFile is the user's setting under the Putnami home.
const settingFile = "machine.json"

type settingDoc struct {
	Process string `json:"process"`
	// CPUShare is "fair" or "off" (CPUShareFair, CPUShareOff).
	CPUShare string `json:"cpuShare,omitempty"`
}

// The CPU share's values: with "fair", the supervisor shares the machine's
// CPU between the concurrent runs of its workers; with "off", the default,
// each run admits its jobs on its own, as in its own process (CLI ADR 0116
// §5).
const (
	CPUShareFair = "fair"
	CPUShareOff  = "off"
)

// readSettingDoc is the user's setting file, or an empty one when none is set
// or it cannot be read.
func readSettingDoc(getenv func(string) string) settingDoc {
	h, err := home(getenv)
	if err != nil {
		return settingDoc{}
	}
	data, err := os.ReadFile(filepath.Join(h, settingFile))
	if err != nil {
		return settingDoc{}
	}
	var doc settingDoc
	if json.Unmarshal(data, &doc) != nil {
		return settingDoc{}
	}
	return doc
}

// ReadCPUShare is the user's CPU share: CPUShareFair when the setting says so,
// CPUShareOff otherwise.
func ReadCPUShare(getenv func(string) string) string {
	if readSettingDoc(getenv).CPUShare == CPUShareFair {
		return CPUShareFair
	}
	return CPUShareOff
}

// home is the Putnami home: PUTNAMI_HOME, else ~/.putnami.
func home(getenv func(string) string) (string, error) {
	if h := strings.TrimSpace(getenv("PUTNAMI_HOME")); h != "" {
		return h, nil
	}
	user, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(user, ".putnami"), nil
}

// ReadSetting is the user's setting, "on", "off", or "" when none is set or
// it cannot be read.
func ReadSetting(getenv func(string) string) string {
	doc := readSettingDoc(getenv)
	switch doc.Process {
	case SettingOn, SettingOff:
		return doc.Process
	}
	return ""
}

// WriteSetting records the user's setting, "on" or "off", and keeps the rest
// of the setting file.
func WriteSetting(getenv func(string) string, value string) error {
	if value != SettingOn && value != SettingOff {
		return fmt.Errorf("machine: setting %q is neither on nor off", value)
	}
	h, err := home(getenv)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(h, 0o755); err != nil {
		return err
	}
	doc := readSettingDoc(getenv)
	doc.Process = value
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(h, settingFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// errNoDir says no directory fits the supervisor's socket.
var errNoDir = errors.New("machine: no private directory fits the supervisor's socket")

// maxSocketPath is the longest socket path every supported platform accepts:
// macOS holds 104 bytes in sun_path, its terminating NUL included.
const maxSocketPath = 103

// socketName, lockName and descriptionName are the supervisor's files in its
// directory.
const (
	socketName      = "supervisor.sock"
	lockName        = "supervisor.lock"
	descriptionName = "supervisor.json"
	logName         = "supervisor.log"
)

// candidateDirs are the supervisor's directories, in order (CLI ADR 0116
// §2): the per-user runtime directory on Linux, the private temporary
// directory on macOS, then the Putnami home.
func candidateDirs(getenv func(string) string, goos string, uid int) []string {
	var dirs []string
	switch goos {
	case "linux":
		if rt := strings.TrimSpace(getenv("XDG_RUNTIME_DIR")); rt != "" && filepath.IsAbs(rt) {
			dirs = append(dirs, filepath.Join(rt, "putnami"))
		}
	case "darwin":
		if tmp := strings.TrimSpace(getenv("TMPDIR")); tmp != "" && filepath.IsAbs(tmp) {
			dirs = append(dirs, filepath.Join(tmp, fmt.Sprintf("putnami-%d", uid)))
		}
	}
	if h, err := home(getenv); err == nil {
		dirs = append(dirs, filepath.Join(h, "machine"))
	}
	return dirs
}

// isPrivateDir reports whether info is a directory, not a link, that uid
// owns and no one else may write.
func isPrivateDir(info fs.FileInfo, owner uint32, uid int) bool {
	return info.IsDir() && info.Mode()&fs.ModeSymlink == 0 && int(owner) == uid && info.Mode().Perm()&0o077 == 0
}
