package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
)

// accessLogLine is the slice of the Caddy JSON access-log envelope the idle
// detection needs.
type accessLogLine struct {
	Request struct {
		Host string `json:"host"`
	} `json:"request"`
}

// hostTrafficDelta reads the [offset, size) window of a per-instance Caddy
// access log and reports whether any COMPLETE line belongs to host.
//
// The files are not exclusive to their instance: the named logger's include
// filter ("http.log.access") also captures every request Caddy served for
// hosts WITHOUT a mapped logger, so raw byte growth only means "some site got
// traffic" -- bot scans hitting sibling subdomains would keep every preview
// alive forever (observed live: analytics/xuantie/coolify lines inside a
// preview's log within minutes). Only lines whose request.host matches count.
//
// The returned offset advances to the end of the last complete line: a torn
// trailing line (mid-write) is left for the next poll.
func hostTrafficDelta(path string, offset int64, host string) (hit bool, newOffset int64) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, offset
	}
	size := fi.Size()
	if size < offset {
		// Recreated after a reload: re-baseline, count nothing.
		return false, size
	}
	if size == offset {
		return false, offset
	}
	f, err := os.Open(path)
	if err != nil {
		return false, offset
	}
	defer f.Close()
	data, err := io.ReadAll(io.NewSectionReader(f, offset, size-offset))
	if err != nil {
		return false, size
	}
	if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
		data = data[:i+1]
	} else {
		data = nil // only a torn line in this window
	}
	want := strings.ToLower(host)
	for _, line := range bytes.Split(data, []byte("\n")) {
		var l accessLogLine
		if len(line) == 0 {
			continue
		}
		if json.Unmarshal(line, &l) == nil && strings.ToLower(l.Request.Host) == want {
			hit = true
		}
	}
	return hit, offset + int64(len(data))
}
