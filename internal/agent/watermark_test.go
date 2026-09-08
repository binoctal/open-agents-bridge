package agent

import (
	"strconv"
	"testing"
)

// S3 (fix-seed-audit-blockers): the observed-ports watermark is what lets the
// API's allocation scan see containers its own table knows nothing about --
// two live build_failed incidents came from exactly that blind spot. The
// regex is the whole parser, so pin its behavior against real `docker ps
// --format {{.Ports}}` shapes.
func TestHostPortRe(t *testing.T) {
	cases := []struct {
		line string
		want []int
	}{
		// Common shapes: IPv4 and IPv6 publications. A dual-stack publication
		// matches twice -- CollectWatermark's seen map does the dedup, the
		// regex reports what is on the wire.
		{"abc123 0.0.0.0:30001->3000/tcp", []int{30001}},
		{"abc123 0.0.0.0:30001->3000/tcp, [::]:30001->3000/tcp", []int{30001, 30001}},
		{"abc123 0.0.0.0:31234->80/tcp 0.0.0.0:31235->443/tcp", []int{31234, 31235}},
		// Container publishing nothing: no match, caller still counts the line.
		{"abc123 ", nil},
		// UDP publications count too -- a bound host port is bound.
		{"abc123 0.0.0.0:30010->53/udp", []int{30010}},
		// Non-published forwarding syntax must not match.
		{"abc123 3000/tcp", nil},
		{"abc123 some-prefix:3000", nil},
	}
	for _, tc := range cases {
		got := []int{}
		for _, m := range hostPortRe.FindAllStringSubmatch(tc.line, -1) {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatalf("%q: unparsable port %q", tc.line, m[1])
			}
			got = append(got, n)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("%q: got %v, want %v", tc.line, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%q: got %v, want %v", tc.line, got, tc.want)
			}
		}
	}
}
