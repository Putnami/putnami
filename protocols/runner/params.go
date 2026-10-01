package runner

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
)

// Portable command-parameter types. A parameter's Go type is cache-significant
// on the executing engine, so the wire names the type explicitly instead of
// letting generic JSON decoding turn an integer into a float or a flag into a
// string. Every producer must map its native type onto exactly one of these.
const (
	ParamTypeString  = "string"
	ParamTypeBool    = "bool"
	ParamTypeInt     = "int"
	ParamTypeFloat   = "float"
	ParamTypeStrings = "strings"
	MaxParams        = 256
	MaxParamBytes    = 64 << 10
)

// ParamValue is one typed command parameter. Value holds the Go value the
// executing engine receives: string, bool, int, float64 or []string.
type ParamValue struct {
	// Type is one of string, bool, int, float or strings.
	Type string
	// Value holds the Go value of that type: string, bool, int, float64 or []string.
	Value any
}

type paramWire struct {
	// Type is the declared parameter type name.
	Type string `json:"type"`
	// Value is the raw JSON value decoded according to Type.
	Value json.RawMessage `json:"value"`
}

// MarshalJSON emits the typed shape and refuses a value that does not match
// its declared type, so a canonical request never carries an ambiguous value.
func (p ParamValue) MarshalJSON() ([]byte, error) {
	if err := validateParam(p); err != nil {
		return nil, err
	}
	value, err := json.Marshal(p.Value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(paramWire{Type: p.Type, Value: value})
}

// UnmarshalJSON decodes the typed shape into the exact Go type the type names.
func (p *ParamValue) UnmarshalJSON(data []byte) error {
	if _, err := strictObject(data, []string{"type", "value"}, nil); err != nil {
		return err
	}
	var wire paramWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	value, err := decodeParamValue(wire.Type, wire.Value)
	if err != nil {
		return err
	}
	*p = ParamValue{Type: wire.Type, Value: value}
	return nil
}

func decodeParamValue(kind string, raw json.RawMessage) (any, error) {
	switch kind {
	case ParamTypeString:
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("runner: string parameter: %w", err)
		}
		return value, nil
	case ParamTypeBool:
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("runner: bool parameter: %w", err)
		}
		return value, nil
	case ParamTypeInt:
		var number json.Number
		if err := json.Unmarshal(raw, &number); err != nil || len(raw) == 0 || raw[0] == '"' {
			return nil, fmt.Errorf("runner: int parameter: %w", err)
		}
		value, err := strconv.ParseInt(number.String(), 10, 64)
		if err != nil || int64(int(value)) != value {
			return nil, fmt.Errorf("runner: int parameter %q is not a machine integer", number)
		}
		return int(value), nil
	case ParamTypeFloat:
		var value float64
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("runner: float parameter: %w", err)
		}
		return value, nil
	case ParamTypeStrings:
		var value []string
		if err := json.Unmarshal(raw, &value); err != nil || value == nil {
			return nil, fmt.Errorf("runner: strings parameter must be an array of strings")
		}
		return value, nil
	default:
		return nil, fmt.Errorf("runner: unsupported parameter type %q", kind)
	}
}

// NewParam projects a native Go parameter onto the portable shape. Signed
// integers of every width become int; unsigned and every other type are
// refused so a producer cannot silently ship a value the executing engine
// would hash differently.
func NewParam(value any) (ParamValue, error) {
	switch typed := value.(type) {
	case string:
		return ParamValue{Type: ParamTypeString, Value: typed}, nil
	case bool:
		return ParamValue{Type: ParamTypeBool, Value: typed}, nil
	case int:
		return ParamValue{Type: ParamTypeInt, Value: typed}, nil
	case int64:
		if int64(int(typed)) != typed {
			return ParamValue{}, fmt.Errorf("runner: integer parameter overflows a machine integer")
		}
		return ParamValue{Type: ParamTypeInt, Value: int(typed)}, nil
	case int32:
		return ParamValue{Type: ParamTypeInt, Value: int(typed)}, nil
	case float64:
		return ParamValue{Type: ParamTypeFloat, Value: typed}, nil
	case []string:
		return ParamValue{Type: ParamTypeStrings, Value: append([]string{}, typed...)}, nil
	case []any:
		strings := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return ParamValue{}, fmt.Errorf("runner: list parameter holds a non-string element")
			}
			strings = append(strings, text)
		}
		return ParamValue{Type: ParamTypeStrings, Value: strings}, nil
	default:
		return ParamValue{}, fmt.Errorf("runner: parameter type %T is not portable", value)
	}
}

// NativeParams returns the executing engine's parameter map: exactly the Go
// types the wire named, never a generic JSON projection.
func NativeParams(params map[string]ParamValue) (map[string]any, error) {
	out := make(map[string]any, len(params))
	for name, param := range params {
		if err := validateParam(param); err != nil {
			return nil, fmt.Errorf("runner: parameter %q: %w", name, err)
		}
		if strings, ok := param.Value.([]string); ok {
			out[name] = append([]string{}, strings...)
			continue
		}
		out[name] = param.Value
	}
	return out, nil
}

func validateParam(param ParamValue) error {
	switch param.Type {
	case ParamTypeString:
		if _, ok := param.Value.(string); !ok {
			return fmt.Errorf("string parameter carries %T", param.Value)
		}
	case ParamTypeBool:
		if _, ok := param.Value.(bool); !ok {
			return fmt.Errorf("bool parameter carries %T", param.Value)
		}
	case ParamTypeInt:
		if _, ok := param.Value.(int); !ok {
			return fmt.Errorf("int parameter carries %T", param.Value)
		}
	case ParamTypeFloat:
		value, ok := param.Value.(float64)
		if !ok || math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("float parameter must be a finite float64")
		}
	case ParamTypeStrings:
		if _, ok := param.Value.([]string); !ok {
			return fmt.Errorf("strings parameter carries %T", param.Value)
		}
	default:
		return fmt.Errorf("unsupported parameter type %q", param.Type)
	}
	return nil
}

func validateParams(params map[string]ParamValue) error {
	if params == nil {
		return fmt.Errorf("runner: params must be an object")
	}
	if len(params) > MaxParams {
		return fmt.Errorf("runner: more than %d parameters", MaxParams)
	}
	names := make([]string, 0, len(params))
	for name := range params {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == "" || len(name) > MaxSourcePathBytes {
			return fmt.Errorf("runner: parameter name must be a bounded non-empty string")
		}
		if err := validateParam(params[name]); err != nil {
			return fmt.Errorf("runner: parameter %q: %w", name, err)
		}
		encoded, err := json.Marshal(params[name])
		if err != nil || len(encoded) > MaxParamBytes {
			return fmt.Errorf("runner: parameter %q exceeds %d bytes", name, MaxParamBytes)
		}
	}
	return nil
}
