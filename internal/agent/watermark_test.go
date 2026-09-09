package agent

import (
	"context"
	"errors"
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

// add-cloud-executor 4.6: the watermark carries a running-sandbox count (sbx-
// name prefix) alongside containers/ports. The semantics the API leans on:
// a set pointer (even to 0) means "counted", nil means "unknown" -- a docker
// hiccup must NOT read as "no sandboxes running".
func withFakePS(t *testing.T, out string, err error) {
	t.Helper()
	prev := psDocker
	psDocker = func(ctx context.Context, args ...string) (string, error) { return out, err }
	t.Cleanup(func() { psDocker = prev })
}

func TestCollectWatermarkSandboxCount(t *testing.T) {
	withFakePS(t, "sbx-abc123def456 abc123def456 0.0.0.0:30001->3000/tcp\n"+
		"my-coolify-app def456789abc \n"+
		"sbx-fffe11dd22cc aabbccddeeff \n", nil)
	w := CollectWatermark(context.Background())
	if w.ContainersRunning != 3 {
		t.Fatalf("ContainersRunning = %d, want 3", w.ContainersRunning)
	}
	if w.SandboxesRunning == nil || *w.SandboxesRunning != 2 {
		t.Fatalf("SandboxesRunning = %v, want *2", w.SandboxesRunning)
	}
	if len(w.PortsInUse) != 1 || w.PortsInUse[0] != 30001 {
		t.Fatalf("PortsInUse = %v, want [30001]", w.PortsInUse)
	}
}

// Zero sandboxes is a REAL count and must be sent as 0, not omitted: the
// launch gate treats NULL as unknown (refuse), so conflating the two would
// wedge sandbox dispatch on a quiet node.
func TestCollectWatermarkZeroSandboxesStillCounted(t *testing.T) {
	withFakePS(t, "my-coolify-app def456789abc 0.0.0.0:31234->80/tcp\n", nil)
	w := CollectWatermark(context.Background())
	if w.SandboxesRunning == nil || *w.SandboxesRunning != 0 {
		t.Fatalf("SandboxesRunning = %v, want *0", w.SandboxesRunning)
	}
}

// A docker failure leaves the field nil -> the API stores unknown. A false
// zero here would let the concurrency gate admit launches onto a node whose
// real sandbox count nobody knows.
func TestCollectWatermarkDockerFailureIsUnknown(t *testing.T) {
	withFakePS(t, "", errors.New("docker: command not found"))
	w := CollectWatermark(context.Background())
	if w.SandboxesRunning != nil {
		t.Fatalf("SandboxesRunning = %v, want nil", w.SandboxesRunning)
	}
	if w.ContainersRunning != 0 {
		t.Fatalf("ContainersRunning = %d, want 0", w.ContainersRunning)
	}
}
