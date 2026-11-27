package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func taskPackage(t *testing.T, key string, records ...string) []byte {
	t.Helper()
	var body bytes.Buffer
	offsets := make([]int64, 0, len(records))
	at := int64(100)
	for _, record := range records {
		offsets = append(offsets, at)
		body.WriteString(record + "\n")
		at += int64(len(record) + 1)
	}
	return EncodeSessionReportingPackage(SessionReportingPackageHeader{Kind: SessionReportingPackageTask, Key: &key, Offsets: offsets}, body.Bytes())
}

// TestSessionReportingPackagesRoundTrip pins ADR 0135's wire: a put carries a
// package addressed by its bytes, its header names one offset per record, a
// manifest lists addresses once, and results echo the address in any order.
func TestSessionReportingPackagesRoundTrip(t *testing.T) {
	spectest.Proves(t, "cli/machine-contract", "the-reporting-wire", "the-reporting-wire-passes-its-corpus")
	data := taskPackage(t, "/a:test~test", `{"record":"task:start"}`, `{"record":"task:end"}`)
	header, body, err := ParseSessionReportingPackage(data)
	if err != nil || *header.Key != "/a:test~test" || len(header.Offsets) != 2 || !bytes.HasPrefix(body, []byte(`{"record":"task:start"}`)) {
		t.Fatalf("package = %+v %q %v", header, body, err)
	}
	doc0 := EncodeSessionReportingPackage(SessionReportingPackageHeader{Kind: SessionReportingPackagePlan}, []byte("{}"))
	put := NewSessionReportingPut("session", []SessionReportingPackage{NewSessionReportingPackage(SessionReportingPackageTask, data), NewSessionReportingPackage(SessionReportingPackagePlan, doc0)})
	line, _ := json.Marshal(put)
	if SessionReportingPackageOp(line) != SessionReportingOpPut {
		t.Fatal("a put line is not recognized")
	}
	parsed, err := ParseSessionReportingPut(line)
	if err != nil || parsed.Packages[0].Address != SessionReportingDigest(data) || len(parsed.Packages) != 2 {
		t.Fatalf("put = %v", err)
	}
	bundle := EncodeSessionReportingBundle(parsed.Packages)
	if back, err := ParseSessionReportingBundle(bundle); err != nil || len(back) != 2 || !bytes.Equal(back[0].Data, data) || back[1].Kind != SessionReportingPackagePlan {
		t.Fatalf("bundle round trip = %v", err)
	}
	for _, bad := range [][]byte{nil, []byte("x"), bundle[:len(bundle)-1], bytes.Replace(bundle, []byte(`"length":`), []byte(`"length":-`), 1)} {
		if _, err := ParseSessionReportingBundle(bad); err == nil {
			t.Errorf("an invalid bundle was accepted: %q", bad)
		}
	}
	result, _ := json.Marshal(parsed.Accept())
	if r, err := ParseSessionReportingResult(result); err != nil || !r.OK || r.Address != put.Address {
		t.Fatalf("result = %+v %v", r, err)
	}
	refused, _ := json.Marshal(parsed.Accept().Refuse("receiver_unavailable", true))
	if r, err := ParseSessionReportingResult(refused); err != nil || r.OK || !r.Retryable {
		t.Fatalf("refusal = %+v %v", r, err)
	}
	manifest := NewSessionReportingManifest("session", []string{put.Packages[0].Address, put.Packages[1].Address})
	mline, _ := json.Marshal(manifest)
	if m, err := ParseSessionReportingManifest(mline); err != nil || m.Address() != manifest.Address() || SessionReportingPackageOp(mline) != SessionReportingOpManifest {
		t.Fatalf("manifest = %v", err)
	}
	if r, err := ParseSessionReportingResult(mustJSON(t, manifest.Accept())); err != nil || r.Op != SessionReportingOpManifest {
		t.Fatalf("manifest result = %+v %v", r, err)
	}
	// A frame is no package line.
	if SessionReportingPackageOp(mustJSON(t, NewSessionReportingChunk("session", "events.jsonl", 0, 0, []byte("x"), false))) != "" {
		t.Fatal("a frame read as a package line")
	}
	doc := EncodeSessionReportingPackage(SessionReportingPackageHeader{Kind: SessionReportingPackageSession}, []byte(`{"protocolVersion":2}`))
	if h, body, err := ParseSessionReportingPackage(doc); err != nil || h.Kind != SessionReportingPackageSession || string(body) != `{"protocolVersion":2}` {
		t.Fatalf("document package = %+v %q %v", h, body, err)
	}
	part := EncodeSessionReportingPackage(SessionReportingPackageHeader{Kind: SessionReportingPackageSession, Part: 2}, []byte(`"tasks":[]`))
	if h, _, err := ParseSessionReportingPackage(part); err != nil || h.Part != 2 {
		t.Fatalf("document part = %+v %v", h, err)
	}
}

