package session

import (
	"os"
	"path/filepath"
	"strings"
)

// ExpandTilde resolves a leading `~` in dir to the device user's home
// directory: `~` becomes the home directory itself and `~/rest` becomes
// home/rest. Absolute and relative paths are returned unchanged. The `~user`
// form (another user's home) is NOT supported — it is returned unchanged and
// will fail at spawn time with a truthful error, which is the documented
// boundary.
//
// Only the bridge (running on the device) knows the real home directory; the
// web/API side cannot expand this, so `~` arrives here verbatim.
func ExpandTilde(dir string) (string, error) {
	if dir != "~" && !strings.HasPrefix(dir, "~/") {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return dir, err
	}
	if dir == "~" {
		return home, nil
	}
	return filepath.Join(home, dir[2:]), nil
}
