package clientcontract

import (
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"time"
)

// The snapshot page transport uses the same query names in every domain.
const (
	QueryParamRelation = "relation"
	QueryParamAfterKey = "afterKey"
	QueryParamLimit    = "limit"
)

// PageQuery selects one relation and an opaque keyset cursor. An absent limit
// uses the owner's default; an absent afterKey starts the relation.
type PageQuery struct {
	Relation string `json:"relation" validate:"required"`
	AfterKey string `json:"afterKey,omitempty"`
	Limit    int32  `json:"limit,omitempty" validate:"min=1"`
}

// Page is the shared snapshot envelope. A provider specializes Rows with its
// domain encoding; the reusable transport uses json.RawMessage. Watermark is
// read BEFORE rows and consumers anchor a multi-page image at the minimum
// observed watermark. OwnerConfirmedAt is that owner's clock, never receipt
// time; its zero value is omitted when the owner does not provide confirmation.
// Only an absent or empty NextKey ends a relation, even on a short page.
type Page[Rows any] struct {
	Watermark        int64     `json:"watermark" validate:"required,min=0"`
	OwnerConfirmedAt time.Time `json:"ownerConfirmedAt,omitempty,omitzero"`
	Relation         string    `json:"relation" validate:"required"`
	Rows             Rows      `json:"rows" validate:"required"`
	NextKey          string    `json:"nextKey,omitempty"`
}

// PageTransportSchemas is the owner-neutral shape a domain pins in a
// conformance test. The owner can narrow relation, rows and input bounds.
func PageTransportSchemas() (query, envelope Schema) {
	zero := json.Number("0")
	one := json.Number("1")
	query = Schema{Type: "object", Properties: map[string]Schema{
		QueryParamRelation: {Type: "string"},
		QueryParamAfterKey: {Type: "string"},
		QueryParamLimit:    {Type: "integer", Format: "int32", Minimum: &one},
	}, Required: []string{QueryParamRelation}}
	envelope = Schema{Type: "object", Properties: map[string]Schema{
		"watermark":        {Type: "integer", Format: "int64", Minimum: &zero},
		"ownerConfirmedAt": {Type: "string", Format: "date-time"},
		"relation":         {Type: "string"},
		"rows":             {OpaqueJSON: OpaqueJSONAny},
		"nextKey":          {Type: "string"},
	}, Required: []string{"watermark", "relation", "rows"}}
	return query, envelope
}

// ValidatePageTransportSchemas pins the shared query/envelope names and wire
// types, while leaving relation vocabulary, row encoding and stricter bounds
// to the owner. Schemas must be resolved before this check. Runtime conformance
// (relation echo, cursor progress and authoritative freshness) remains the
// domain's responsibility; a schema cannot prove when an owner read its clock.
func ValidatePageTransportSchemas(query, envelope Schema) error {
	wantQuery, wantEnvelope := PageTransportSchemas()
	for _, pair := range []struct {
		name             string
		actual, expected Schema
	}{
		{"query", query, wantQuery}, {"envelope", envelope, wantEnvelope},
	} {
		if diagnostics := ValidateSchema(&pair.actual); len(diagnostics) > 0 {
			return fmt.Errorf("page %s: %s", pair.name, diagnostics[0].String())
		}
		if pair.actual.Type != "object" || pair.actual.Ref != "" || pair.actual.Nullable != nil && *pair.actual.Nullable || len(pair.actual.Properties) != len(pair.expected.Properties) {
			return fmt.Errorf("page %s must declare exactly the shared properties", pair.name)
		}
		for _, name := range pair.expected.Required {
			if !slices.Contains(pair.actual.Required, name) {
				return fmt.Errorf("page %s.%s must be required", pair.name, name)
			}
		}
		for _, name := range slices.Sorted(maps.Keys(pair.expected.Properties)) {
			want := pair.expected.Properties[name]
			got, exists := pair.actual.Properties[name]
			if !exists {
				return fmt.Errorf("page %s.%s is missing", pair.name, name)
			}
			if name == "rows" {
				continue
			}
			if got.Type != want.Type || got.Format != want.Format || got.Ref != "" || got.Nullable != nil && *got.Nullable {
				return fmt.Errorf("page %s.%s has an incompatible wire type", pair.name, name)
			}
			if want.Minimum != nil {
				if got.Minimum == nil {
					return fmt.Errorf("page %s.%s must retain its minimum", pair.name, name)
				}
				minimum, ok := new(big.Rat).SetString(got.Minimum.String())
				floor, _ := new(big.Rat).SetString(want.Minimum.String())
				if !ok || minimum.Cmp(floor) < 0 {
					return fmt.Errorf("page %s.%s weakens its minimum", pair.name, name)
				}
			}
		}
	}
	return nil
}