// TestSessionReportingPackagesRefuseInvalidLines refuses every malformed put,
// manifest, result and package header.
func TestSessionReportingPackagesRefuseInvalidLines(t *testing.T) {
	spectest.Proves(t, "cli/machine-contract", "the-reporting-wire", "the-reporting-wire-passes-its-corpus")
	data := taskPackage(t, "k", `{"a":1}`)
	pkg := NewSessionReportingPackage(SessionReportingPackageTask, data)
	good := NewSessionReportingPut("session", []SessionReportingPackage{pkg})
	big := NewSessionReportingPackage(SessionReportingPackageTask, bytes.Repeat([]byte("x"), SessionReportingMaxPackageBytes+1))
	for name, change := range map[string]func(*SessionReportingPut){
		"version": func(p *SessionReportingPut) { p.ProtocolVersion = 3 },
		"op":      func(p *SessionReportingPut) { p.Op = "get" },
		"kind": func(p *SessionReportingPut) {
			p.Packages = []SessionReportingPackage{{Kind: "evidence", Address: pkg.Address, Data: pkg.Data}}
		},
		"address": func(p *SessionReportingPut) { p.Address = SessionReportingDigest([]byte("other")) },
		"digest": func(p *SessionReportingPut) {
			p.Packages = []SessionReportingPackage{{Kind: pkg.Kind, Address: SessionReportingDigest(nil), Data: pkg.Data}}
		},
		"empty": func(p *SessionReportingPut) { *p = NewSessionReportingPut("session", nil) },
		"duplicate": func(p *SessionReportingPut) {
			*p = NewSessionReportingPut("session", []SessionReportingPackage{pkg, pkg})
		},
		"session": func(p *SessionReportingPut) { p.SessionID = "../x" },
		"large":   func(p *SessionReportingPut) { *p = NewSessionReportingPut("session", []SessionReportingPackage{big}) },
	} {
		bad := good
		change(&bad)
		if _, err := ParseSessionReportingPut(mustJSON(t, bad)); err == nil {
			t.Errorf("%s: an invalid put was accepted", name)
		}
	}
	for name, m := range map[string]SessionReportingManifest{
		"duplicate": NewSessionReportingManifest("session", []string{pkg.Address, pkg.Address}),
		"digest":    NewSessionReportingManifest("session", []string{"nope"}),
		"not final": {ProtocolVersion: 4, Op: SessionReportingOpManifest, SessionID: "session", Addresses: []string{}},
	} {
		if _, err := ParseSessionReportingManifest(mustJSON(t, m)); err == nil {
			t.Errorf("%s: an invalid manifest was accepted", name)
		}
	}
	for _, line := range []string{
		`{"protocolVersion":4,"op":"put","sessionId":"session","address":"` + good.Address + `","ok":false}`,
		`{"protocolVersion":4,"op":"put","sessionId":"session","address":"` + good.Address + `","ok":true,"code":"x"}`,
		`{"protocolVersion":4,"op":"other","sessionId":"session","address":"` + good.Address + `","ok":true}`,
	} {
		if _, err := ParseSessionReportingResult([]byte(line)); err == nil {
			t.Errorf("an invalid result was accepted: %s", line)
		}
	}
	key := "k"
	for name, header := range map[string]string{
		"no header":      `{"a":1}`,
		"offsets":        `{"package":1,"kind":"task","key":"k","offsets":[1,2]}` + "\n" + `{"a":1}` + "\n",
		"descending":     `{"package":1,"kind":"task","key":"k","offsets":[5,1]}` + "\n" + "a\nb\n",
		"no key":         `{"package":1,"kind":"task","offsets":[1]}` + "\n" + "a\n",
		"document key":   `{"package":1,"kind":"plan","key":"k"}` + "\n{}",
		"document part":  `{"package":1,"kind":"session","part":-1}` + "\n{}",
		"format":         `{"package":2,"kind":"plan"}` + "\n{}",
		"unknown member": `{"package":1,"kind":"plan","x":1}` + "\n{}",
	} {
		if _, _, err := ParseSessionReportingPackage([]byte(header)); err == nil {
			t.Errorf("%s: an invalid package was accepted", name)
		}
	}
	_ = key
	ack := NewSessionReportingChunk("session", "plan.json", 0, 0, []byte("x"), false).Ack()
	if _, err := ParseSessionReportingAck([]byte(strings.Replace(string(mustJSON(t, ack)), `"ok":true`, `"ok":true,"packages":false`, 1))); err == nil {
		t.Error("an ack with packages false was accepted")
	}
	ack.Packages = true
	if a, err := ParseSessionReportingAck(mustJSON(t, ack)); err != nil || !a.Packages {
		t.Errorf("an ack with packages true was refused: %v", err)
	}
}
