// Package schema embeds the public payload and report JSON Schemas extracted
// from the service OpenAPI. The site compares its hosted files with these bytes.
package schema

import _ "embed"

// PayloadFile and ReportFile are the published file names.
const (
	PayloadFile = "payload.v1.json"
	ReportFile  = "report.v1.json"
)

// PayloadV1 is the JSON Schema of contract.Payload.
//
//go:embed payload.v1.json
var PayloadV1 []byte

// ReportV1 is the JSON Schema of contract.Report.
//
//go:embed report.v1.json
var ReportV1 []byte
