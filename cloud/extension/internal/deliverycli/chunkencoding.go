package deliverycli

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
)

// Content coding of a chunk request (RFC 9110 section 8.4). A receiver names
// the codings it reads in the Accept-Encoding header of its answers
// (RFC 7694), and stores the decoded bytes of a gzip chunk.
const (
	contentEncodingHeader = "Content-Encoding"
	acceptEncodingHeader  = "Accept-Encoding"
	gzipCoding            = "gzip"
)

// chunkEncoder chooses the body of each chunk request. It compresses a chunk
// with gzip only when the latest answer of the same destination advertised
// gzip, so the first chunk to a destination goes as is. A receiver that does
// not advertise gzip stores a body as it arrives, and a compressed body would
// corrupt its record.
//
// A chunk with no bytes, such as a final marker, goes as is, and so does a
// chunk that gzip does not shrink. A receiver stores the same bytes either
// way, so a resent sequence compares equal compressed or not.
//
// One encoder serves one reporter, whose frames arrive one at a time; it is
// not safe for concurrent use.
type chunkEncoder struct {
	// advertised is the destination whose latest answer advertised gzip, or ""
	// when the latest exchange did not.
	advertised string
	zw         *gzip.Writer
}

// body returns the request body of data for url and the content coding it
// carries: gzip, or "" for data as is. The returned slice is never reused.
func (e *chunkEncoder) body(url string, data []byte) ([]byte, string) {
	if len(data) == 0 || url == "" || e.advertised != url {
		return data, ""
	}
	var buf bytes.Buffer
	if e.zw == nil {
		e.zw = gzip.NewWriter(&buf)
	} else {
		e.zw.Reset(&buf)
	}
	if _, err := e.zw.Write(data); err != nil {
		return data, ""
	}
	if err := e.zw.Close(); err != nil || buf.Len() >= len(data) {
		return data, ""
	}
	return buf.Bytes(), gzipCoding
}

// observe records the answer url gave, or no answer when header is nil (a
// failure in transport). Only an answer that advertises gzip lets the next
// chunk to url be compressed.
func (e *chunkEncoder) observe(url string, header http.Header) {
	if header != nil && advertisesGzip(header) {
		e.advertised = url
		return
	}
	e.advertised = ""
}

// accepts reports whether the latest answer of url advertised gzip.
func (e *chunkEncoder) accepts(url string) bool {
	return url != "" && e.advertised == url
}

// setContentEncoding names coding on req, when it has one.
func setContentEncoding(req *http.Request, coding string) {
	if coding != "" {
		req.Header.Set(contentEncodingHeader, coding)
	}
}

// advertisesGzip reports whether header holds an Accept-Encoding member that
// names gzip with a nonzero weight. A malformed weight does not advertise.
func advertisesGzip(header http.Header) bool {
	for _, value := range header.Values(acceptEncodingHeader) {
		for _, member := range strings.Split(value, ",") {
			coding, params, _ := strings.Cut(member, ";")
			if !strings.EqualFold(strings.TrimSpace(coding), gzipCoding) {
				continue
			}
			if acceptableWeight(params) {
				return true
			}
		}
	}
	return false
}

// acceptableWeight reports whether the parameters of one Accept-Encoding
// member leave it acceptable: no q parameter, or a q between 0 excluded and 1.
func acceptableWeight(params string) bool {
	for _, param := range strings.Split(params, ";") {
		name, value, _ := strings.Cut(param, "=")
		if !strings.EqualFold(strings.TrimSpace(name), "q") {
			continue
		}
		weight, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || weight <= 0 || weight > 1 {
			return false
		}
	}
	return true
}
