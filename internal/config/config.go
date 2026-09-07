// Package config loads hosting-agent settings from the environment.
//
// The agent is systemd-managed infrastructure on the ECS node
// (add-coolify-hosting, design D2): all secrets arrive via an EnvironmentFile
// (0600 root), same operational pattern as node-runner before it. A missing
// INTERNAL_SECRET must fail loud at boot -- the known incident class is a
// silently absent secret turning every API call into a 403 (memory:
// bridge-callbacks-403-without-an-undeliverable-secret).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the full runtime configuration of the hosting-agent.
type Config struct {
	// AgentID identifies this agent instance to the API (single-node today;
	// kept explicit so Coolify multi-server later does not change the wire
	// contract).
	AgentID string

	// InternalSecret is the shared infra secret (X-Internal-Secret header).
	InternalSecret string

	// APIBaseURL is the API worker origin, e.g. https://api-staging.openagents.top.
	APIBaseURL string

	// CacheDir holds content-addressed R2 objects and bare repos
	// (default /var/lib/hosting-agent).
	CacheDir string

	// CaddyAdminAddr is the localhost admin API endpoint (default 127.0.0.1:2019).
	// Empty disables route wiring (local dev has no Caddy).
	CaddyAdminAddr string

	// DomainSuffix builds the public host for a deployment label
	// (design D7: deploy.openagents.top).
	DomainSuffix string

	// GitBindHost is what the per-deployment git http listeners bind. Default
	// is the docker0 gateway: Coolify's git fetch runs inside the
	// coolify-helper container, which cannot reach the host's loopback
	// (S3 spike). Override with 127.0.0.1 for local dev/tests.
	GitBindHost string

	// GitPortMin/Max bound the per-deployment git http port range.
	GitPortMin int
	GitPortMax int

	// WorkPollInterval is how often the agent asks the API for work items.
	WorkPollIntervalSec int

	// WatermarkInterval is how often node capacity is reported (design D6
	// input: reject-new decisions live in the API).
	WatermarkIntervalSec int
}

// FromEnv builds a Config from environment variables, applying defaults and
// validating the mandatory ones. Validation order matters: the secret first,
// so a misconfigured deployment fails with the most actionable message.
func FromEnv() (*Config, error) {
	c := &Config{
		AgentID:              envOr("HOSTING_AGENT_ID", "primary"),
		InternalSecret:       os.Getenv("INTERNAL_SECRET"),
		APIBaseURL:           strings.TrimRight(os.Getenv("API_BASE_URL"), "/"),
		CacheDir:             envOr("CACHE_DIR", "/var/lib/hosting-agent"),
		CaddyAdminAddr:       envOr("CADDY_ADMIN_ADDR", "127.0.0.1:2019"),
		DomainSuffix:         envOr("DEPLOY_DOMAIN_SUFFIX", "deploy.openagents.top"),
		GitBindHost:          envOr("GIT_HTTP_BIND", "172.17.0.1"),
		GitPortMin:           envInt("GIT_HTTP_PORT_MIN", 24100),
		GitPortMax:           envInt("GIT_HTTP_PORT_MAX", 24199),
		WorkPollIntervalSec:  envInt("WORK_POLL_INTERVAL_SEC", 10),
		WatermarkIntervalSec: envInt("WATERMARK_INTERVAL_SEC", 60),
	}

	switch {
	case c.InternalSecret == "":
		return nil, fmt.Errorf("INTERNAL_SECRET is required")
	case c.APIBaseURL == "":
		return nil, fmt.Errorf("API_BASE_URL is required")
	case c.GitPortMin >= c.GitPortMax:
		return nil, fmt.Errorf("GIT_HTTP_PORT_MIN (%d) must be below GIT_HTTP_PORT_MAX (%d)", c.GitPortMin, c.GitPortMax)
	}
	return c, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
