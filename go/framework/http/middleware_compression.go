package http

import (
	"bytes"
	"compress/gzip"
	"strings"
	"sync"
)

// CompressionOptions configures the compression middleware.
type CompressionOptions struct {
	// Threshold is the minimum response body size in bytes to compress. Default: 1024.
	Threshold int
}

// compressibleTypes is the set of Content-Type prefixes that should be compressed.
var compressibleTypes = []string{
	"text/",
	"application/json",
	"application/xml",
	"application/javascript",
	"application/yaml",
	"image/svg+xml",
}

// gzipPool reuses gzip.Writer instances to reduce allocation pressure.
var gzipPool = sync.Pool{
	New: func() any {
		return gzip.NewWriter(nil)
	},
}

// Compression middleware applies gzip compression to responses when the client
// supports it and the response body exceeds the threshold.
//
//	server.Use(http.Compression(http.CompressionOptions{Threshold: 512}))
func Compression(opts CompressionOptions) Middleware {
	if opts.Threshold <= 0 {
		opts.Threshold = 1024
	}

	return func(ctx *Context, next func() *Response) *Response {
		// Check if client accepts gzip
		acceptEncoding := ctx.Header("Accept-Encoding")
		if !strings.Contains(acceptEncoding, "gzip") {
			return next()
		}

		resp := next()
		if resp == nil {
			return nil
		}

		// Skip if already encoded
		if resp.Headers.Get("Content-Encoding") != "" {
			return resp
		}

		// Check content type is compressible
		ct := resp.Headers.Get("Content-Type")
		if !isCompressible(ct) {
			return resp
		}

		// Serialize body to check size
		bodyBytes, err := resp.BodyBytes()
		if err != nil {
			return resp
		}
		if len(bodyBytes) < opts.Threshold {
			// Below the compression threshold: reuse the bytes we just
			// serialized so Response.WriteTo doesn't JSON-marshal the body a
			// second time. Only adopt them when the response carried a
			// structured body (raw was nil); otherwise leave it untouched.
			if resp.raw == nil {
				resp.raw = bodyBytes
				resp.body = nil
			}
			return resp
		}

		// Compress with gzip using pooled writer
		var buf bytes.Buffer
		gz, _ := gzipPool.Get().(*gzip.Writer) //nolint:errcheck // pool always returns *gzip.Writer
		if gz == nil {
			gz = gzip.NewWriter(&buf)
		} else {
			gz.Reset(&buf)
		}
		if _, err := gz.Write(bodyBytes); err != nil {
			gzipPool.Put(gz)
			return resp // Fall back to uncompressed
		}
		if err := gz.Close(); err != nil {
			gzipPool.Put(gz)
			return resp
		}
		gzipPool.Put(gz)

		compressed := NewResponse(resp.Status)
		compressed.raw = buf.Bytes()
		// Copy original headers
		for key, values := range resp.Headers {
			for _, v := range values {
				compressed.Headers.Add(key, v)
			}
		}
		compressed.Headers.Set("Content-Encoding", "gzip")
		compressed.Headers.Set("Vary", "Accept-Encoding")
		compressed.Headers.Del("Content-Length")
		return compressed
	}
}

func isCompressible(contentType string) bool {
	if contentType == "" {
		return false
	}
	ct := strings.ToLower(contentType)
	for _, prefix := range compressibleTypes {
		if strings.HasPrefix(ct, prefix) {
			return true
		}
	}
	return false
}
