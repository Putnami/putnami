package git

import (
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strings"
)

// LFPolicyAttributes is the .gitattributes rule a new workspace carries
// (decision D-W4): Git stores text with LF endings and checks it out with LF
// on every platform, so a Windows checkout hashes the same bytes as a Linux one.
const LFPolicyAttributes = "* text=auto eol=lf"

// CRLFCheckouts lists the tracked text files under dir whose working-tree copy
// has CRLF (or mixed) line endings while the index copy has LF: the files a
// line-ending conversion on checkout rewrote. A file whose attributes set
// eol=crlf is left out: the workspace asks for CRLF there, and Git checks it
// out with CRLF on every platform, so every checkout holds the same bytes.
// Paths are relative to dir, in slash form, sorted. It fails when dir is not
// inside a Git working tree.
func CRLFCheckouts(dir string) ([]string, error) {
	out, err := run(dir, "ls-files", "--eol", "-z")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, record := range strings.Split(out, "\x00") {
		// Each record is "i/<eol> w/<eol> attr/<attrs>\t<path>", where
		// <attrs> is empty or holds space-separated attributes such as
		// "text=auto eol=crlf".
		info, path, ok := strings.Cut(record, "\t")
		if !ok || path == "" {
			continue
		}
		eols, attrs, _ := strings.Cut(info, "attr/")
		fields := strings.Fields(eols)
		if len(fields) < 2 || fields[0] != "i/lf" {
			continue
		}
		if fields[1] != "w/crlf" && fields[1] != "w/mixed" {
			continue
		}
		if slices.Contains(strings.Fields(attrs), "eol=crlf") {
			continue
		}
		files = append(files, path)
	}
	sort.Strings(files)
	return files, nil
}

// ConfigValue returns the value Git resolves for key in dir, and whether the
// key is set at all. An unset key is not an error.
func ConfigValue(dir, key string) (value string, set bool, err error) {
	stdout, stderr, err := runCapture(dir, "config", "--get", key)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", false, nil
		}
		return "", false, fmt.Errorf("git config (in %s): %w: %s", dir, err, strings.TrimSpace(stderr))
	}
	return strings.TrimSpace(stdout), true, nil
}

// ConfigBool reports whether a Git configuration value means true under Git's
// boolean rules (true, yes, on, 1, case-insensitive).
func ConfigBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "yes", "on", "1":
		return true
	default:
		return false
	}
}

// HasLFPolicy reports whether the attributes Git applies to path (relative to
// dir) check it out with LF endings, whichever .gitattributes file sets them.
func HasLFPolicy(dir, path string) (bool, error) {
	out, err := run(dir, "check-attr", "eol", "--", path)
	if err != nil {
		return false, err
	}
	// The output is "<path>: eol: <value>".
	return strings.TrimSpace(out[strings.LastIndex(out, ":")+1:]) == "lf", nil
}
