package upload

import (
	"context"
	"os/exec"
	"runtime"
	"time"
)

// browserTimeout bounds the command that hands a URL to the browser.
const browserTimeout = 10 * time.Second

// OpenBrowser hands url to the desktop's browser: open on macOS, rundll32 on
// Windows, xdg-open elsewhere.
func OpenBrowser(url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), browserTimeout)
	defer cancel()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", url)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", url)
	}
	return cmd.Run()
}
