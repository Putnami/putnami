package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Session reporting packages (ADR 0135). Once a session reporter acknowledges
// a frame with Packages, the engine stops sending frames and sends its
// sealed packages in SessionReportingPut lines, several at once; the
// reporter answers each with a SessionReportingResult of the same address, in
// any order. A put carries every package sealed while the previous puts were
// in flight, so the count of requests follows the round trips, not the
// tasks. A final SessionReportingManifest, sent once every package is
// acknowledged, lists them all and closes the session.
const (
	SessionReportingPackagesVersion = 4
	SessionReportingOpPut           = "put"
	SessionReportingOpManifest      = "manifest"
	// SessionReportingPackageRecordBytes bounds the records of one package:
	// a package is sealed before one more record would take its records past
	// it. A single record larger than it travels alone.
	SessionReportingPackageRecordBytes = 4 << 20
	// SessionReportingMaxPackageBytes bounds a package's bytes, its header
	// line included.
	SessionReportingMaxPackageBytes = 8 << 20
	// SessionReportingMaxPackageLineBytes bounds a put, a result or a
	// manifest line: the base64 expansion of the largest package, and room
	// for its identity.
	SessionReportingMaxPackageLineBytes = 11 << 20
	// SessionReportingPackageFormat is the format the header line of every
	// package names.
	SessionReportingPackageFormat = 1
)

// The kinds of package. A task package holds records of events.jsonl; a
// document package holds one whole document.
const (
	SessionReportingPackageTask             = "task"
	SessionReportingPackagePlan             = "plan"
	SessionReportingPackageSession          = "session"
	SessionReportingPackageSpecVerification = "spec-verification"
)

var packageKinds = map[string]bool{
	SessionReportingPackageTask: true, SessionReportingPackagePlan: true,
	SessionReportingPackageSession: true, SessionReportingPackageSpecVerification: true,
}

// SessionReportingPackageKind reports whether kind is a known package kind.
func SessionReportingPackageKind(kind string) bool {
	return packageKinds[kind]
}

// SessionReportingPackageHeader is the first line of every package's bytes.
// A task package names its task's identity key ("" for records without one),
// its part (the packages of one key, from 0) and the raw offset in
// events.jsonl of each record that follows, in order; a document package
// names its kind and, for a document cut in parts, its part.
type SessionReportingPackageHeader struct {
	Package int     `json:"package"`
	Kind    string  `json:"kind"`
	Key     *string `json:"key,omitempty"`
	Part    int     `json:"part,omitempty"`
	Offsets []int64 `json:"offsets,omitempty"`
}

// EncodeSessionReportingPackage returns a package's bytes: the header line,
// then body as is.
func EncodeSessionReportingPackage(header SessionReportingPackageHeader, body []byte) []byte {
	header.Package = SessionReportingPackageFormat
	line, _ := json.Marshal(header)
	out := make([]byte, 0, len(line)+1+len(body))
	out = append(append(out, line...), '\n')
	return append(out, body...)
}

// ParseSessionReportingPackage splits a package into its header and the bytes
// after it. It checks the header's format and kind, and that a task package
// names a key and one offset per record of its body.
func ParseSessionReportingPackage(data []byte) (SessionReportingPackageHeader, []byte, error) {
	var header SessionReportingPackageHeader
	line, body, found := bytes.Cut(data, []byte{'\n'})
	if !found {
		return header, nil, fmt.Errorf("reporting package has no header line")
	}
	d := json.NewDecoder(bytes.NewReader(line))
	d.DisallowUnknownFields()
	if err := d.Decode(&header); err != nil || header.Package != SessionReportingPackageFormat || !packageKinds[header.Kind] {
		return header, nil, fmt.Errorf("invalid reporting package header")
	}
	if header.Kind != SessionReportingPackageTask {
		// A document larger than SessionReportingPackageRecordBytes travels in
		// parts, numbered from 0, which a receiver concatenates in order.
		if header.Key != nil || header.Part < 0 || header.Offsets != nil {
			return header, nil, fmt.Errorf("invalid reporting package header")
		}
		return header, body, nil
	}
	records := bytes.Count(body, []byte{'\n'})
	if len(body) > 0 && body[len(body)-1] != '\n' {
		records++
	}
	if header.Key == nil || header.Part < 0 || len(header.Offsets) != records || records == 0 {
		return header, nil, fmt.Errorf("invalid reporting package header")
	}
	for i, offset := range header.Offsets {
		if offset < 0 || i > 0 && offset <= header.Offsets[i-1] {
			return header, nil, fmt.Errorf("invalid reporting package offsets")
		}
	}
	return header, body, nil
}

// SessionReportingPackage is one package of a put: its kind, its address,
// the SHA-256 of Data, and its bytes (EncodeSessionReportingPackage).
type SessionReportingPackage struct {
	Kind    string `json:"kind"`
	Address string `json:"address"`
	Data    []byte `json:"data"`
}

