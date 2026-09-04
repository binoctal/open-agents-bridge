package config

import (
	"encoding/json"
	"testing"
)

func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

func boolPtr(v bool) *bool { return &v }

// PreviewBuildEffective is a three-state toggle (preview-hosting-ux-parity
// D4): the whole point of the pointer is that "key absent" and "key false"
// mean different things. Pin all three states so a future refactor back to
// a plain bool fails here instead of silently re-introducing the opt-in
// default that ate previews.
func TestPreviewBuildEffectiveThreeState(t *testing.T) {
	cases := []struct {
		name string
		cfg  *bool
		want bool
	}{
		{"unconfigured defaults ON", nil, true},
		{"explicit true", boolPtr(true), true},
		{"explicit false honored", boolPtr(false), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{PreviewBuildEnabled: tc.cfg}
			if got := c.PreviewBuildEffective(); got != tc.want {
				t.Fatalf("PreviewBuildEffective() = %v, want %v", got, tc.want)
			}
		})
	}
}

// JSON round-trip of the omitted key: an absent previewBuildEnabled must
// unmarshal to nil (default ON), and an explicit false must survive.
func TestPreviewBuildEnabledJSONSemantics(t *testing.T) {
	var without Config
	if err := jsonUnmarshal([]byte(`{}`), &without); err != nil {
		t.Fatal(err)
	}
	if without.PreviewBuildEnabled != nil {
		t.Fatalf("absent key should leave nil, got %v", *without.PreviewBuildEnabled)
	}

	var off Config
	if err := jsonUnmarshal([]byte(`{"previewBuildEnabled":false}`), &off); err != nil {
		t.Fatal(err)
	}
	if off.PreviewBuildEnabled == nil || *off.PreviewBuildEnabled {
		t.Fatal("explicit false must unmarshal to a false pointer")
	}
}
