package shared

import "testing"

func TestAppendUnique(t *testing.T) {
	s := []string{"a", "b"}
	s = AppendUnique(s, "b")
	if len(s) != 2 {
		t.Errorf("AppendUnique should not duplicate: got %v", s)
	}
	s = AppendUnique(s, "c")
	if len(s) != 3 {
		t.Errorf("AppendUnique should add new: got %v", s)
	}
}