// NewSessionReportingPackage returns the package of data, addressed.
func NewSessionReportingPackage(kind string, data []byte) SessionReportingPackage {
	return SessionReportingPackage{kind, SessionReportingDigest(data), append([]byte{}, data...)}
}

// SessionReportingPut carries a batch of packages, those sealed while the
// previous puts were in flight. Address is the put's identity in its result:
// the SHA-256 of its packages' addresses, each followed by LF.
type SessionReportingPut struct {
	ProtocolVersion int                       `json:"protocolVersion"`
	Op              string                    `json:"op"`
	SessionID       string                    `json:"sessionId"`
	Address         string                    `json:"address"`
	Packages        []SessionReportingPackage `json:"packages"`
}

// SessionReportingMaxPutPackages bounds the packages of one put; their bytes
// together are bounded by SessionReportingMaxPackageBytes.
const SessionReportingMaxPutPackages = 4096

// NewSessionReportingPut returns the put of packages.
func NewSessionReportingPut(sessionID string, packages []SessionReportingPackage) SessionReportingPut {
	return SessionReportingPut{SessionReportingPackagesVersion, SessionReportingOpPut, sessionID, putAddress(packages), append([]SessionReportingPackage{}, packages...)}
}

func putAddress(packages []SessionReportingPackage) string {
	var b strings.Builder
	for _, p := range packages {
		b.WriteString(p.Address)
		b.WriteByte('\n')
	}
	return SessionReportingDigest([]byte(b.String()))
}

// Validate checks the identity, the bounds, each package's kind and address,
// and the put's address.
func (p SessionReportingPut) Validate() error {
	if p.ProtocolVersion != SessionReportingPackagesVersion || p.Op != SessionReportingOpPut || !reportingSessionID.MatchString(p.SessionID) || !reportingDigest.MatchString(p.Address) {
		return fmt.Errorf("invalid reporting put identity")
	}
	if len(p.Packages) == 0 || len(p.Packages) > SessionReportingMaxPutPackages {
		return fmt.Errorf("invalid reporting put size")
	}
	total := 0
	seen := make(map[string]bool, len(p.Packages))
	for _, pkg := range p.Packages {
		total += len(pkg.Data)
		if !packageKinds[pkg.Kind] || len(pkg.Data) == 0 || seen[pkg.Address] || SessionReportingDigest(pkg.Data) != pkg.Address {
			return fmt.Errorf("invalid reporting package")
		}
		seen[pkg.Address] = true
	}
	if total > SessionReportingMaxPackageBytes {
		return fmt.Errorf("invalid reporting put size")
	}
	if putAddress(p.Packages) != p.Address {
		return fmt.Errorf("reporting put address mismatch")
	}
	return nil
}

// EncodeSessionReportingBundle returns the body a reporter sends for a put:
// for each package, one line `{"kind":...,"address":...,"length":N}`, then its
// N bytes.
func EncodeSessionReportingBundle(packages []SessionReportingPackage) []byte {
	var out bytes.Buffer
	for _, p := range packages {
		line, _ := json.Marshal(struct {
			Kind    string `json:"kind"`
			Address string `json:"address"`
			Length  int    `json:"length"`
		}{p.Kind, p.Address, len(p.Data)})
		out.Write(line)
		out.WriteByte('\n')
		out.Write(p.Data)
	}
	return out.Bytes()
}

// ParseSessionReportingBundle splits a bundle into its packages, checking
// each address.
func ParseSessionReportingBundle(body []byte) ([]SessionReportingPackage, error) {
	var packages []SessionReportingPackage
	for len(body) > 0 {
		line, rest, found := bytes.Cut(body, []byte{'\n'})
		var head struct {
			Kind    string `json:"kind"`
			Address string `json:"address"`
			Length  int    `json:"length"`
		}
		if !found || json.Unmarshal(line, &head) != nil || head.Length <= 0 || head.Length > len(rest) {
			return nil, fmt.Errorf("invalid reporting bundle")
		}
		p := SessionReportingPackage{head.Kind, head.Address, rest[:head.Length]}
		if !packageKinds[p.Kind] || SessionReportingDigest(p.Data) != p.Address {
			return nil, fmt.Errorf("invalid reporting bundle package")
		}
		packages = append(packages, p)
		body = rest[head.Length:]
	}
	if len(packages) == 0 {
		return nil, fmt.Errorf("empty reporting bundle")
	}
	return packages, nil
}

// SessionReportingManifest closes a session: every package's address, in
// sealing order, then the documents. Only a final manifest is sent.
type SessionReportingManifest struct {
	ProtocolVersion int      `json:"protocolVersion"`
	Op              string   `json:"op"`
	SessionID       string   `json:"sessionId"`
	Addresses       []string `json:"addresses"`
	Final           bool     `json:"final"`
}

// NewSessionReportingManifest returns the final manifest of addresses.
func NewSessionReportingManifest(sessionID string, addresses []string) SessionReportingManifest {
	return SessionReportingManifest{SessionReportingPackagesVersion, SessionReportingOpManifest, sessionID, append([]string{}, addresses...), true}
}

