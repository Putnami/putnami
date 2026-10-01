package objectcachetest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sync"

	cache "go.putnami.dev/protocol/cache"
)

// Client is a minimal driver for the object-cache socket: one connection, one
// request in flight, ids numbered from 1 within the connection.
//
// It is a TEST DRIVER, not the production client. A real consumer batches,
// keeps a local store in front of the socket, puts asynchronously, and never
// blocks its compiler on the network. This one exists so both sides of the
// contract can be exercised from a test: a provider implementation drives its
// own server with it, and a consumer implementation drives Server with it.
type Client struct {
	conn net.Conn

	mu     sync.Mutex
	reader *bufio.Reader
	nextID int64
}

// Dial connects to an object-cache socket.
func Dial(path string) (*Client, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("objectcachetest: dial %s: %w", path, err)
	}
	return &Client{conn: conn, reader: bufio.NewReader(conn), nextID: 1}, nil
}

// Close releases the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Get looks up a batch of ids and returns the hits. Misses are simply absent
// from the result.
func (c *Client) Get(params *cache.ObjectGetParams) (*cache.ObjectGetResult, error) {
	payload, err := c.call(cache.OpObjectGet, params)
	if err != nil {
		return nil, err
	}
	result, diags := cache.ParseAndValidateObjectGetResult(payload)
	if result == nil {
		return nil, fmt.Errorf("objectcachetest: invalid object-get result: %v", diags)
	}
	return result, nil
}

// Put offers a batch of objects whose bytes the caller already staged in the
// exchange directory, and returns how many the server queued.
func (c *Client) Put(params *cache.ObjectPutParams) (*cache.ObjectPutResult, error) {
	payload, err := c.call(cache.OpObjectPut, params)
	if err != nil {
		return nil, err
	}
	result, diags := cache.ParseAndValidateObjectPutResult(payload)
	if result == nil {
		return nil, fmt.Errorf("objectcachetest: invalid object-put result: %v", diags)
	}
	return result, nil
}

// Call issues one raw request and returns the response envelope, so a test can
// assert what the socket does with an op it must refuse.
func (c *Client) Call(op cache.ProviderOp, params any) (*cache.ProviderResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.roundTrip(op, params)
}

func (c *Client) call(op cache.ProviderOp, params any) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	resp, err := c.roundTrip(op, params)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		code, message := "unknown", "provider reported failure"
		if resp.Error != nil {
			code, message = resp.Error.Code, resp.Error.Message
		}
		return nil, fmt.Errorf("objectcachetest: op %s failed (%s): %s", op, code, message)
	}
	return resp.Payload, nil
}

func (c *Client) roundTrip(op cache.ProviderOp, params any) (*cache.ProviderResponse, error) {
	raw, err := cache.MarshalPayload(params)
	if err != nil {
		return nil, fmt.Errorf("objectcachetest: encode %s params: %w", op, err)
	}
	id := c.nextID
	c.nextID++
	line, err := json.Marshal(&cache.ProviderRequest{
		ProtocolVersion: cache.ProviderProtocolVersion,
		ID:              id,
		Op:              op,
		Payload:         raw,
	})
	if err != nil {
		return nil, fmt.Errorf("objectcachetest: encode %s request: %w", op, err)
	}
	if _, err := c.conn.Write(append(line, '\n')); err != nil {
		return nil, fmt.Errorf("objectcachetest: write %s request: %w", op, err)
	}
	responseLine, err := c.reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("objectcachetest: read %s response: %w", op, err)
	}
	resp, diags := cache.ParseAndValidateProviderResponse(responseLine)
	if resp == nil {
		return nil, fmt.Errorf("objectcachetest: malformed response: %v", diags)
	}
	if resp.ID != id {
		return nil, fmt.Errorf("objectcachetest: response id %d does not answer request %d", resp.ID, id)
	}
	return resp, nil
}

// StageBlob writes content into the exchange directory at its content address,
// which is what a caller must do before offering the object in a put. It
// returns the digest.
func StageBlob(exchangeDir string, content []byte) (string, error) {
	digest := cache.DigestOf(content)
	if err := stageBlob(exchangeDir, digest, content); err != nil {
		return "", err
	}
	return digest, nil
}
