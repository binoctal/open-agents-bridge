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
		// NetIO renders decimal SI on docker 26 (staging 5.3 cross-check:
		// "1.46MB"-style tx halves were silently parsed as 0 before this).
		{"1.46MB", 1_460_000, true},
		{"454kB", 454_000, true},
		{"20.7GB", 20_700_000_000, true},
		{"2TB", 2_000_000_000_000, true},
		{"", 0, false},
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

// NetIO tx extraction (launch-plus-plan-guardrails 2.1): the tx half after
// "/" is the cumulative egress counter; every docker size unit must round-trip.
func TestSplitNetIOTx(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"1.5MiB / 3.2GiB", 3435973836, true}, // 3.2 * 1024^3 = 3435973836.8, truncated
		{"0B / 512B", 512, true},
		{"819B / 2048KiB", 2 * 1024 * 1024, true},
		{"1.5MiB / 0B", 0, true},
		{"1.5MiB / 77.3MiB", 81_054_924, true},
		// Live docker-26 shapes (node capture 2026-09-13): NetIO is decimal SI.
		{"53.7MB / 1.46MB", 1_460_000, true},
		{"454kB / 290kB", 290_000, true},
		{"2.48MB / 44.8MB", 44_800_000, true},
		{"1.5MiB /", 0, false},      // empty tx half
		{"1.5MiB 3.2GiB", 0, false}, // no separator
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := splitNetIOTx(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("splitNetIOTx(%q) = %d,%v want %d,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// --- compose label normalization (hosted-compose-stacks 8.13) ----------------

func TestNormalizeCoolifyAppID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Live capture 2026-10-30: compose service label embeds the stack uuid
		// (the row's coolify_application_id) between service name and ordinal.
		{"openfga-6parewue1tdmclhr6o1cudjn-070527871608", "6parewue1tdmclhr6o1cudjn"},
		{"postgres-6parewue1tdmclhr6o1cudjn-070527872186", "6parewue1tdmclhr6o1cudjn"},
		// Underscores are legal in service names.
		{"my_db-6parewue1tdmclhr6o1cudjn-1", "6parewue1tdmclhr6o1cudjn"},
		// A service name that itself ends in a 24-char run: the greedy prefix
		// must still capture the LAST 24-char run before the ordinal.
		{"svc-aaaaaaaaaaaaaaaaaaaaaaaa-6parewue1tdmclhr6o1cudjn-0705", "6parewue1tdmclhr6o1cudjn"},
		// Single-container apps label with the bare app uuid: no -<ordinal>
		// suffix, passes through untouched.
		{"6parewue1tdmclhr6o1cudjn", "6parewue1tdmclhr6o1cudjn"},
		{"owpd4yq2zsewmz1vh5lqmgxx", "owpd4yq2zsewmz1vh5lqmgxx"},
		// Not a compose shape: wrong tail, too-short uuid run, empty.
		{"openfga-6parewue1tdmclhr6o1cudjn", "openfga-6parewue1tdmclhr6o1cudjn"},
		{"app-shortuuid1-12", "app-shortuuid1-12"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeCoolifyAppID(c.in); got != c.want {
			t.Errorf("normalizeCoolifyAppID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- volume quota sampling (hosted-compose-stacks 4.5/5.4) ------------------

func TestParseVolumeSizes(t *testing.T) {
	// Shape captured on dang 2026-10-30: a blank line follows EACH section
	// header -- including "Local Volumes space usage:" -- and the tail "Build
	// cache usage: 0B" line is not a "space usage:" header (its value rides
	// inline), so it falls through to row parsing as a harmless unused key.
	out := `Images space usage:

REPOSITORY   TAG   IMAGE ID   CREATED ago   SIZE   SHARED
nginx        1.27  abc123     2 weeks ago  187MB  0B

Containers space usage:

CONTAINER ID  IMAGE        LOCAL SIZE   RECLAIMABLE
aaa111        nginx:1.27   0B           0B

Local Volumes space usage:

VOLUME NAME                                                        LINKS      SIZE
4e2a1f6b9c3d2e8a7b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b  some-app-1   52.4MB
project_data                                                       some-app-1,worker-2   1.07GB
short_vol                                                          0B

Build cache usage: 0B
`
	got := parseVolumeSizes(out)
	if got["4e2a1f6b9c3d2e8a7b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b"] != 52_400_000 {
		t.Errorf("52.4MB = %d, want 52400000", got["4e2a1f6b9c3d2e8a7b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b"])
	}
	if got["project_data"] != 1_070_000_000 {
		t.Errorf("1.07GB = %d, want 1070000000", got["project_data"])
	}
	if got["short_vol"] != 0 {
		t.Errorf("0B = %d, want 0", got["short_vol"])
	}
}

func TestParseVolumeSizes_EmptySectionYieldsNoSizes(t *testing.T) {
	// A node with no volumes at all, or a CLI whose table moved: both must
	// parse to an empty map so the caller can tell "nothing sized" from
	// "nothing mounted".
	if got := parseVolumeSizes("Images space usage:\nREPOSITORY TAG SIZE\nnginx 1.27 187MB\n"); len(got) != 0 {
		t.Errorf("no volumes section must parse to empty, got %v", got)
	}
}

func TestVolumeTotalsByApp(t *testing.T) {
	// Two apps; the stack's two containers mount disjoint volumes plus one
	// shared -- the union (not the sum) is the app total; the single-container
	// app mounts nothing and totals zero.
	idsByApp := map[string][]string{
		"app-uuid-stack": {"aaa111", "bbb222"},
		"app-uuid-solo":  {"ccc333"},
	}
	volumesByID := map[string][]string{
		"aaa1111111111111111111111111111111111111111111111111111111111111": {"stack_db", "stack_shared"},
		"bbb2222222222222222222222222222222222222222222222222222222222222": {"stack_shared", "stack_cache"},
		"ccc3333333333333333333333333333333333333333333333333333333333333": {},
	}
	sizes := map[string]uint64{"stack_db": 100, "stack_shared": 10, "stack_cache": 5}
	got := volumeTotalsByApp(idsByApp, volumesByID, sizes)
	if got["app-uuid-stack"] != 115 {
		t.Errorf("stack union total = %d, want 115", got["app-uuid-stack"])
	}
	if got["app-uuid-solo"] != 0 {
		t.Errorf("solo total = %d, want 0", got["app-uuid-solo"])
	}
}
