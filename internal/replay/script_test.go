package replay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeScript writes raw lines to a temp script file and returns its path.
func writeScript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.jsonl")
	if err := os.WriteFile(path, []byte(joinLines(lines)), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

func joinLines(lines []string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}

func TestLoadScriptParsesHeaderAndFrames(t *testing.T) {
	path := writeScript(t,
		`{"kind":"header","cliType":"claude","adapterVersion":"0.6.2","recordedAt":"2026-09-01","recipe":"local-e2e"}`,
		`{"kind":"frame","seq":0,"dir":"in","frame":{"id":1,"method":"initialize"}}`,
		`{"kind":"frame","seq":1,"dir":"out","after":"initialize","frame":{"id":1,"result":{"protocolVersion":1}}}`,
	)

	script, err := LoadScript(path)
	if err != nil {
		t.Fatalf("LoadScript: %v", err)
	}
	if script.Header.CLIType != "claude" {
		t.Errorf("CLIType = %q, want claude", script.Header.CLIType)
	}
	if len(script.Frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(script.Frames))
	}
	if script.Frames[1].After != "initialize" {
		t.Errorf("frame 1 After = %q, want initialize", script.Frames[1].After)
	}
	var probe struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
	}
	if err := probeJSON(script.Frames[0].Frame, &probe); err != nil {
		t.Fatalf("frame 0 payload: %v", err)
	}
	if probe.Method != "initialize" {
		t.Errorf("frame 0 method = %q, want initialize", probe.Method)
	}
}

func TestLoadScriptStopsReasonVerbatim(t *testing.T) {
	// The turn-end metadata is the evidence the suite exists to protect:
	// stopReason must survive the format untouched (spec scenario
	// "Recording captures turn-end metadata").
	path := writeScript(t,
		`{"kind":"header","cliType":"claude","adapterVersion":"0.6.2","recordedAt":"2026-09-01"}`,
		`{"kind":"frame","seq":0,"dir":"out","frame":{"id":7,"result":{"stopReason":"end_turn"}}}`,
	)
	script, err := LoadScript(path)
	if err != nil {
		t.Fatalf("LoadScript: %v", err)
	}
	var probe struct {
		Result struct {
			StopReason string `json:"stopReason"`
		} `json:"result"`
	}
	if err := probeJSON(script.Frames[0].Frame, &probe); err != nil {
		t.Fatalf("frame payload: %v", err)
	}
	if probe.Result.StopReason != "end_turn" {
		t.Errorf("stopReason = %q, want end_turn", probe.Result.StopReason)
	}
}

func TestLoadScriptRejectsInvalidScripts(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
	}{
		{"empty", nil},
		{"missing header", []string{`{"kind":"frame","seq":0,"dir":"in","frame":{}}`}},
		{"header not first", []string{
			`{"kind":"frame","seq":0,"dir":"in","frame":{}}`,
			`{"kind":"header","cliType":"claude"}`,
		}},
		{"non-monotonic seq", []string{
			`{"kind":"header","cliType":"claude"}`,
			`{"kind":"frame","seq":1,"dir":"in","frame":{}}`,
		}},
		{"invalid direction", []string{
			`{"kind":"header","cliType":"claude"}`,
			`{"kind":"frame","seq":0,"dir":"sideways","frame":{}}`,
		}},
		{"after gate on in frame", []string{
			`{"kind":"header","cliType":"claude"}`,
			`{"kind":"frame","seq":0,"dir":"in","after":"initialize","frame":{}}`,
		}},
		{"unknown kind", []string{
			`{"kind":"header","cliType":"claude"}`,
			`{"kind":"golden","seq":0,"dir":"in","frame":{}}`,
		}},
		{"not json", []string{`{"kind":"header"`, `oops`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LoadScript(writeScript(t, tc.lines...)); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func probeJSON(raw json.RawMessage, v any) error {
	return json.Unmarshal(raw, v)
}
