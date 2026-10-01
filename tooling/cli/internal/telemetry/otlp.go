package telemetry

import (
	"encoding/json"
	"runtime"
	"strings"
	"time"

	otlp "go.putnami.dev/protocol/telemetry"
)

const telemetryScopeName = "putnami-cli"

// encodeLogs converts the curated local event buffer into one OTLP/JSON logs
// envelope. The caller supplies stable process metadata so the conversion is
// deterministic in tests and independent from the transport.
func encodeLogs(events []Event, fallbackDeviceID, fallbackDeviceIDMonth, version, goos, goarch string) otlp.LogsRequest {
	version = telemetryVersionOrDev(version)
	events = assignBufferedDeviceIDs(events, fallbackDeviceID, fallbackDeviceIDMonth)
	records := make([]otlp.LogRecord, 0, len(events))
	for _, event := range events {
		record, ok := eventLogRecord(event, version, goos, goarch)
		if ok {
			records = append(records, record)
		}
	}

	return otlp.LogsRequest{ResourceLogs: []otlp.ResourceLogs{{
		Resource: otlp.ResourceFromStrings(map[string]string{
			otlp.AttrPutnamiFramework: otlp.FrameworkGo,
			otlp.AttrServiceName:      "putnami-cli",
			otlp.AttrServiceVersion:   version,
		}),
		ScopeLogs: []otlp.ScopeLogs{{
			Scope:      otlp.Scope{Name: telemetryScopeName, Version: version},
			LogRecords: records,
		}},
	}}}
}

func telemetryVersionOrDev(version string) string {
	if version = strings.TrimSpace(version); version == "" {
		return "dev"
	}
	return version
}

func eventLogRecord(event Event, version, goos, goarch string) (otlp.LogRecord, bool) {
	timestamp, err := time.Parse(time.RFC3339, event.Timestamp)
	if err != nil || timestamp.UnixNano() < 0 || event.Name == "" {
		return otlp.LogRecord{}, false
	}

	attrs := make(map[string]otlp.AnyValue, len(event.Data)+5)
	for key, value := range event.Data {
		if converted, ok := telemetryValue(value); ok {
			attrs[key] = converted
		}
	}
	// Static process attributes intentionally win over any legacy buffer field
	// with the same name.
	attrs["event.name"] = otlp.StringVal(event.Name)
	attrs["device.id"] = otlp.StringVal(event.DeviceID)
	attrs["cli.version"] = otlp.StringVal(version)
	attrs["os"] = otlp.StringVal(goos)
	attrs["arch"] = otlp.StringVal(goarch)

	body := otlp.StringVal(event.Name)
	return otlp.LogRecord{
		TimeUnixNano:   otlp.FormatUint(uint64(timestamp.UnixNano())),
		SeverityNumber: otlp.SeverityInfo,
		SeverityText:   "info",
		Body:           &body,
		Attributes:     attrsFromValues(attrs),
	}, true
}

// assignBufferedDeviceIDs preserves the ID captured with new records. Buffers
// written before Event carried an ID cannot reconstruct their original random
// value, so they receive an ephemeral, distinct ID per UTC event month instead
// of retroactively sharing the current month's ID.
func assignBufferedDeviceIDs(events []Event, fallbackDeviceID, fallbackDeviceIDMonth string) []Event {
	assigned := make([]Event, len(events))
	legacyByMonth := make(map[string]string)
	for i, event := range events {
		if event.DeviceID == "" {
			month, ok := eventUTCMonth(event.Timestamp)
			if ok && month == fallbackDeviceIDMonth && fallbackDeviceID != "" {
				event.DeviceID = fallbackDeviceID
			} else if ok {
				deviceID := legacyByMonth[month]
				if deviceID == "" {
					deviceID = generateDeviceID()
					legacyByMonth[month] = deviceID
				}
				event.DeviceID = deviceID
			}
		}
		assigned[i] = event
	}
	return assigned
}

func eventUTCMonth(timestamp string) (string, bool) {
	parsed, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return "", false
	}
	return parsed.UTC().Format("2006-01"), true
}

// telemetryValue converts the local JSONL values to the scalar OTLP subset.
// Commands are the only curated collection; joining their closed vocabulary
// preserves the values without introducing a non-protocol array type.
func telemetryValue(value any) (otlp.AnyValue, bool) {
	switch typed := value.(type) {
	case string:
		return otlp.StringVal(typed), true
	case bool:
		return otlp.BoolVal(typed), true
	case int:
		return otlp.IntVal(int64(typed)), true
	case int64:
		return otlp.IntVal(typed), true
	case float64:
		return otlp.DoubleVal(typed), true
	case json.Number:
		if integer, err := typed.Int64(); err == nil {
			return otlp.IntVal(integer), true
		}
		if decimal, err := typed.Float64(); err == nil {
			return otlp.DoubleVal(decimal), true
		}
	case []string:
		return otlp.StringVal(strings.Join(typed, ",")), true
	case []any:
		commands := make([]string, 0, len(typed))
		for _, item := range typed {
			command, ok := item.(string)
			if !ok {
				return otlp.AnyValue{}, false
			}
			commands = append(commands, command)
		}
		return otlp.StringVal(strings.Join(commands, ",")), true
	}
	return otlp.AnyValue{}, false
}

func attrsFromValues(values map[string]otlp.AnyValue) []otlp.KeyValue {
	attrs := make([]otlp.KeyValue, 0, len(values))
	for key, value := range values {
		attrs = append(attrs, otlp.Attr(key, value))
	}
	return otlp.SortAttrs(attrs)
}

func encodeBufferedLogs(events []Event, deviceID, deviceIDMonth, version string) ([]byte, error) {
	return otlp.MarshalCanonical(encodeLogs(events, deviceID, deviceIDMonth, version, runtime.GOOS, runtime.GOARCH))
}
