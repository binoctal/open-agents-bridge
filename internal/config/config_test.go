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

// 5c.3: environment detection matches official hosts exactly, so a URL that
// merely contains "staging" or "localhost" is not mistaken for one.
func TestEnvironmentForURL(t *testing.T) {
	cases := map[string]string{
		"https://api.openagents.top":                  "production",
		"https://API.openagents.top/":                 "production",
		"https://api-staging.openagents.top":          "staging",
		"http://localhost:8989":                       "development",
		"http://127.0.0.1:8989":                       "development",
		"http://[::1]:8989":                           "development",
		"https://evil.com/?staging":                   "custom",
		"https://staging.evil.com":                    "custom",
		"https://api-staging.openagents.top.evil.com": "custom",
		"https://evil.com/api.openagents.top":         "custom",
		"https://localhost.evil.com":                  "custom",
		"https://preview-x.example.com":               "custom",
		"not a url":                                   "custom",
	}
	for raw, want := range cases {
		if got := EnvironmentForURL(raw); got != want {
			t.Errorf("EnvironmentForURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestIsLoopbackURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://localhost:8787":       true,
		"http://127.0.0.1":            true,
		"http://[::1]:8989":           true,
		"https://localhost.evil.com":  false,
		"https://evil.com/?127.0.0.1": false,
		"https://api.openagents.top":  false,
	} {
		if got := IsLoopbackURL(raw); got != want {
			t.Errorf("IsLoopbackURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestGetEnvironmentExplicitWins(t *testing.T) {
	c := &Config{ServerURL: "https://evil.com/?staging"}
	if got := c.GetEnvironment(); got != "custom" {
		t.Errorf("got %q, want custom", got)
	}
	c.Environment = "staging"
	if got := c.GetEnvironment(); got != "staging" {
		t.Errorf("explicit environment ignored: got %q", got)
	}
}
