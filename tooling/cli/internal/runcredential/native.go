package runcredential

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
)

// NativeHolderError reports a cache provider or credential-provider that a
// hosted run does not start, because it is not its extension's native runtime
// executable started directly.
type NativeHolderError struct {
	// Holder names the process that would receive the credential.
	Holder string
	// Reason says what the holder's command is instead.
	Reason string
}

func (e *NativeHolderError) Error() string {
	return fmt.Sprintf("%s: %s cannot hold the run credential: %s; a hosted run starts a cache provider or "+
		"a credential-provider only as its extension's native runtime executable, command {extensionRuntime}, "+
		"because a script or an interpreted entry reads more files after it starts",
		Flag, e.Holder, e.Reason)
}

// RequireNativeHolder fails with a *NativeHolderError on a hosted run unless
// executable, the file a cache provider or credential-provider starts as, is
// runtime, the runtime executable of the extension that serves it, and that
// file is a native executable image of this system (nativeMagic): a regular
// ELF file on Linux, Mach-O on macOS or PE on Windows, not a script or a
// link. It returns nil without a run credential.
//
// Custody covers the executable a holder starts as, not the files it reads
// later (ADR 0055, part 4). A shell launcher or an interpreted entry, such as
// bun or node with a script, reads more store files after it starts, when a
// repository process may have rewritten them. A native runtime reads none.
// Call it where StartHolder runs start, so that no repository code starts
// between the check and the start.
func RequireNativeHolder(holder, executable, runtime string) error {
	if !Hosted() {
		return nil
	}
	if runtime == "" || filepath.Clean(executable) != filepath.Clean(runtime) {
		return &NativeHolderError{Holder: holder,
			Reason: fmt.Sprintf("its command %s is not its extension's runtime executable", executable)}
	}
	if reason := nativeImage(executable, goruntime.GOOS); reason != "" {
		return &NativeHolderError{Holder: holder, Reason: reason}
	}
	return nil
}

// The first bytes of the executable images a kernel starts without an
// interpreter, by format.
var (
	elfMagic   = [][]byte{{0x7f, 'E', 'L', 'F'}}
	machOMagic = [][]byte{
		// 32-bit and 64-bit, either byte order.
		{0xfe, 0xed, 0xfa, 0xce}, {0xfe, 0xed, 0xfa, 0xcf},
		{0xce, 0xfa, 0xed, 0xfe}, {0xcf, 0xfa, 0xed, 0xfe},
		// Universal.
		{0xca, 0xfe, 0xba, 0xbe}, {0xca, 0xfe, 0xba, 0xbf},
	}
	peMagic = [][]byte{{'M', 'Z'}}
)

// nativeMagic returns the magic numbers of the executable format of goos:
// Mach-O on darwin, PE on windows, and ELF on every other system. A file of
// another format runs there only through a handler, such as a binfmt_misc
// entry for PE or a Java class file on Linux, which is an interpreter that
// reads more files after it starts.
func nativeMagic(goos string) [][]byte {
	switch goos {
	case "darwin":
		return machOMagic
	case "windows":
		return peMagic
	default:
		return elfMagic
	}
}

// nativeImage returns why the file at path is not a native executable image of
// goos, or "" when it is one. A link is not one: it can name an interpreter.
func nativeImage(path, goos string) string {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Sprintf("its runtime executable %s cannot be read: %v", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Sprintf("its runtime executable %s is not a regular file", path)
	}
	file, err := os.Open(path) //nolint:gosec // G304: the runtime executable the CLI prepared and verified
	if err != nil {
		return fmt.Sprintf("its runtime executable %s cannot be read: %v", path, err)
	}
	defer func() { _ = file.Close() }()
	head := make([]byte, 4)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return fmt.Sprintf("its runtime executable %s cannot be read: %v", path, err)
	}
	head = head[:n]
	if bytes.HasPrefix(head, []byte("#!")) {
		return fmt.Sprintf("its runtime executable %s is a script (#!)", path)
	}
	for _, magic := range nativeMagic(goos) {
		if bytes.HasPrefix(head, magic) {
			return ""
		}
	}
	return fmt.Sprintf("its runtime executable %s is not a native executable of %s", path, goos)
}
