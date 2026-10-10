package clicore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
)

// ReadAuth loads the stored credential from ~/.putnami/auth.json. With required
// set, a missing file or empty access token is a not-authenticated error;
// otherwise a missing file returns (nil, nil).
func ReadAuth(env map[string]string, required bool) (*StoredToken, error) {
	data, err := os.ReadFile(authPath(env))
	if err != nil {
		if os.IsNotExist(err) && !required {
			return nil, nil
		}
		if os.IsNotExist(err) {
			return nil, NewError("not authenticated; run putnami cloud login", ExitAuth)
		}
		return nil, err
	}
	var auth StoredToken
	if err := json.Unmarshal(data, &auth); err != nil {
		return nil, err
	}
	if required && auth.AccessToken == "" {
		return nil, NewError("not authenticated; run putnami cloud login", ExitAuth)
	}
	return &auth, nil
}

// WriteAuth persists auth atomically (temp file + rename) with 0600 perms. It
// keeps every top-level member of the file it replaces that StoredToken does
// not declare: the Cloud and Intelligence CLIs share auth.json, so a member one
// of them adds survives the other's token refresh.
func WriteAuth(auth *StoredToken, env map[string]string) error {
	file := authPath(env)
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return err
	}
	if previous, readErr := os.ReadFile(file); readErr == nil {
		if data, err = withForeignAuthMembers(data, previous); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(dir, ".auth-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, file); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Chmod(file, 0o600)
}

// RemoveAuth deletes the stored credential, treating absence as success.
func RemoveAuth(env map[string]string) error {
	err := os.Remove(authPath(env))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// withForeignAuthMembers adds to data, a marshaled StoredToken, the top-level
// members of previous that StoredToken does not declare. A declared member
// that data omits stays omitted. previous that is not a JSON object adds
// nothing.
func withForeignAuthMembers(data, previous []byte) ([]byte, error) {
	var old map[string]json.RawMessage
	if json.Unmarshal(previous, &old) != nil {
		return data, nil
	}
	declared := storedTokenMembers()
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(data, &merged); err != nil {
		return nil, err
	}
	added := false
	for name, value := range old {
		if declared[name] {
			continue
		}
		if _, ok := merged[name]; !ok {
			merged[name] = value
			added = true
		}
	}
	if !added {
		return data, nil
	}
	return json.MarshalIndent(merged, "", "  ")
}

// storedTokenMembers names the JSON members StoredToken declares.
func storedTokenMembers() map[string]bool {
	t := reflect.TypeFor[StoredToken]()
	names := make(map[string]bool, t.NumField())
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			names[name] = true
		}
	}
	return names
}

func authPath(env map[string]string) string {
	return filepath.Join(PutnamiHome(env), "auth.json")
}

// PutnamiHome resolves the Putnami home directory: PUTNAMI_HOME when set,
// otherwise .putnami in the user home directory (see UserHomeDirFor). On Windows
// that is %USERPROFILE%\.putnami, the directory the Putnami CLI uses.
func PutnamiHome(env map[string]string) string {
	return putnamiHome(env, runtime.GOOS)
}

func putnamiHome(env map[string]string, goos string) string {
	if home := EnvGet(env, "PUTNAMI_HOME"); home != "" {
		return home
	}
	if home := UserHomeDirFor(env, goos); home != "" {
		return filepath.Join(home, ".putnami")
	}
	return ".putnami"
}

// UserHomeDirFor returns the user home directory on goos, a GOOS value, by the
// rule of os.UserHomeDir, reading the variable from env: USERPROFILE on
// Windows, home on Plan 9, HOME elsewhere. HOME is never read on Windows. When
// env lacks the variable, it returns os.UserHomeDir(), or "" when that fails.
func UserHomeDirFor(env map[string]string, goos string) string {
	if home := EnvGet(env, userHomeEnv(goos)); home != "" {
		return home
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}

// userHomeEnv names the variable os.UserHomeDir reads on goos.
func userHomeEnv(goos string) string {
	switch goos {
	case "windows":
		return "USERPROFILE"
	case "plan9":
		return "home"
	default:
		return "HOME"
	}
}
