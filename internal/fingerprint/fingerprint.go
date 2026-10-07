// Package fingerprint derives the machine hint sent at pairing time
// (machine-model-ia D3). It is only a duplicate-pairing hint for the user: the
// server never uses it for quota or access decisions, and only a hash leaves
// the machine.
package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"runtime"
	"strings"
)

// machineIDPaths are read in order; the first non-empty one is used.
var machineIDPaths = []string{"/etc/machine-id", "/var/lib/dbus/machine-id"}

// Compute returns a 32-hex-char hash of hostname, OS/arch and the OS machine
// id when one is readable. It matches the server-side format [A-Za-z0-9_-]{8,128}.
func Compute() string {
	host, _ := os.Hostname()
	return hash(host, runtime.GOOS, runtime.GOARCH, readMachineID(machineIDPaths))
}

func hash(parts ...string) string {
	sum := sha256.Sum256([]byte("oa-fp-v1\x00" + strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16])
}

func readMachineID(paths []string) string {
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			if id := strings.TrimSpace(string(b)); id != "" {
				return id
			}
		}
	}
	return ""
}
