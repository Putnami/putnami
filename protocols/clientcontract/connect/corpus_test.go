package connect

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The Connect protocol conformance corpus is transcribed from the published
// specification, not from any implementation, and it already arbitrates the
// TypeScript half of this epic. Reading the same file here is what makes the Go
// provider, the Go client and the TypeScript pair answerable to one document
// instead of to three hand-transcriptions of it.
//
// The path is relative because the corpus is test data, not a dependency: a Go
// module cannot require a TypeScript package, and copying the file would
// recreate exactly the drift it exists to prevent. A move breaks this test
// loudly, which is the intended failure.
const connectCorpusPath = "../../../typescript/framework/application/test/grpc/connect-conformance/corpus.json"

type connectCorpus struct {
	Source struct {
		Specification   string `json:"specification"`
		URL             string `json:"url"`
		ProtocolVersion string `json:"protocolVersion"`
	} `json:"source"`
	Codes []struct {
		Code       string `json:"code"`
		HTTPStatus int    `json:"httpStatus"`
		GRPCNumber int    `json:"grpcNumber"`
	} `json:"codes"`
	HTTPInference []struct {
		Status int    `json:"status"`
		Code   string `json:"code"`
	} `json:"httpInference"`
	Headers struct {
		ProtocolVersion struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"protocolVersion"`
		Timeout struct {
			Name string `json:"name"`
		} `json:"timeout"`
		StreamingContentEncoding struct {
			Name string `json:"name"`
		} `json:"streamingContentEncoding"`
		StreamingAcceptEncoding struct {
			Name string `json:"name"`
		} `json:"streamingAcceptEncoding"`
	} `json:"headers"`
	ContentTypes struct {
		UnaryJSON             string `json:"unaryJson"`
		UnaryProto            string `json:"unaryProto"`
		StreamJSON            string `json:"streamJson"`
		StreamProto           string `json:"streamProto"`
		UnaryErrorContentType string `json:"unaryErrorContentType"`
	} `json:"contentTypes"`
	Timeouts []struct {
		Raw    string `json:"raw"`
		Expect *int64 `json:"expect"`
	} `json:"timeouts"`
	Envelope struct {
		HeaderBytes int `json:"headerBytes"`
		Flags       []struct {
			Mask int    `json:"mask"`
			Name string `json:"name"`
		} `json:"flags"`
		ReservedMask int `json:"reservedMask"`
		Golden       struct {
			Name     string `json:"name"`
			Flags    int    `json:"flags"`
			Payload  string `json:"payload"`
			BytesHex string `json:"bytesHex"`
		} `json:"golden"`
	} `json:"envelope"`
	Errors []struct {
		Name   string          `json:"name"`
		JSON   json.RawMessage `json:"json"`
		Expect *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"expect"`
	} `json:"errors"`
	EndStream []struct {
		Name   string          `json:"name"`
		JSON   json.RawMessage `json:"json"`
		Expect *struct {
			OK   bool   `json:"ok"`
			Code string `json:"code"`
		} `json:"expect"`
	} `json:"endStream"`
}

func loadConnectCorpus(t *testing.T) connectCorpus {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(connectCorpusPath))
	if err != nil {
		t.Fatalf("read the shared Connect corpus: %v", err)
	}
	var corpus connectCorpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("the shared Connect corpus is not readable: %v", err)
	}
	if corpus.Source.ProtocolVersion != ProtocolVersion {
		t.Fatalf("the corpus transcribes protocol version %q, this runtime speaks %q",
			corpus.Source.ProtocolVersion, ProtocolVersion)
	}
	return corpus
}

// Every constant this runtime spells out is the one the corpus transcribes.
func TestConnectCorpusPinsTheProtocolConstants(t *testing.T) {
	corpus := loadConnectCorpus(t)

	if len(corpus.Codes) != len(connectStatuses) {
		t.Fatalf("the corpus lists %d codes, this runtime declares %d", len(corpus.Codes), len(connectStatuses))
	}
	for _, declared := range corpus.Codes {
		resolved, listed := StatusByName(declared.Code)
		if !listed {
			t.Errorf("the corpus declares code %q, which this runtime does not", declared.Code)
			continue
		}
		if resolved.Status != declared.HTTPStatus || resolved.Number != declared.GRPCNumber {
			t.Errorf("code %q = status %d / number %d, corpus says %d / %d",
				declared.Code, resolved.Status, resolved.Number, declared.HTTPStatus, declared.GRPCNumber)
		}
	}
	for _, inferred := range corpus.HTTPInference {
		if got := InferredCode(inferred.Status); got != inferred.Code {
			t.Errorf("InferredCode(%d) = %q, corpus says %q", inferred.Status, got, inferred.Code)
		}
	}

	for _, header := range []struct {
		name string
		got  string
		want string
	}{
		{"protocol version", ProtocolVersionHeader, corpus.Headers.ProtocolVersion.Name},
		{"protocol version value", ProtocolVersion, corpus.Headers.ProtocolVersion.Value},
		{"timeout", TimeoutHeader, corpus.Headers.Timeout.Name},
		{"streaming content encoding", ContentEncoding, corpus.Headers.StreamingContentEncoding.Name},
		{"streaming accept encoding", AcceptEncoding, corpus.Headers.StreamingAcceptEncoding.Name},
		{"unary json", UnaryJSONContentType, corpus.ContentTypes.UnaryJSON},
		{"unary proto", UnaryProtoContentType, corpus.ContentTypes.UnaryProto},
		{"stream json", StreamJSONContentType, corpus.ContentTypes.StreamJSON},
		{"stream proto", StreamProtoType, corpus.ContentTypes.StreamProto},
	} {
		if header.got != header.want {
			t.Errorf("%s = %q, corpus says %q", header.name, header.got, header.want)
		}
	}
}

