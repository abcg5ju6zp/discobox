//go:build windows

package sandboxcreate

import (
	"os"
	"path/filepath"
)

// cliStateDir is this package's state directory under %LOCALAPPDATA%, the
// per-machine state home Windows conventions name. Empty when it cannot be
// found, in which case create records are disabled.
func cliStateDir() string {
	if value := os.Getenv("LOCALAPPDATA"); value != "" {
		return filepath.Join(value, "discobox", "cli")
	}
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		return filepath.Join(dir, "discobox", "cli")
	}
	return ""
}
