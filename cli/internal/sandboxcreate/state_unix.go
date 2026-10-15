//go:build !windows

package sandboxcreate

import (
	"os"
	"path/filepath"
)

// cliStateDir is this package's state directory, the XDG default:
// $XDG_STATE_HOME/discobox/cli, else ~/.local/state/discobox/cli. Empty when
// this machine gives no home, in which case create records are disabled.
func cliStateDir() string {
	if value := os.Getenv("XDG_STATE_HOME"); value != "" {
		return filepath.Join(value, "discobox", "cli")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state", "discobox", "cli")
}
