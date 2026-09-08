package agent

import "testing"

func TestParseDockerSize(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"77.3MiB", 81_054_924, true}, // 77.3 * 1048576 = 81054924.8, truncated
		{"512B", 512, true},
		{"1.5GiB", 1610612736, true},
		{"2048KiB", 2 * 1024 * 1024, true},
		{"0B", 0, true},
		{"", 0, false},
		{"77.3 MB", 0, false}, // decimal units never appear in docker stats
		{"abc", 0, false},
	}
	for _, c := range cases {
		got, ok := parseDockerSize(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parseDockerSize(%q) = %d,%v want %d,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// 77.3MiB is 81,056,481.28 bytes; uint64 truncation must stay stable so the
// byte value the API receives never drifts with float rounding modes.
func TestParseDockerSizeMiBExact(t *testing.T) {
	got, ok := parseDockerSize("1MiB")
	if !ok || got != 1048576 {
		t.Fatalf("1MiB = %d,%v want 1048576,true", got, ok)
	}
}
