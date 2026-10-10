package identitycli

import (
	"strings"
	"testing"
)

func TestDeviceInstructionsOmitCompleteLinkWhenBrowserOpens(t *testing.T) {
	device := map[string]any{"verification_uri_complete": "https://control.test/device?code=BCDF-GHJK"}

	withDirectLink := strings.Join(deviceInstructions(device, "https://control.test/device", "BCDF-GHJK", true), "\n")
	if !strings.Contains(withDirectLink, "Or open this link directly:") {
		t.Fatalf("expected complete-link instructions, got:\n%s", withDirectLink)
	}

	withoutDirectLink := strings.Join(deviceInstructions(device, "https://control.test/device", "BCDF-GHJK", false), "\n")
	if strings.Contains(withoutDirectLink, "Or open this link directly:") {
		t.Fatalf("did not expect complete-link instructions, got:\n%s", withoutDirectLink)
	}
}
