//go:build unix

package credentialcustody

import (
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// fdsProbe reads every descriptor a hostile process holds open. A file or a
// pipe its parent left open is how a credential could reach a child without
// its environment, its arguments or a path the file probes walk.
const fdsProbe = "fds-self"

// descriptorSearchProbes names the descriptor probe, which must find no
// secret.
func descriptorSearchProbes() []string {
	return []string{fdsProbe}
}

// descriptorProbes lists this process's descriptors in /dev/fd and reads each
// one except standard output and error, which only the engine reads: a
// regular file from its start, a pipe or a socket as far as it holds data
// now. It reads without blocking, and leaves every descriptor's offset and
// flags as it found them.
func descriptorProbes(role string) []finding {
	f := finding{Role: role, Probe: fdsProbe}
	names, err := descriptorNames()
	if err != nil {
		f.Detail = "list /dev/fd: " + err.Error()
		return []finding{f}
	}
	f.Checked = true
	foundSet := map[string]bool{}
	kinds := map[string]int{}
	read := 0
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil || fd == 1 || fd == 2 {
			continue
		}
		data, kind := readDescriptor(fd)
		kinds[kind]++
		read += len(data)
		for _, s := range scanBytes(data) {
			foundSet[s] = true
		}
	}
	f.Found = collectFound(foundSet)
	f.Detail = fmt.Sprintf("files=%d streams=%d other=%d closed=%d bytes=%d",
		kinds["file"], kinds["stream"], kinds["other"], kinds["closed"], read)
	return []finding{f}
}

// descriptorNames lists /dev/fd by name alone. Listing it with types would
// stat each entry, and the entry of a descriptor that closed meanwhile, such
// as the one that lists the directory, fails that stat.
func descriptorNames() ([]string, error) {
	dir, err := os.Open("/dev/fd")
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	return dir.Readdirnames(-1)
}

// readDescriptor reads what descriptor fd holds and names its kind: "file",
// "stream" for a pipe or a socket, "other" for a descriptor it does not read,
// such as a directory or a device, and "closed" for one that closed after it
// was listed, such as the descriptor that listed them.
func readDescriptor(fd int) ([]byte, string) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, "closed"
	}
	switch stat.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		return preadDescriptor(fd), "file"
	case unix.S_IFIFO, unix.S_IFSOCK:
		return readAvailable(fd), "stream"
	default:
		return nil, "other"
	}
}

// preadDescriptor reads a regular file from its start, up to
// maxProbeFileSize bytes, without moving its offset.
func preadDescriptor(fd int) []byte {
	var data []byte
	buf := make([]byte, 64<<10)
	for len(data) < maxProbeFileSize {
		n, err := unix.Pread(fd, buf, int64(len(data)))
		if n <= 0 || err != nil {
			break
		}
		data = append(data, buf[:n]...)
	}
	return data
}

// readAvailable reads what a pipe or a socket holds now, up to
// maxProbeFileSize bytes, and then restores its flags.
func readAvailable(fd int) []byte {
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return nil
	}
	if flags&unix.O_NONBLOCK == 0 {
		if err := unix.SetNonblock(fd, true); err != nil {
			return nil
		}
		defer func() { _ = unix.SetNonblock(fd, false) }()
	}
	var data []byte
	buf := make([]byte, 64<<10)
	for len(data) < maxProbeFileSize {
		n, err := unix.Read(fd, buf)
		if n <= 0 || err != nil {
			break
		}
		data = append(data, buf[:n]...)
	}
	return data
}
