package telemetry

import "testing"

func TestSeverityNumberFor(t *testing.T) {
	cases := []struct {
		level string
		want  SeverityNumber
	}{
		{"trace", SeverityTrace},
		{"debug", SeverityDebug},
		{"info", SeverityInfo},
		{"warn", SeverityWarn},
		{"warning", SeverityWarn}, // alias resolves to the same number
		{"error", SeverityError},
		{"fatal", SeverityFatal},
		{"", 0},     // unknown maps to the OTLP "unspecified" 0
		{"nope", 0}, // unknown level
		{"INFO", 0}, // case-sensitive: uppercase is not a known level
	}
	for _, c := range cases {
		if got := SeverityNumberFor(c.level); got != c.want {
			t.Errorf("SeverityNumberFor(%q) = %d, want %d", c.level, got, c.want)
		}
	}
}

func TestBoolVal(t *testing.T) {
	for _, b := range []bool{true, false} {
		v := BoolVal(b)
		if v.BoolValue == nil {
			t.Fatalf("BoolVal(%v).BoolValue is nil", b)
		}
		if *v.BoolValue != b {
			t.Errorf("BoolVal(%v).BoolValue = %v, want %v", b, *v.BoolValue, b)
		}
		if v.StringValue != nil || v.IntValue != nil || v.DoubleValue != nil {
			t.Errorf("BoolVal(%v) set a non-bool field: %+v", b, v)
		}
	}
}

func TestDoubleVal(t *testing.T) {
	for _, f := range []float64{0, -1.5, 3.14159} {
		v := DoubleVal(f)
		if v.DoubleValue == nil {
			t.Fatalf("DoubleVal(%v).DoubleValue is nil", f)
		}
		if *v.DoubleValue != f {
			t.Errorf("DoubleVal(%v).DoubleValue = %v, want %v", f, *v.DoubleValue, f)
		}
		if v.StringValue != nil || v.IntValue != nil || v.BoolValue != nil {
			t.Errorf("DoubleVal(%v) set a non-double field: %+v", f, v)
		}
	}
}
