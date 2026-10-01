package logger

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// bannedSlogCall matches package-qualified stdlib slog *logging* calls
// (slog.Debug/Info/Warn/Error) and slog.Default(). Type/attr references such as
// slog.Attr, slog.Level, slog.String, or slog.Int remain legitimate and are not
// matched. The leading \b anchors on a word boundary so identifiers like
// myslog.Warn do not trip the guard.
var bannedSlogCall = regexp.MustCompile(`\bslog\.(Debug|Info|Warn|Error|Default)\b`)

// TestNoStdlibSlogLoggingInFramework walks the go/framework source tree and fails
// if any package other than go/framework/logger itself makes a package-level
// stdlib slog logging call. Those calls bypass the framework logger's JSON sink,
// LOG_LEVEL config, GCP severity mapping, buffer/OTLP sinks, and trace
// correlation, so the whole class is banned. Framework code must log through
// go.putnami.dev/logger instead. Test files are included in the scan — they must
// not call package-level slog logging either.
func TestNoStdlibSlogLoggingInFramework(t *testing.T) {
	// The logger package itself is the only exemption: it legitimately wraps
	// log/slog. Everything else under go/framework is guarded.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the logger package directory")
	}
	loggerPkgDir := filepath.Dir(thisFile)
	frameworkRoot := filepath.Dir(loggerPkgDir) // go/framework

	var offenders []string
	err := filepath.Walk(frameworkRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Skip the logger package (the sole exemption) and non-source dirs.
			name := info.Name()
			if path == loggerPkgDir || name == ".gen" || name == "testdata" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		content, readErr := os.ReadFile(path) //nolint:gosec // G304: path is derived from a fixed in-repo walk root, not user input.
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(content), "\n") {
			if bannedSlogCall.MatchString(line) {
				rel, relErr := filepath.Rel(frameworkRoot, path)
				if relErr != nil {
					rel = path
				}
				offenders = append(offenders, rel+":"+strconv.Itoa(i+1)+"  "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking framework tree: %v", err)
	}

	if len(offenders) > 0 {
		t.Fatalf("stdlib slog logging calls are banned outside go/framework/logger; "+
			"log through go.putnami.dev/logger instead. Offending sites:\n%s",
			strings.Join(offenders, "\n"))
	}
}