// Connect-Timeout-Ms is a positive integer of at most ten digits, with no
// surrounding space. The corpus enumerates the accepted and refused forms.
func TestConnectCorpusPinsTheTimeoutGrammar(t *testing.T) {
	corpus := loadConnectCorpus(t)
	if len(corpus.Timeouts) == 0 {
		t.Fatal("the corpus declares no timeout scenes")
	}
	for _, scene := range corpus.Timeouts {
		value, present, err := ParseTimeout(scene.Raw)
		if scene.Expect == nil {
			if err == nil && present {
				t.Errorf("ParseTimeout(%q) accepted %d; the corpus refuses it", scene.Raw, value)
			}
			continue
		}
		if err != nil || !present || value != *scene.Expect {
			t.Errorf("ParseTimeout(%q) = %d, %v, %v; corpus says %d", scene.Raw, value, present, err, *scene.Expect)
		}
	}
}

// The envelope is a 5-byte prefix with two meaningful flag bits and six
// reserved ones, and the corpus carries the specification's own golden bytes.
func TestConnectCorpusPinsTheEnvelopeLayout(t *testing.T) {
	corpus := loadConnectCorpus(t)
	if corpus.Envelope.HeaderBytes != connectEnvelopePrefix {
		t.Errorf("envelope prefix = %d, corpus says %d", connectEnvelopePrefix, corpus.Envelope.HeaderBytes)
	}
	if byte(corpus.Envelope.ReservedMask) != connectFlagReserved {
		t.Errorf("reserved mask = %#x, corpus says %#x", connectFlagReserved, corpus.Envelope.ReservedMask)
	}
	for _, flag := range corpus.Envelope.Flags {
		want := byte(flag.Mask)
		got := byte(0)
		switch flag.Name {
		case "compressed":
			got = FlagCompressed
		case "endStream":
			got = FlagEndStream
		default:
			t.Errorf("the corpus declares an envelope flag %q this runtime does not name", flag.Name)
			continue
		}
		if got != want {
			t.Errorf("flag %s = %#x, corpus says %#x", flag.Name, got, want)
		}
	}
	golden := AppendEnvelope(nil, byte(corpus.Envelope.Golden.Flags), []byte(corpus.Envelope.Golden.Payload))
	if hex.EncodeToString(golden) != corpus.Envelope.Golden.BytesHex {
		t.Errorf("%s = %s\ncorpus says %s", corpus.Envelope.Golden.Name, hex.EncodeToString(golden), corpus.Envelope.Golden.BytesHex)
	}
}

// Only the sixteen codes are valid, and the corpus enumerates the error and
// end-of-stream documents a conforming peer accepts.
func TestConnectCorpusPinsTheErrorAndTerminalDocuments(t *testing.T) {
	corpus := loadConnectCorpus(t)
	for _, scene := range corpus.Errors {
		t.Run("error/"+scene.Name, func(t *testing.T) {
			var document ErrorEnvelope
			decodeErr := json.Unmarshal(scene.JSON, &document)
			_, listed := StatusByName(document.Code)
			valid := decodeErr == nil && listed
			if scene.Expect == nil {
				if valid {
					t.Errorf("this runtime accepts %s; the corpus refuses it", scene.JSON)
				}
				return
			}
			if !valid {
				t.Fatalf("this runtime refuses %s; the corpus accepts it", scene.JSON)
			}
			if document.Code != scene.Expect.Code || document.Message != scene.Expect.Message {
				t.Errorf("decoded = %q/%q, corpus says %q/%q", document.Code, document.Message, scene.Expect.Code, scene.Expect.Message)
			}
		})
	}
	for _, scene := range corpus.EndStream {
		t.Run("endStream/"+scene.Name, func(t *testing.T) {
			// The three rules the runtime applies, stated here against the
			// corpus: the document parses; a named `error` member that is not
			// an error is invalid; and a failure names one of the sixteen codes.
			var end EndStreamResponse
			decodeErr := json.Unmarshal(scene.JSON, &end)
			named := jsonMentionsError(scene.JSON)
			var code string
			ok := true
			valid := decodeErr == nil && !(named && end.Error == nil)
			if end.Error != nil {
				resolved, listed := StatusByName(end.Error.Code)
				valid, ok, code = listed, false, resolved.Name
			}
			if scene.Expect == nil {
				if valid {
					t.Errorf("this runtime accepts %s; the corpus refuses it", scene.JSON)
				}
				return
			}
			if !valid {
				t.Fatalf("this runtime refuses %s; the corpus accepts it", scene.JSON)
			}
			if ok != scene.Expect.OK {
				t.Fatalf("outcome ok = %v, corpus says %v", ok, scene.Expect.OK)
			}
			if !ok && code != scene.Expect.Code {
				t.Errorf("terminal code = %q, corpus says %q", code, scene.Expect.Code)
			}
		})
	}
}

// jsonMentionsError reports whether a terminal document names an `error` member
// at all. `{"error": null}` decodes to a nil error, and the specification calls
// it invalid rather than a success.
func jsonMentionsError(document json.RawMessage) bool {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(document, &members); err != nil {
		return false
	}
	_, named := members["error"]
	return named
}
