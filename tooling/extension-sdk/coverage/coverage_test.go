package coverage

import "testing"

func TestCheckThreshold_DisabledWhenZero(t *testing.T) {
	if ok, msg := CheckThreshold(0, 0, false); !ok || msg != "" {
		t.Errorf("threshold 0 should pass; got ok=%v msg=%q", ok, msg)
	}
}

func TestCheckThreshold_DisabledWhenNegative(t *testing.T) {
	if ok, _ := CheckThreshold(-1, 0, true); !ok {
		t.Errorf("negative threshold should pass")
	}
}

func TestCheckThreshold_AboveThreshold(t *testing.T) {
	if ok, msg := CheckThreshold(80, 92.3, true); !ok || msg != "" {
		t.Errorf("coverage above threshold should pass; got ok=%v msg=%q", ok, msg)
	}
}

func TestCheckThreshold_ExactlyAtThreshold(t *testing.T) {
	if ok, _ := CheckThreshold(80, 80, true); !ok {
		t.Errorf("coverage equal to threshold should pass")
	}
}

func TestCheckThreshold_BoundaryRounding(t *testing.T) {
	// A value a hair below the threshold due to float rounding should still pass.
	if ok, _ := CheckThreshold(80, 80-1e-12, true); !ok {
		t.Errorf("coverage within epsilon of threshold should pass")
	}
}

func TestCheckThreshold_BelowThreshold(t *testing.T) {
	ok, msg := CheckThreshold(80, 79.4, true)
	if ok {
		t.Fatalf("coverage below threshold should fail")
	}
	if msg == "" {
		t.Errorf("expected a failure message")
	}
}

func TestCheckThreshold_NoCoverageData(t *testing.T) {
	ok, msg := CheckThreshold(80, 0, false)
	if ok {
		t.Fatalf("threshold set without coverage data should fail")
	}
	if msg == "" {
		t.Errorf("expected a failure message")
	}
}
