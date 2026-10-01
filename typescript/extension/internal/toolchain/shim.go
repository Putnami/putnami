package toolchain

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
)

// ErrShimArgument is the error CheckShimArgs returns for an argument that
// cmd.exe would reinterpret on its way to a batch-file shim.
var ErrShimArgument = errors.New("argument cmd.exe would reinterpret")

// cmdMetacharacters are the characters cmd.exe acts on in an argument Go
// leaves unquoted. Go quotes only an argument that holds a space or a tab. A
// shim that passes %* inside a parenthesized block ends that block at a ")". A
// line break ends the command.
const cmdMetacharacters = cmdPrintableMetacharacters + "\r\n"

// cmdQuotedMetacharacters are the cmdMetacharacters cmd.exe still acts on in
// an argument Go quoted. Inside quotes cmd.exe expands %VAR%, and !VAR! under
// delayed expansion. Go escapes a quote with a backslash, which cmd.exe does
// not read as an escape, so a quote ends the quoted part.
const cmdQuotedMetacharacters = "%!\"\r\n"

// cmdPrintableMetacharacters are the cmdMetacharacters an error can print.
const cmdPrintableMetacharacters = "%!^&|<>()\""

// CheckShimArgs returns ErrShimArgument when Windows would start path through
// cmd.exe and cmd.exe would reinterpret path or one of args. Windows starts a
// .cmd or .bat file through cmd.exe, which parses the command line again with
// its own rules; the quotes Go adds protect only some characters from that
// parse. It returns nil on every other OS and for every other file.
func CheckShimArgs(path string, args []string) error {
	return CheckShimArgsOn(runtime.GOOS, path, args)
}

// CheckShimArgsOn is CheckShimArgs for a process that runs on goos.
func CheckShimArgsOn(goos, path string, args []string) error {
	if goos != "windows" || !isBatchFile(path) {
		return nil
	}
	for _, value := range append([]string{path}, args...) {
		live := cmdMetacharacters
		if strings.ContainsAny(value, " \t") {
			live = cmdQuotedMetacharacters
		}
		if i := strings.IndexAny(value, live); i >= 0 {
			return fmt.Errorf("%w: Windows starts %s through cmd.exe, which would interpret the %q in %q; "+
				"install the tool so that a native executable starts it (bun install writes a .exe shim in node_modules\\.bin), "+
				"or keep the characters %s out of the paths it receives",
				ErrShimArgument, path, value[i:i+1], value, cmdPrintableMetacharacters)
		}
	}
	return nil
}

// isBatchFile reports whether path names a Windows batch file, whichever
// separator it uses.
func isBatchFile(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".cmd") || strings.HasSuffix(lower, ".bat")
}
