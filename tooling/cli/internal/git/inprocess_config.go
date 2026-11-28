package git

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// localConfig is what the in-process reads take from Git's configuration.
type localConfig struct {
	// IgnoreCase is core.ignoreCase: paths and rules compare with ASCII case
	// folded.
	IgnoreCase bool
	// PrecomposeUnicode is core.precomposeUnicode: Git rewrites the names it
	// reads from a directory.
	PrecomposeUnicode bool
	// ExcludesFile is core.excludesFile, when ExcludesFileSet.
	ExcludesFile    string
	ExcludesFileSet bool
	// SHA256 is extensions.objectFormat=sha256.
	SHA256 bool
	// Bare and WorkTree are core.bare=true and a core.worktree.
	Bare, WorkTree bool
	// RemoteURLs are the remote.<name>.url values, the last of each.
	RemoteURLs map[string]string
	// SkipHash is index.skipHash=true.
	SkipHash bool
	// AttributesFile is core.attributesFile, which a diff reads, and
	// AttributesFileSet says the configuration sets it.
	AttributesFile    string
	AttributesFileSet bool
	// Reading is the SHA-256 of Git's whole reading, every entry with its
	// origin: two equal readings configure Git the same way.
	Reading [sha256.Size]byte
}

// parseConfig reads the output of `git config -z --list --show-origin`, run
// in root: for each entry its origin ended by NUL, then its key, a newline
// and its value when it has one, ended by NUL. The last value of a key wins,
// as Git reads a single-valued key. A value it reads and cannot parse is
// unsupported, and so is a repository whose format Git refuses: a format
// version above 1, an unknown extension in version 1, an extension of
// version 1 in version 0. Git reads the format from the repository's own
// config file, repositoryConfig, alone.
//
// It also returns the files the reading depends on, the files the entries
// came from and the files the includes name, whether they exist or not, and
// whether the reading may be kept while they do not change: a value that
// came from no file, or an include whose path it cannot place, is read again
// each time.
func parseConfig(output, root, repositoryConfig string) (*localConfig, []string, bool, error) {
	config := &localConfig{RemoteURLs: map[string]string{}}
	cacheable := true
	seen := map[string]bool{}
	var files []string
	watch := func(file string) {
		if !seen[file] {
			seen[file] = true
			files = append(files, file)
		}
	}
	fields := strings.Split(output, "\x00")
	if len(fields)%2 == 1 && fields[len(fields)-1] == "" {
		fields = fields[:len(fields)-1]
	}
	if len(fields)%2 != 0 {
		return nil, nil, false, unsupported("a git config listing without an origin for each entry")
	}
	version := 0
	var v1Only, unknown []string
	sha256 := false
	for i := 0; i < len(fields); i += 2 {
		origin, entry := fields[i], fields[i+1]
		file, fromFile := strings.CutPrefix(origin, "file:")
		if fromFile {
			if !filepath.IsAbs(file) {
				file = filepath.Join(root, file)
			}
			file = filepath.Clean(file)
			watch(file)
		} else {
			cacheable = false
		}
		rawKey, value, hasValue := strings.Cut(entry, "\n")
		key := strings.ToLower(rawKey)
		repositoryFormat := fromFile && file == filepath.Clean(repositoryConfig)
		var err error
		switch {
		case key == "core.ignorecase":
			config.IgnoreCase, err = parseBool(value, hasValue)
		case key == "core.precomposeunicode":
			config.PrecomposeUnicode, err = parseBool(value, hasValue)
		case key == "core.bare":
			config.Bare, err = parseBool(value, hasValue)
		case key == "core.worktree":
			config.WorkTree = true
		case key == "core.excludesfile":
			if !hasValue {
				return nil, nil, false, unsupported("core.excludesFile without a value")
			}
			config.ExcludesFile, config.ExcludesFileSet = value, true
		case strings.HasPrefix(key, "remote.") && strings.HasSuffix(key, ".url") && len(key) > len("remote..url"):
			// The subsection keeps its case in the listing; the last value
			// is what `git config --get` answers.
			// A key without a value reads as empty.
			config.RemoteURLs[rawKey[len("remote."):len(rawKey)-len(".url")]] = value
		case key == "core.attributesfile":
			if !hasValue {
				return nil, nil, false, unsupported("core.attributesFile without a value")
			}
			config.AttributesFile, config.AttributesFileSet = value, true
		case key == "index.skiphash":
			config.SkipHash, err = parseBool(value, hasValue)
		case key == "core.repositoryformatversion" && repositoryFormat:
			if version, err = strconv.Atoi(value); err != nil {
				err = unsupported("core.repositoryFormatVersion %q", value)
			}
		case strings.HasPrefix(key, "extensions.") && repositoryFormat:
			switch extension := strings.TrimPrefix(key, "extensions."); extension {
			case "noop", "preciousobjects", "partialclone", "worktreeconfig":
			case "noop-v1":
				v1Only = append(v1Only, extension)
			case "objectformat":
				v1Only = append(v1Only, extension)
				switch strings.ToLower(value) {
				case "sha1":
					sha256 = false
				case "sha256":
					sha256 = true
				default:
					return nil, nil, false, unsupported("extensions.objectFormat %q", value)
				}
			default:
				unknown = append(unknown, extension)
			}
		case key == "include.path" || strings.HasPrefix(key, "includeif.") && strings.HasSuffix(key, ".path"):
			target, placed := includeTarget(value, file, fromFile)
			if !placed {
				cacheable = false
				break
			}
			watch(target)
		}
		if err != nil {
			return nil, nil, false, err
		}
	}
	// Git leaves out the configuration of a repository it does not open,
	// such as one of a format it refuses: a repository config that holds
	// something and gave no entry is that refusal.
	if info, err := os.Stat(repositoryConfig); err == nil && info.Size() > 0 && !seen[filepath.Clean(repositoryConfig)] {
		return nil, nil, false, unsupported("git read no entry of %s", repositoryConfig)
	}
	switch {
	case version > 1 || version < 0:
		return nil, nil, false, unsupported("repository format version %d", version)
	case version == 1 && len(unknown) > 0:
		return nil, nil, false, unsupported("the unknown repository extensions %v", unknown)
	case version == 0 && len(unknown) > 0:
		// Git 2.43 ignores them; a later Git refuses the version 1
		// extensions it added since, so the reader leaves it to Git.
		return nil, nil, false, unsupported("the unknown repository extensions %v in a version 0 repository", unknown)
	case version == 0 && len(v1Only) > 0:
		return nil, nil, false, unsupported("the version 1 extensions %v in a version 0 repository", v1Only)
	}
	config.SHA256 = sha256
	if config.SkipHash {
		return nil, nil, false, unsupported("index.skipHash")
	}
	return config, files, cacheable, nil
}

