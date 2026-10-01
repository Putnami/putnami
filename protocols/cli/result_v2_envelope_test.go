package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// The non-job envelope constructor. Every case asserts the
// document against ValidateDocument rather than against hand-written expected
// bytes: the point of the constructor is that a command cannot build an
// envelope the contract rejects, so the contract's own validator is the
// assertion.

func TestNewResultV2_SuccessCarriesNoError(t *testing.T) {
	r := NewResultV2("contracts check", map[string]any{"outcome": "clean"}, nil)
	if r.ProtocolVersion != ResultProtocolVersion {
		t.Errorf("protocolVersion = %d, want %d", r.ProtocolVersion, ResultProtocolVersion)
	}
	if r.Status != StatusSuccess {
		t.Errorf("status = %q, want %q", r.Status, StatusSuccess)
	}
	if r.ExitCode != ExitSuccess {
		t.Errorf("exitCode = %d, want %d", r.ExitCode, ExitSuccess)
	}
	if r.Error != nil {
		t.Errorf("success envelope carries error %+v", r.Error)
	}
	assertEnvelopeValid(t, r)
}

// TestNewResultV2_FailureClassesMapToTheContract walks the whole error
// vocabulary. The exit code and the error class are derived by the SAME
// functions the process exit path uses, so a class whose code disagreed with
// its verdict would fail the validator here rather than on a consumer's wire.
func TestNewResultV2_FailureClassesMapToTheContract(t *testing.T) {
	for name, tc := range map[string]struct {
		err      error
		status   string
		code     string
		exitCode int
	}{
		"unclassified": {errors.New("boom"), StatusFailure, "failure", ExitFailure},
		"usage":        {Usagef("bad flag"), StatusFailure, "usage", ExitUsage},
		"invalid config": {
			Classify(errors.New("manifest drifted"), ErrInvalidConfig),
			StatusFailure, "usage", ExitUsage,
		},
		"auth": {Authf("no token"), StatusFailure, "auth", ExitAuth},
		"api":  {APIf("registry 500"), StatusFailure, "api", ExitAPI},
		"signal": {
			Classify(errors.New("interrupted"), ErrSignal),
			StatusAborted, "signal", ExitSignal,
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := NewResultV2("doctor", map[string]any{"checks": 3}, tc.err)
			if r.Status != tc.status {
				t.Errorf("status = %q, want %q", r.Status, tc.status)
			}
			if r.ExitCode != tc.exitCode {
				t.Errorf("exitCode = %d, want %d", r.ExitCode, tc.exitCode)
			}
			if r.Error == nil {
				t.Fatal("a non-success envelope must carry an error member")
			}
			if r.Error.Code != tc.code {
				t.Errorf("error.code = %q, want %q", r.Error.Code, tc.code)
			}
			if r.Error.Message != tc.err.Error() {
				t.Errorf("error.message = %q, want the error's own message %q", r.Error.Message, tc.err.Error())
			}
			assertEnvelopeValid(t, r)
		})
	}
}

// TestNewResultV2_CarriesTheSuggestedNext keeps the recovery hint on the machine
// surface: an agent reads error.next instead of the human "Try:" line.
func TestNewResultV2_CarriesTheSuggestedNext(t *testing.T) {
	err := WithNext(Classify(errors.New("lock needs migration"), ErrInvalidConfig),
		"putnami migrate vnext --apply")
	r := NewResultV2("migrate vnext", nil, err)
	if r.Error == nil || r.Error.Next != "putnami migrate vnext --apply" {
		t.Fatalf("error.next = %+v, want the suggested command", r.Error)
	}
	assertEnvelopeValid(t, r)
}

func TestWriteResultV2_Modes(t *testing.T) {
	r := NewResultV2("doctor", map[string]any{"verdict": "pass"}, nil)

	t.Run("json is indented", func(t *testing.T) {
		var buf bytes.Buffer
		code, err := WriteResultV2(&buf, OutputJSON, r)
		if err != nil || code != ExitSuccess {
			t.Fatalf("WriteResultV2 = (%d, %v)", code, err)
		}
		if !strings.Contains(buf.String(), "\n  \"protocolVersion\": 2") {
			t.Errorf("OutputJSON is not indented:\n%s", buf.String())
		}
		assertEnvelopeBytesValid(t, buf.Bytes())
	})

	t.Run("jsonl is one compact line", func(t *testing.T) {
		var buf bytes.Buffer
		if _, err := WriteResultV2(&buf, OutputJSONL, r); err != nil {
			t.Fatalf("WriteResultV2: %v", err)
		}
		if lines := strings.Count(strings.TrimSpace(buf.String()), "\n"); lines != 0 {
			t.Errorf("OutputJSONL wrote %d extra lines:\n%s", lines, buf.String())
		}
		if !strings.HasPrefix(buf.String(), `{"protocolVersion":2,`) {
			t.Errorf("OutputJSONL did not lead with the version stamp:\n%s", buf.String())
		}
		assertEnvelopeBytesValid(t, buf.Bytes())
	})

	t.Run("text writes nothing", func(t *testing.T) {
		var buf bytes.Buffer
		code, err := WriteResultV2(&buf, OutputText, r)
		if err != nil {
			t.Fatalf("WriteResultV2: %v", err)
		}
		if code != ExitSuccess {
			t.Errorf("exit code = %d, want %d", code, ExitSuccess)
		}
		if buf.Len() != 0 {
			t.Errorf("a non-structured mode wrote %q", buf.String())
		}
	})

	t.Run("the failure exit code is returned", func(t *testing.T) {
		var buf bytes.Buffer
		code, err := WriteResultV2(&buf, OutputJSONL, NewResultV2("doctor", nil, Authf("no token")))
		if err != nil {
			t.Fatalf("WriteResultV2: %v", err)
		}
		if code != ExitAuth {
			t.Errorf("exit code = %d, want %d", code, ExitAuth)
		}
	})
}

func assertEnvelopeValid(t *testing.T, r ResultV2) {
	t.Helper()
	var buf bytes.Buffer
	if _, err := WriteResultV2(&buf, OutputJSONL, r); err != nil {
		t.Fatalf("render envelope: %v", err)
	}
	assertEnvelopeBytesValid(t, buf.Bytes())
}

func assertEnvelopeBytesValid(t *testing.T, data []byte) {
	t.Helper()
	violations := ValidateDocument(DocumentResultEnvelope, bytes.TrimSpace(data))
	if len(violations) == 0 {
		return
	}
	t.Errorf("envelope violates the v2 contract: %v\n%s", violations, data)
}
