package clicore

import (
	"os/exec"
	"runtime"
)

// OpenBrowser opens url in the operating system's default browser. It starts
// the platform launcher without waiting for the browser process to exit.
func OpenBrowser(url string) error {
	if url == "" {
		return nil
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