// includeTarget is the file an include path names, as Git resolves it: a
// path under ~/, an absolute path, or a path relative to the directory of
// the file that holds the include. placed is false for any other spelling.
func includeTarget(value, origin string, fromFile bool) (string, bool) {
	if rest, ok := strings.CutPrefix(value, "~/"); ok {
		home := os.Getenv("HOME")
		if home == "" {
			return "", false
		}
		return filepath.Join(home, rest), true
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value), true
	}
	if value == "" || strings.HasPrefix(value, "~") || strings.HasPrefix(value, "%(") || !fromFile {
		return "", false
	}
	return filepath.Join(filepath.Dir(origin), value), true
}

// parseBool reads a boolean as Git's git_parse_maybe_bool does, then as an
// integer.
func parseBool(value string, hasValue bool) (bool, error) {
	if !hasValue {
		return true, nil
	}
	switch strings.ToLower(value) {
	case "true", "yes", "on":
		return true, nil
	case "false", "no", "off", "":
		return false, nil
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return false, unsupported("a boolean %q", value)
	}
	return number != 0, nil
}

// excludesFile is the path of the global ignore file Git reads: the
// core.excludesFile, else $XDG_CONFIG_HOME/git/ignore, else
// $HOME/.config/git/ignore; "" when there is none.
func (c *localConfig) excludesFile() (string, error) {
	if c.ExcludesFileSet {
		value := c.ExcludesFile
		if value == "" {
			return "", unsupported("an empty core.excludesFile")
		}
		if rest, ok := strings.CutPrefix(value, "~/"); ok {
			home := os.Getenv("HOME")
			if home == "" {
				return "", unsupported("core.excludesFile under an unset HOME")
			}
			return filepath.Join(home, rest), nil
		}
		if !filepath.IsAbs(value) {
			return "", unsupported("a core.excludesFile that is not absolute")
		}
		return value, nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "git", "ignore"), nil
	}
	if home, ok := os.LookupEnv("HOME"); ok {
		return filepath.Join(home, ".config", "git", "ignore"), nil
	}
	return "", nil
}
