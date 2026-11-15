// Package screens serves putnami change screenshots: it selects the pages a
// change touches in a web project, captures them through a headless Chromium
// driven over the DevTools pipe, and reads and checks the manifest of the
// captures, which change finalize asks for.
package screens

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// maxMessageBytes bounds one DevTools message. A full-page PNG screenshot of
// a long page is the largest message the command receives.
const maxMessageBytes = 256 << 20

// cdpError is an error the browser answered to a call.
type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string { return fmt.Sprintf("%s (%d)", e.Message, e.Code) }

// message is one DevTools message in either direction: a call, its answer, or
// an event.
type message struct {
	ID        int64           `json:"id,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
}

// event is one DevTools event: its method, the session it belongs to, and its
// parameters.
type event struct {
	SessionID string
	Method    string
	Params    json.RawMessage
}

// client speaks the DevTools protocol over the pipe transport: each message
// is one JSON document followed by a NUL byte, written to the browser's
// input and read from its output.
type client struct {
	w io.Writer

	writeMu sync.Mutex
	mu      sync.Mutex
	next    int64
	pending map[int64]chan message
	// listeners receive every event; a listener that returns true is removed.
	listeners map[int64]func(event) bool
	nextL     int64
	err       error
	done      chan struct{}
}

// newClient starts reading r and returns the client that writes to w. The
// reader stops at the end of r or at its first malformed message; every
// pending and later call then fails with that cause.
func newClient(r io.Reader, w io.Writer) *client {
	c := &client{w: w, pending: map[int64]chan message{}, listeners: map[int64]func(event) bool{}, done: make(chan struct{})}
	go c.read(r)
	return c
}

func (c *client) read(r io.Reader) {
	reader := bufio.NewReaderSize(r, 1<<16)
	var cause error
	for {
		data, err := readFrame(reader)
		if err != nil {
			cause = err
			break
		}
		var msg message
		if err := json.Unmarshal(data, &msg); err != nil {
			cause = fmt.Errorf("the browser wrote a message that is not JSON: %w", err)
			break
		}
		c.dispatch(msg)
	}
	if errors.Is(cause, io.EOF) {
		cause = errors.New("the browser closed its DevTools pipe")
	}
	c.mu.Lock()
	c.err = cause
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	close(c.done)
}

// readFrame reads one NUL-terminated message of at most maxMessageBytes.
func readFrame(reader *bufio.Reader) ([]byte, error) {
	var data []byte
	for {
		chunk, err := reader.ReadSlice(0)
		data = append(data, chunk...)
		if len(data) > maxMessageBytes {
			return nil, fmt.Errorf("a DevTools message exceeds %d bytes", maxMessageBytes)
		}
		if err == nil {
			return data[:len(data)-1], nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			if len(data) > 0 && errors.Is(err, io.EOF) {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
	}
}

func (c *client) dispatch(msg message) {
	if msg.ID != 0 {
		c.mu.Lock()
		ch, ok := c.pending[msg.ID]
		delete(c.pending, msg.ID)
		c.mu.Unlock()
		if ok {
			ch <- msg
		}
		return
	}
	if msg.Method == "" {
		return
	}
	ev := event{SessionID: msg.SessionID, Method: msg.Method, Params: msg.Params}
	c.mu.Lock()
	listeners := make([]int64, 0, len(c.listeners))
	for id := range c.listeners {
		listeners = append(listeners, id)
	}
	c.mu.Unlock()
	for _, id := range listeners {
		c.mu.Lock()
		listener, ok := c.listeners[id]
		c.mu.Unlock()
		if ok && listener(ev) {
			c.mu.Lock()
			delete(c.listeners, id)
			c.mu.Unlock()
		}
	}
}

// listen registers fn for every later event until it returns true or the
// returned function removes it. fn runs on the reader goroutine and must not
// call the browser.
func (c *client) listen(fn func(event) bool) (remove func()) {
	c.mu.Lock()
	c.nextL++
	id := c.nextL
	c.listeners[id] = fn
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.listeners, id)
		c.mu.Unlock()
	}
}

// call sends method with params in session, an empty session for the browser
// target, and decodes the answer into result when result is not nil.
func (c *client) call(ctx context.Context, session, method string, params, result any) error {
	var raw json.RawMessage
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			return err
		}
		raw = data
	}
	ch := make(chan message, 1)
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return fmt.Errorf("%s: %w", method, err)
	}
	c.next++
	id := c.next
	c.pending[id] = ch
	c.mu.Unlock()
	data, err := json.Marshal(message{ID: id, SessionID: session, Method: method, Params: raw})
	if err == nil {
		c.writeMu.Lock()
		_, err = c.w.Write(append(data, 0))
		c.writeMu.Unlock()
	}
	if err != nil {
		c.forget(id)
		return fmt.Errorf("%s: %w", method, err)
	}
	select {
	case <-ctx.Done():
		c.forget(id)
		return fmt.Errorf("%s: %w", method, ctx.Err())
	case msg, ok := <-ch:
		if !ok {
			c.mu.Lock()
			err := c.err
			c.mu.Unlock()
			return fmt.Errorf("%s: %w", method, err)
		}
		if msg.Error != nil {
			return fmt.Errorf("%s: %w", method, msg.Error)
		}
		if result == nil || len(msg.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(msg.Result, result); err != nil {
			return fmt.Errorf("%s answered an unexpected result: %w", method, err)
		}
		return nil
	}
}

func (c *client) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}
