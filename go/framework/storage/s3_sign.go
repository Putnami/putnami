package storage

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/errors"
)

// unsignedPayload is the literal value used for X-Amz-Content-Sha256 (and as the
// hashed-payload in the canonical request) for SigV4's single unsigned payload
// mode. The request still needs a normal, non-aws-chunked HTTP payload; chunked
// signed bodies use a different S3-specific streaming SigV4 protocol.
const unsignedPayload = "UNSIGNED-PAYLOAD"

// signRequest signs an HTTP request using AWS Signature Version 4.
// If no credentials are configured, the request is sent unsigned.
//
// payloadHash is the value placed in both the X-Amz-Content-Sha256 header and
// the hashed-payload field of the canonical request. Pass sha256Hex(body) for a
// buffered body, sha256Hex(nil) for an empty body, or unsignedPayload for the
// single unsigned payload mode.
func (b *S3Backend) signRequest(req *http.Request, payloadHash string) {
	if b.config.AccessKey == "" || b.config.SecretKey == "" {
		return
	}

	now := time.Now().UTC()
	date := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("Host", req.URL.Host)

	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	// Canonical request
	canonicalHeaders, signedHeaders := canonicalAndSignedHeaders(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req),
		canonicalQueryString(req),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	// String to sign
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", date, b.config.Region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	// Signing key
	signingKey := deriveSigningKey(b.config.SecretKey, date, b.config.Region, "s3")

	// Signature
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	// Authorization header
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		b.config.AccessKey, scope, signedHeaders, signature,
	))
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func deriveSigningKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

// canonicalURI returns the URI-encoded request path used in the SigV4 canonical
// request. It must be the exact path transmitted on the wire (EscapedPath), so
// the signature covers what the server receives — signing the decoded path would
// mismatch any key containing characters that get percent-encoded.
func canonicalURI(req *http.Request) string {
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	return path
}

func canonicalQueryString(req *http.Request) string {
	return canonicalQueryValues(req.URL.Query())
}

// canonicalQueryValues renders query parameters in the AWS SigV4 canonical form:
// keys (and values) URI-encoded and sorted, joined with "&".
func canonicalQueryValues(params url.Values) string {
	if len(params) == 0 {
		return ""
	}

	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var pairs []string
	for _, k := range keys {
		values := params[k]
		sort.Strings(values)
		for _, v := range values {
			pairs = append(pairs, fmt.Sprintf("%s=%s", uriEncode(k), uriEncode(v)))
		}
	}
	return strings.Join(pairs, "&")
}

// encodeS3Path URI-encodes an object key for use in an S3 request path. Each
// "/"-separated segment is percent-encoded per AWS SigV4 rules while the slashes
// that delimit pseudo-directories are preserved. Without this, keys containing
// spaces or reserved characters (e.g. "?") would corrupt the request URL or
// produce a signature that does not match the path sent on the wire.
func encodeS3Path(key string) string {
	segments := strings.Split(key, "/")
	for i, s := range segments {
		segments[i] = uriEncode(s)
	}
	return strings.Join(segments, "/")
}

// presignedURL builds a query-string SigV4 pre-signed URL for the given method.
// Credentials are required. For PUT URLs, a non-empty contentType is bound into
// the signature so the upload is restricted to that type.
func (b *S3Backend) presignedURL(method, bucket, key string, expiry time.Duration, contentType string) (string, error) {
	if b.config.AccessKey == "" || b.config.SecretKey == "" {
		return "", errors.New(CodeStorageRequest, "presigned URL requires AccessKey and SecretKey", errors.String("backend", "s3"))
	}
	if expiry <= 0 {
		expiry = time.Hour
	}

	u, err := url.Parse(b.objectURL(bucket, key))
	if err != nil {
		return "", errors.Wrap(err, CodeStorageRequest, errors.String("backend", "s3"), errors.String("op", "presign"))
	}

	now := time.Now().UTC()
	date := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", date, b.config.Region)

	signedHeaders := "host"
	canonicalHeaders := "host:" + u.Host + "\n"
	if method == http.MethodPut && contentType != "" {
		signedHeaders = "content-type;host"
		canonicalHeaders = "content-type:" + contentType + "\nhost:" + u.Host + "\n"
	}

	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", b.config.AccessKey+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.Itoa(int(expiry.Seconds())))
	q.Set("X-Amz-SignedHeaders", signedHeaders)
	canonicalQuery := canonicalQueryValues(q)

	canonicalURIStr := u.EscapedPath()
	if canonicalURIStr == "" {
		canonicalURIStr = "/"
	}

	canonicalRequest := strings.Join([]string{
		method,
		canonicalURIStr,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		unsignedPayload,
	}, "\n")

	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signingKey := deriveSigningKey(b.config.SecretKey, date, b.config.Region, "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	u.RawQuery = canonicalQuery + "&X-Amz-Signature=" + signature
	return u.String(), nil
}

func canonicalAndSignedHeaders(req *http.Request) (canonical, signed string) {
	headerMap := make(map[string]string, len(req.Header))
	headerKeys := make([]string, 0, len(req.Header))

	for key, values := range req.Header {
		lower := strings.ToLower(key)
		headerMap[lower] = strings.TrimSpace(strings.Join(values, ","))
		headerKeys = append(headerKeys, lower)
	}
	sort.Strings(headerKeys)

	canonicalParts := make([]string, 0, len(headerKeys))
	for _, k := range headerKeys {
		canonicalParts = append(canonicalParts, k+":"+headerMap[k]+"\n")
	}
	return strings.Join(canonicalParts, ""), strings.Join(headerKeys, ";")
}

func uriEncode(s string) string {
	var buf strings.Builder
	for _, b := range []byte(s) {
		if isUnreserved(b) {
			buf.WriteByte(b)
		} else {
			buf.WriteByte('%')
			buf.WriteString(strings.ToUpper(hex.EncodeToString([]byte{b})))
		}
	}
	return buf.String()
}

func isUnreserved(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') ||
		(b >= '0' && b <= '9') || b == '-' || b == '.' || b == '_' || b == '~'
}
