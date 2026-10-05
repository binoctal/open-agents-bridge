package config

import (
	"fmt"
	"os"
	"strings"
)

// Cloud interactive session mode (cloud-session-shadow-device §5): the runner
// container has no config.json and no pairing. Its whole identity arrives as
// environment variables set by the hosting-agent, and the sst_ session token
// plays the role of the device token on the WS URL and every Bearer call
// (the API recognises the prefix and binds it to exactly one session).
const (
	envSessionToken = "SESSION_TOKEN"
	envSessionID    = "SESSION_ID"
	envUserID       = "USER_ID"
	envShadowDevice = "SHADOW_DEVICE_ID"
	envAPIBaseURL   = "API_BASE_URL"
)

// SessionEnvActive reports whether the process was launched as a cloud
// session container. SESSION_TOKEN alone decides: a half-configured container
// must fail loudly in FromSessionEnv rather than fall back to a device login.
func SessionEnvActive() bool {
	return os.Getenv(envSessionToken) != ""
}

// wsBaseURL maps the API's http(s) base to the ws(s) form Config.ServerURL
// carries: connect() dials it as-is, and every HTTP consumer normalizes it
// back. Passing https:// through fails the dial with "malformed ws or wss URL".
func wsBaseURL(apiBase string) string {
	switch {
	case strings.HasPrefix(apiBase, "https://"):
		return "wss://" + strings.TrimPrefix(apiBase, "https://")
	case strings.HasPrefix(apiBase, "http://"):
		return "ws://" + strings.TrimPrefix(apiBase, "http://")
	}
	return apiBase
}

// FromSessionEnv builds the in-memory config of a session container and wires
// the Claude Code gateway credential into this process's environment (the ACP
// child inherits it). Nothing is written to disk.
func FromSessionEnv() (*Config, error) {
	var missing []string
	get := func(k string) string {
		v := strings.TrimSpace(os.Getenv(k))
		if v == "" {
			missing = append(missing, k)
		}
		return v
	}
	token := get(envSessionToken)
	userID := get(envUserID)
	deviceID := get(envShadowDevice)
	apiBase := strings.TrimRight(get(envAPIBaseURL), "/")
	get(envSessionID)
	if len(missing) > 0 {
		return nil, fmt.Errorf("cloud session environment incomplete: missing %s", strings.Join(missing, ", "))
	}
	if !strings.HasPrefix(token, "sst_") {
		return nil, fmt.Errorf("%s is not a session token", envSessionToken)
	}

	// Gateway mode: the platform LLM outlet speaks the Anthropic Messages API
	// and the sst_ token is the API key (billing anchor). Claude Code must not
	// see any other credential source in this container.
	_ = os.Setenv("ANTHROPIC_BASE_URL", apiBase+"/api/sandbox/llm")
	_ = os.Setenv("ANTHROPIC_AUTH_TOKEN", token)
	_ = os.Setenv("DISABLE_TELEMETRY", "1")
	_ = os.Setenv("DISABLE_ERROR_REPORTING", "1")

	return &Config{
		UserID:      userID,
		DeviceID:    deviceID,
		DeviceToken: token,
		ServerURL:   wsBaseURL(apiBase),
		DeviceName:  "cloud-session",
	}, nil
}
