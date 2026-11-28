//go:build linux || darwin

package machineproc

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// maxFrame bounds one message: a run request carries the client's
// environment, which stays far below it.
const maxFrame = 4 << 20

// maxFiles bounds the descriptors one message carries: the client's three
// standard streams and the connection a supervisor hands to a worker.
const maxFiles = 4

// conn is one end of a Unix stream socket that carries framed JSON messages,
// each with the descriptors it passes (SCM_RIGHTS).
type conn struct {
	c *net.UnixConn
	// mu serializes the writes of the goroutines that share a control
	// channel.
	mu sync.Mutex
}

// send writes msg as a 4-byte big-endian length and its JSON, with files
// passed alongside its first byte.
func (c *conn) send(msg any, files ...*os.File) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if len(body) > maxFrame {
		return fmt.Errorf("machine: message of %d bytes exceeds %d", len(body), maxFrame)
	}
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame, uint32(len(body)))
	copy(frame[4:], body)
	var oob []byte
	if len(files) > 0 {
		fds := make([]int, len(files))
		for i, f := range files {
			fds[i] = int(f.Fd())
		}
		oob = unix.UnixRights(fds...)
	}
	c.mu.Lock()
	n, oobn, err := c.c.WriteMsgUnix(frame, oob, nil)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if n != len(frame) || oobn != len(oob) {
		return io.ErrShortWrite
	}
	return nil
}

// receive reads one message into msg and returns the descriptors it passed.
// The caller owns them.
func (c *conn) receive(msg any) ([]*os.File, error) {
	head := make([]byte, 4)
	oob := make([]byte, unix.CmsgSpace(maxFiles*4))
	n, oobn, _, _, err := c.c.ReadMsgUnix(head, oob)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, io.EOF
	}
	files, err := parseRights(oob[:oobn])
	if err != nil {
		return nil, err
	}
	if n < 4 {
		if _, err := io.ReadFull(c.c, head[n:]); err != nil {
			closeAll(files)
			return nil, err
		}
	}
	size := binary.BigEndian.Uint32(head)
	if size > maxFrame {
		closeAll(files)
		return nil, fmt.Errorf("machine: message of %d bytes exceeds %d", size, maxFrame)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(c.c, body); err != nil {
		closeAll(files)
		return nil, err
	}
	if err := json.Unmarshal(body, msg); err != nil {
		closeAll(files)
		return nil, err
	}
	return files, nil
}

func parseRights(oob []byte) ([]*os.File, error) {
	if len(oob) == 0 {
		return nil, nil
	}
	messages, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	var files []*os.File
	for i := range messages {
		fds, err := unix.ParseUnixRights(&messages[i])
		if err != nil {
			closeAll(files)
			return nil, err
		}
		for _, fd := range fds {
			unix.CloseOnExec(fd)
			files = append(files, os.NewFile(uintptr(fd), "machine-fd"))
		}
	}
	return files, nil
}

func closeAll(files []*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}

// errPeer refuses a connection from another user.
var errPeer = errors.New("machine: connection from another user")

// unixConn is c as a Unix socket connection.
func unixConn(c net.Conn) (*net.UnixConn, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		_ = c.Close()
		return nil, errors.New("machine: not a Unix socket")
	}
	return uc, nil
}