// Address is the manifest's identity in its result: the SHA-256 of its
// addresses, each followed by LF.
func (m SessionReportingManifest) Address() string {
	var b strings.Builder
	for _, address := range m.Addresses {
		b.WriteString(address)
		b.WriteByte('\n')
	}
	return SessionReportingDigest([]byte(b.String()))
}

// Validate checks the identity, that it is final, and that every address is
// a digest named once.
func (m SessionReportingManifest) Validate() error {
	if m.ProtocolVersion != SessionReportingPackagesVersion || m.Op != SessionReportingOpManifest || !reportingSessionID.MatchString(m.SessionID) || !m.Final {
		return fmt.Errorf("invalid reporting manifest")
	}
	seen := make(map[string]bool, len(m.Addresses))
	for _, address := range m.Addresses {
		if !reportingDigest.MatchString(address) || seen[address] {
			return fmt.Errorf("invalid reporting manifest address")
		}
		seen[address] = true
	}
	return nil
}

// SessionReportingResult answers one put or one manifest: Address echoes the
// put's address, or the manifest's (SessionReportingManifest.Address). OK
// means durable acceptance, an identical package included.
type SessionReportingResult struct {
	ProtocolVersion int    `json:"protocolVersion"`
	Op              string `json:"op"`
	SessionID       string `json:"sessionId"`
	Address         string `json:"address"`
	OK              bool   `json:"ok"`
	Retryable       bool   `json:"retryable,omitempty"`
	Code            string `json:"code,omitempty"`
}

// Accept constructs the acceptance of this put.
func (p SessionReportingPut) Accept() SessionReportingResult {
	return SessionReportingResult{ProtocolVersion: p.ProtocolVersion, Op: p.Op, SessionID: p.SessionID, Address: p.Address, OK: true}
}

// Accept constructs the acceptance of this manifest.
func (m SessionReportingManifest) Accept() SessionReportingResult {
	return SessionReportingResult{ProtocolVersion: m.ProtocolVersion, Op: m.Op, SessionID: m.SessionID, Address: m.Address(), OK: true}
}

// Refuse turns an acceptance into a refusal with code.
func (r SessionReportingResult) Refuse(code string, retryable bool) SessionReportingResult {
	r.OK, r.Code, r.Retryable = false, code, retryable
	return r
}

// SessionReportingPackageOp reports the op of a version 4 line, or "" for any
// other line, such as a frame.
func SessionReportingPackageOp(line []byte) string {
	var probe struct {
		ProtocolVersion int    `json:"protocolVersion"`
		Op              string `json:"op"`
	}
	if json.Unmarshal(line, &probe) != nil || probe.ProtocolVersion != SessionReportingPackagesVersion {
		return ""
	}
	return probe.Op
}

// ParseSessionReportingPut strictly decodes one put line and verifies its
// bytes.
func ParseSessionReportingPut(line []byte) (*SessionReportingPut, error) {
	var p SessionReportingPut
	if err := parseReportingLine(line, SessionReportingMaxPackageLineBytes, &p, "protocolVersion", "op", "sessionId", "address", "packages"); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	var fields struct {
		Packages []map[string]json.RawMessage `json:"packages"`
	}
	_ = json.Unmarshal(line, &fields)
	for i, pkg := range fields.Packages {
		if len(pkg) != 3 || pkg["kind"] == nil || pkg["address"] == nil || pkg["data"] == nil {
			return nil, fmt.Errorf("invalid reporting package fields")
		}
		var encoded string
		_ = json.Unmarshal(pkg["data"], &encoded)
		if encoded != base64.StdEncoding.EncodeToString(p.Packages[i].Data) {
			return nil, fmt.Errorf("noncanonical reporting base64")
		}
	}
	return &p, nil
}

// ParseSessionReportingManifest strictly decodes one manifest line.
func ParseSessionReportingManifest(line []byte) (*SessionReportingManifest, error) {
	var m SessionReportingManifest
	if err := parseReportingLine(line, SessionReportingMaxPackageLineBytes, &m, "protocolVersion", "op", "sessionId", "addresses", "final"); err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// ParseSessionReportingResult strictly decodes one result line.
func ParseSessionReportingResult(line []byte) (*SessionReportingResult, error) {
	var r SessionReportingResult
	if err := parseReporting(line, &r, "protocolVersion", "op", "sessionId", "address", "ok"); err != nil {
		return nil, err
	}
	if r.ProtocolVersion != SessionReportingPackagesVersion || r.Op != SessionReportingOpPut && r.Op != SessionReportingOpManifest || !reportingSessionID.MatchString(r.SessionID) || !reportingDigest.MatchString(r.Address) {
		return nil, fmt.Errorf("invalid reporting result identity")
	}
	if r.OK && (r.Code != "" || r.Retryable) || !r.OK && !reportingCode.MatchString(r.Code) {
		return nil, fmt.Errorf("invalid reporting result status")
	}
	return &r, nil
}
