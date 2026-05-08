package importer

import (
	"os/exec"
	"runtime"
)

// openBrowser fires off the user's default browser pointed at url.
// Best-effort: returns the underlying error if the OS-specific
// launcher couldn't be spawned, but never blocks.
//
// Picking the launcher per-OS instead of via a dependency keeps
// the importer's transitive deps small. If a future host needs to
// override (e.g., an SSH tunnel that routes through a different
// host's browser), expose a Prompter method instead.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
