package shared

import (
	"encoding/json"
	"testing"
)

// TestRecordsData pins the data of a list result: an empty list is [], one
// record is the record itself, and more records are an array in order.
func TestRecordsData(t *testing.T) {
	t.Parallel()
	type record struct {
		ID string `json:"id"`
	}
	for _, tc := range []struct {
		records []record
		want    string
	}{
		{nil, "[]"},
		{[]record{{ID: "a"}}, `{"id":"a"}`},
		{[]record{{ID: "a"}, {ID: "b"}}, `[{"id":"a"},{"id":"b"}]`},
	} {
		got, err := json.Marshal(RecordsData(tc.records))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Errorf("RecordsData(%v) = %s, want %s", tc.records, got, tc.want)
		}
	}
}
