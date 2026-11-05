package cli

import (
	"fmt"
	"testing"
)

func TestWithResultDataKeepsTheErrorAndCarriesTheData(t *testing.T) {
	t.Parallel()
	if WithResultData(nil, "data") != nil {
		t.Fatal("WithResultData(nil, data) is not nil")
	}
	cause := Usagef("bad flag")
	err := fmt.Errorf("check: %w", WithResultData(cause, map[string]int{"problems": 2}))
	if err.Error() != "check: bad flag" {
		t.Errorf("message = %q, want the wrapped error's", err.Error())
	}
	data, ok := ResultData(err).(map[string]int)
	if !ok || data["problems"] != 2 {
		t.Errorf("ResultData = %#v, want the attached payload", ResultData(err))
	}
	if ResultData(cause) != nil || ResultData(nil) != nil {
		t.Error("ResultData of an error without data is not nil")
	}
}
