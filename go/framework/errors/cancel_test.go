package errors

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// A caller that withdraws is not a server fault: the code it carries, and a bare
// context.Canceled a handler returns, both answer a status under 500.
func TestHTTPStatusForCodeCancelledIsClientClosedRequest(t *testing.T) {
	status, ok := HTTPStatusForCode(CodeCancelled)
	if !ok || status != StatusClientClosedRequest {
		t.Fatalf("HTTPStatusForCode(CodeCancelled) = %d, %v; want %d, true", status, ok, StatusClientClosedRequest)
	}
	if StatusClientClosedRequest != 499 {
		t.Fatalf("StatusClientClosedRequest = %d, want 499", StatusClientClosedRequest)
	}
}

func TestHTTPErrorResponseAnswersCanceledContext(t *testing.T) {
	for name, err := range map[string]error{
		"bare":    context.Canceled,
		"wrapped": fmt.Errorf("load orders: %w", context.Canceled),
		"coded":   New(CodeCancelled, "caller left"),
	} {
		t.Run(name, func(t *testing.T) {
			status, body := HTTPErrorResponse(err)
			if status != StatusClientClosedRequest {
				t.Errorf("status = %d, want %d", status, StatusClientClosedRequest)
			}
			if body.Code != string(CodeCancelled) {
				t.Errorf("code = %q, want %q", body.Code, CodeCancelled)
			}
			if body.Error != "Client Closed Request" {
				t.Errorf("error = %q, want %q", body.Error, "Client Closed Request")
			}
		})
	}
}

// A deadline the server set is a server stall, not a withdrawal: it stays 5xx.
func TestHTTPErrorResponseKeepsDeadlineExceededAServerError(t *testing.T) {
	status, body := HTTPErrorResponse(context.DeadlineExceeded)
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", status)
	}
	if body.Code != string(CodeInternal) {
		t.Errorf("code = %q, want %q", body.Code, CodeInternal)
	}
}
