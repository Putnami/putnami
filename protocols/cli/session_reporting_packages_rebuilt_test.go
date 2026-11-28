package cli

import (
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// TestSessionReportingPackagesCarryRebuiltRecords pins the rebuilt member of a
// task package header (CLI ADR 0137): records events.jsonl does not hold sit
// at the offset of the task:end that follows them, so a package's offsets do
// not decrease, and two are equal only after a rebuilt record.
func TestSessionReportingPackagesCarryRebuiltRecords(t *testing.T) {
	spectest.Proves(t, "cli/machine-contract", "the-reporting-wire", "the-reporting-wire-passes-its-corpus")
	key := "/a:test~test"
	body := []byte(`{"record":"task:start"}` + "\n" + `{"record":"task:event"}` + "\n" + `{"record":"test:case"}` + "\n" + `{"record":"task:end"}` + "\n")
	encode := func(offsets []int64, rebuilt []int) []byte {
		return EncodeSessionReportingPackage(SessionReportingPackageHeader{Kind: SessionReportingPackageTask, Key: &key, Offsets: offsets, Rebuilt: rebuilt}, body)
	}

	header, got, err := ParseSessionReportingPackage(encode([]int64{100, 200, 200, 200}, []int{1, 2}))
	if err != nil || len(header.Rebuilt) != 2 || header.Rebuilt[0] != 1 || header.Rebuilt[1] != 2 || len(got) != len(body) {
		t.Fatalf("a package with two rebuilt records before its task:end = %+v %v", header, err)
	}
	if _, _, err := ParseSessionReportingPackage(encode([]int64{200, 200, 200, 200}, []int{0, 1, 2})); err != nil {
		t.Fatalf("a part that starts with rebuilt records was refused: %v", err)
	}
	if _, _, err := ParseSessionReportingPackage(encode([]int64{100, 200, 300, 400}, []int{3})); err != nil {
		t.Fatalf("a part that ends with a rebuilt record was refused: %v", err)
	}

	for name, data := range map[string][]byte{
		"equal offsets after a stream record":  encode([]int64{100, 200, 200, 300}, nil),
		"equal offsets after the wrong record": encode([]int64{100, 200, 200, 200}, []int{0, 2}),
		"a decreasing offset":                  encode([]int64{100, 200, 150, 300}, []int{1}),
		"a rebuilt index past the records":     encode([]int64{100, 200, 300, 400}, []int{4}),
		"a negative rebuilt index":             encode([]int64{100, 200, 300, 400}, []int{-1}),
		"rebuilt indexes out of order":         encode([]int64{100, 200, 200, 200}, []int{2, 1}),
		"a rebuilt index twice":                encode([]int64{100, 200, 200, 200}, []int{1, 1}),
		"rebuilt records in a document":        EncodeSessionReportingPackage(SessionReportingPackageHeader{Kind: SessionReportingPackageSession, Rebuilt: []int{0}}, []byte("{}")),
	} {
		if _, _, err := ParseSessionReportingPackage(data); err == nil {
			t.Errorf("%s: a package was accepted", name)
		}
	}
}

// TestSessionReportingAckOffersRebuiltPackages pins the acknowledgement member
// that gates rebuilt records (CLI ADR 0137): packagesRebuilt is true only,
// and only beside packages, and an engine that reads it names framing
// version 5, past the packages version 4, whose engines refuse it.
func TestSessionReportingAckOffersRebuiltPackages(t *testing.T) {
	spectest.Proves(t, "cli/machine-contract", "the-reporting-wire", "the-reporting-wire-passes-its-corpus")
	if SessionReportingPackagesVersion != 4 || SessionReportingRebuiltPackagesVersion != 5 {
		t.Fatalf("framing versions = %d and %d, want 4 for packages and 5 for rebuilt packages", SessionReportingPackagesVersion, SessionReportingRebuiltPackagesVersion)
	}
	ack := NewSessionReportingChunk("session", "plan.json", 0, 0, []byte("x"), false).Ack()
	ack.Packages, ack.PackagesRebuilt = true, true
	if a, err := ParseSessionReportingAck(mustJSON(t, ack)); err != nil || !a.Packages || !a.PackagesRebuilt {
		t.Fatalf("an ack offering rebuilt packages = %+v, %v", a, err)
	}
	packagesOnly := ack
	packagesOnly.PackagesRebuilt = false
	if a, err := ParseSessionReportingAck(mustJSON(t, packagesOnly)); err != nil || a.PackagesRebuilt {
		t.Fatalf("an ack offering packages only = %+v, %v", a, err)
	}
	plain := NewSessionReportingChunk("session", "plan.json", 0, 0, []byte("x"), false).Ack()
	for name, member := range map[string]string{
		"false":            `"packages":true,"packagesRebuilt":false`,
		"without packages": `"packagesRebuilt":true`,
	} {
		line := strings.Replace(string(mustJSON(t, plain)), `"ok":true`, `"ok":true,`+member, 1)
		if _, err := ParseSessionReportingAck([]byte(line)); err == nil {
			t.Errorf("%s: an ack with packagesRebuilt was accepted", name)
		}
	}
}
