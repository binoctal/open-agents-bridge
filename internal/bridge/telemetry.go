package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/url"
	"os"
	"sync"
)

// Handshake telemetry (bridge-abuse-hardening 5b.5). The bridge reports its
// version (`v`) and the SHA256 of its own executable (`sha`) on the WS
// upgrade. Both are TELEMETRY ONLY: a modified bridge controls both strings,
// so the server uses them to measure the version / official-build share and
// must never gate anything on them. Failure to compute the digest simply
// omits the parameter.

var (
	selfSHAOnce  sync.Once
	selfSHAValue string
)

// selfSHA256 returns the lowercase hex SHA256 of the running executable, or
// "" when it cannot be read. Computed once per process.
func selfSHA256() string {
	selfSHAOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			return
		}
		selfSHAValue = fileSHA256(exe)
	})
	return selfSHAValue
}

func fileSHA256(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// addTelemetryParams sets the `v` and `sha` query parameters. sha is omitted
// when empty.
func addTelemetryParams(q url.Values, version, sha string) {
	if version != "" {
		q.Set("v", version)
	}
	if sha != "" {
		q.Set("sha", sha)
	}
}
