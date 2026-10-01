package jsonl

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"sync"

	runtime "go.putnami.dev/protocol/runtime"
)

// ForwardPipe reads lines from a pipe and forwards them as JSONL log events.
//
// It is called once per spawned workload, so a serve wrapper that restarts its
// workload runs it again on the new pipes with the SAME emitter. Nothing here
// is stateful across lines or across pipes, which is what makes readiness
// re-announce itself on every restart rather than only on the first start.
func ForwardPipe(emit *Emitter, pipe io.ReadCloser, fallbackLevel string, wg *sync.WaitGroup) {
	defer wg.Done()
	scanner := bufio.NewScanner(pipe)
	for scanner.Scan() {
		ForwardLine(emit, scanner.Text(), fallbackLevel)
	}
}

// forwardReservedKeys are the members of a structured log record the forwarder
// consumes itself instead of passing through as context. severity/message carry
// the event's own fields, timestamp is re-stamped by the emitter, error becomes
// the typed error info, and the readiness marker is a machine channel the
// forwarder turns into a typed event — leaving it in context would print the
// payload back into the user's console.
var forwardReservedKeys = map[string]bool{
	"severity":          true,
	"message":           true,
	"timestamp":         true,
	"error":             true,
	runtime.ReadyLogKey: true,
}

// ForwardLine parses a single output line and re-emits it as a JSONL log event.
// Detects JSON logs with severity+message fields (as produced by the Go framework's
// JSONSink and the TypeScript runtime's JsonSink) and preserves context fields.
//
// A record carrying the reserved readiness marker (runtime.ReadyLogKey)
// additionally produces a typed `ready` event, emitted AFTER the log event so
// the human output ordering — which B6b's watch adapter consumes as the only readiness signal — is
// exactly what it was before readiness was typed. The typed event is dropped
// silently when the stream was not negotiated to v2, which keeps the whole
// mechanism additive.
func ForwardLine(emit *Emitter, line string, fallbackLevel string) {
	if strings.HasPrefix(line, "{") {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err == nil {
			severity, hasSeverity := parsed["severity"].(string)
			message, hasMessage := parsed["message"].(string)
			if hasSeverity && hasMessage {
				level := MapSeverity(severity, fallbackLevel)

				// Extract error field if present
				var errInfo map[string]any
				if errObj, ok := parsed["error"].(map[string]any); ok {
					if _, hasMsg := errObj["message"].(string); hasMsg {
						errInfo = errObj
					}
				}

				// Collect remaining fields as context (exclude reserved keys)
				var context map[string]any
				for k, v := range parsed {
					if forwardReservedKeys[k] {
						continue
					}
					if v == nil {
						continue
					}
					if context == nil {
						context = make(map[string]any)
					}
					context[k] = v
				}

				emit.LogEvent(level, message, context, errInfo)
				forwardReadiness(emit, parsed)
				return
			}
		}
	}
	emit.Log(fallbackLevel, line)
}

// forwardReadiness turns a workload's reserved readiness marker into a typed
// `ready` event. The marker is decoded and validated by the protocol package,
// so a malformed one costs this signal instead of poisoning the stream.
func forwardReadiness(emit *Emitter, record map[string]any) {
	if !emit.SupportsReady() {
		return
	}
	data, ok := runtime.ReadyMarkerFromLogRecord(record)
	if !ok {
		return
	}
	emit.Ready(*data)
}

// MapSeverity normalizes a severity string to a log level.
func MapSeverity(severity, fallback string) string {
	switch strings.ToUpper(severity) {
	case "ERROR":
		return "error"
	case "WARNING", "WARN":
		return "warn"
	case "INFO":
		return "info"
	case "DEBUG":
		return "debug"
	default:
		return fallback
	}
}
