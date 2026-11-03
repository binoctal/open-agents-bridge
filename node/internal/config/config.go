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

	"github.com/binoctal/open-agents/hosting-agent/internal/register"
)

// Endpoint is one API environment the agent serves. The single ECS node hosts
// both staging and production Coolify projects, so the agent dual-homes
// (dual-home-hosting-agent design D1): every loop iterates the endpoints and
// every work-item report routes back to the endpoint that offered the item.
type Endpoint struct {
	// BaseURL is that environment's API worker origin,
	// e.g. https://api-staging.openagents.top.
	BaseURL string

	// InternalSecret is that environment's X-Internal-Secret value. The two
	// environments keep independent secrets (each is a worker-side wrangler
	// secret), so the credential travels per endpoint, not globally.
	InternalSecret string

	// CoolifyProjectUUID is this environment's project for the runtime
	// preview static placement fallback (items without a userId). Empty
	// disables the static fallback for that endpoint; per-user placement
	// (coolify-project-per-user) still works.
	CoolifyProjectUUID string
}

// Config is the full runtime configuration of the hosting-agent.
type Config struct {
	// AgentID identifies this agent instance to the API (single-node today;
	// kept explicit so Coolify multi-server later does not change the wire
	// contract).
	AgentID string

	// Endpoints is every API environment this agent serves. Always non-empty
	// after FromEnv validates. Populated from API_ENDPOINTS, or -- as the
	// backward-compatible shorthand -- a single entry from API_BASE_URL +
	// INTERNAL_SECRET (+ the global COOLIFY_PROJECT_UUID).
	Endpoints []Endpoint

	// InternalSecret is the legacy single-endpoint shorthand value (set only
	// when API_ENDPOINTS is unset). Deprecated: read Endpoints instead.
	InternalSecret string

	// APIBaseURL is the legacy single-endpoint shorthand value (set only when
	// API_ENDPOINTS is unset). Deprecated: read Endpoints instead.
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

	// WorkPollIdleMaxSec caps the idle backoff of the work poll (see
	// agent.Agent.WorkIdleMax). <= WorkPollIntervalSec disables it.
	// Default 60s: an idle node settles at ~1 poll/55s (the effective gap is
	// cap - base/2, see noteWork), which keeps worst-case pickup latency for a
	// newly queued deploy at ~1 minute while cutting the account-wide Worker
	// request burn roughly in half versus a 30s cap.
	WorkPollIdleMaxSec int

	// CoolifyBaseURL is the Coolify API origin (default http://127.0.0.1:8000 —
	// same-host Coolify on the ECS node). Empty disables runtime previews
	// (local dev / tests have no Coolify).
	CoolifyBaseURL string

	// CoolifyAPIToken authenticates against the Coolify API. Required when
	// CoolifyBaseURL is set — a missing token would turn every runtime_up into
	// a silent 401 loop, same failure class as the missing internal secret.
	CoolifyAPIToken string

	// CoolifyProjectUUID / CoolifyServerUUID / CoolifyEnvironmentName place
	// the agent-created runtime preview applications inside the existing
	// Coolify project (design D5: per-mission prebuilt app oa-preview-{id}).
	CoolifyProjectUUID     string
	CoolifyServerUUID      string
	CoolifyEnvironmentName string

	// RuntimePreviewPortMin/Max bound the runtime preview host port range
	// (32100-32199 by default; disjoint from hosted 30000-31999 and git http
	// 24100-24199, so no cross-allocator coordination).
	RuntimePreviewPortMin int
	RuntimePreviewPortMax int

	// RuntimePreviewIdleTTLMinutes is the zero-access window after which a
	// running preview is idle-stopped (design D3, default 30).
	RuntimePreviewIdleTTLMinutes int

	// RuntimePreviewLogDir holds Caddy per-host access logs. Must be writable
	// by the caddy user (the spike found root-owned /var/log rejected); the
	// default is Coolify's caddy-owned /var/lib/caddy.
	RuntimePreviewLogDir string

	// SessionPreviewIdleTTLMinutes is the session tier's zero-access window
	// (default 30). Separate from the runtime tier's knob so one tier can
	// tighten without moving the other.
	SessionPreviewIdleTTLMinutes int

	// NodeCreds is set when the agent runs in USER-NODE mode (byo-hosting-node
	// D1): credentials written by `register --code`, one endpoint (the
	// platform the code was redeemed against), no Coolify, no shared secret.
	// Nil = platform mode (the ECS agent, unchanged).
	NodeCreds *register.Credentials

	// ScratchRoot is the only directory tree same-machine declare-only
	// manifest entries (localPath) may reference (byo-hosting-node D3). Every
	// local path is checked against escape BEFORE any read; entries outside
	// refuse the materialize.
	ScratchRoot string

	// AskListenAddr is the localhost listener Caddy's on_demand_tls ask
	// callback hits (user-node mode only; empty = "127.0.0.1:20190").
	AskListenAddr string

	// CaddyfilePath is where the user-node mode writes its Caddy scaffold
	// (on-demand TLS + ask callback + wildcard catch-all) for the operator's
	// systemd caddy unit to run. Empty = "/etc/hosting-agent/Caddyfile".
	CaddyfilePath string
}

// FromEnv builds a Config from environment variables, applying defaults and
// validating the mandatory ones. Validation order matters: the endpoints
// first, so a misconfigured deployment fails with the most actionable
// message.
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
		WorkPollIdleMaxSec:   envInt("WORK_POLL_IDLE_MAX_SEC", 60),

		CoolifyBaseURL:               strings.TrimRight(envOr("COOLIFY_BASE_URL", "http://127.0.0.1:8000"), "/"),
		CoolifyAPIToken:              os.Getenv("COOLIFY_API_TOKEN"),
		CoolifyProjectUUID:           os.Getenv("COOLIFY_PROJECT_UUID"),
		CoolifyServerUUID:            os.Getenv("COOLIFY_SERVER_UUID"),
		CoolifyEnvironmentName:       envOr("COOLIFY_ENVIRONMENT_NAME", "production"),
		RuntimePreviewPortMin:        envInt("RUNTIME_PREVIEW_PORT_MIN", 32100),
		RuntimePreviewPortMax:        envInt("RUNTIME_PREVIEW_PORT_MAX", 32199),
		RuntimePreviewIdleTTLMinutes: envInt("RUNTIME_PREVIEW_IDLE_TTL_MINUTES", 30),
		RuntimePreviewLogDir:         envOr("RUNTIME_PREVIEW_LOG_DIR", "/var/lib/caddy/oa-runtime"),

		SessionPreviewIdleTTLMinutes: envInt("SESSION_PREVIEW_IDLE_TTL_MINUTES", 30),

		ScratchRoot:   envOr("HOSTING_AGENT_SCRATCH_ROOT", "/var/lib/hosting-agent/scratch"),
		AskListenAddr: envOr("HOSTING_AGENT_ASK_ADDR", "127.0.0.1:20190"),
		CaddyfilePath: envOr("HOSTING_AGENT_CADDYFILE", "/etc/hosting-agent/Caddyfile"),
	}

	// User-node mode (byo-hosting-node D1): the credentials file IS the
	// configuration -- one platform, node-credential auth, no Coolify, no
	// endpoint list. Checked before resolveEndpoints so a user machine needs
	// none of the platform's mandatory env.
	if p := strings.TrimSpace(os.Getenv("HOSTING_NODE_CREDENTIALS")); p != "" {
		creds, err := register.LoadCredentials(p)
		if err != nil {
			return nil, err
		}
		c.NodeCreds = creds
		c.AgentID = creds.NodeID
		c.Endpoints = []Endpoint{{BaseURL: creds.APIBaseURL}}
		// Caddy on the user node is agent-managed on-demand TLS (D2); the
		// admin endpoint default is the same localhost:2019 convention.
		return c, nil
	}

	endpoints, err := resolveEndpoints(c.CoolifyProjectUUID)
	if err != nil {
		return nil, err
	}
	c.Endpoints = endpoints

	switch {
	case c.GitPortMin >= c.GitPortMax:
		return nil, fmt.Errorf("GIT_HTTP_PORT_MIN (%d) must be below GIT_HTTP_PORT_MAX (%d)", c.GitPortMin, c.GitPortMax)
	case c.RuntimePreviewPortMin >= c.RuntimePreviewPortMax:
		return nil, fmt.Errorf("RUNTIME_PREVIEW_PORT_MIN (%d) must be below RUNTIME_PREVIEW_PORT_MAX (%d)", c.RuntimePreviewPortMin, c.RuntimePreviewPortMax)
	}
	return c, nil
}

// resolveEndpoints builds the endpoint list from API_ENDPOINTS (dual-home)
// or, when unset, from the legacy API_BASE_URL + INTERNAL_SECRET shorthand.
// API_ENDPOINTS is ';'-separated; each entry is
// base_url|internal_secret[|coolify_project_uuid] with '|' as the field
// separator (URLs cannot contain '|', secrets in this deployment class are
// opaque tokens). The project field is optional: empty disables that
// endpoint's static preview fallback.
func resolveEndpoints(globalProjectUUID string) ([]Endpoint, error) {
	raw := strings.TrimSpace(os.Getenv("API_ENDPOINTS"))
	if raw == "" {
		// Legacy shorthand: exactly one endpoint, project = the global UUID.
		if os.Getenv("INTERNAL_SECRET") == "" {
			return nil, fmt.Errorf("no API endpoint configured: set API_ENDPOINTS or INTERNAL_SECRET")
		}
		if os.Getenv("API_BASE_URL") == "" {
			return nil, fmt.Errorf("no API endpoint configured: set API_ENDPOINTS or API_BASE_URL")
		}
		return []Endpoint{{
			BaseURL:            strings.TrimRight(os.Getenv("API_BASE_URL"), "/"),
			InternalSecret:     os.Getenv("INTERNAL_SECRET"),
			CoolifyProjectUUID: globalProjectUUID,
		}}, nil
	}
	var endpoints []Endpoint
	seen := map[string]bool{}
	for i, entry := range strings.Split(raw, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, "|")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, fmt.Errorf("API_ENDPOINTS entry %d: want base_url|internal_secret[|coolify_project_uuid], got %d fields", i+1, len(parts))
		}
		ep := Endpoint{
			BaseURL:            strings.TrimRight(strings.TrimSpace(parts[0]), "/"),
			InternalSecret:     strings.TrimSpace(parts[1]),
			CoolifyProjectUUID: "",
		}
		if len(parts) == 3 {
			ep.CoolifyProjectUUID = strings.TrimSpace(parts[2])
		}
		switch {
		case ep.BaseURL == "":
			return nil, fmt.Errorf("API_ENDPOINTS entry %d: base URL is empty", i+1)
		case ep.InternalSecret == "":
			return nil, fmt.Errorf("API_ENDPOINTS entry %d (%s): internal secret is empty", i+1, ep.BaseURL)
		case seen[ep.BaseURL]:
			// The same URL twice with (presumably) different secrets is a
			// pasted-file accident, not a feature.
			return nil, fmt.Errorf("API_ENDPOINTS entry %d: duplicate base URL %s", i+1, ep.BaseURL)
		}
		seen[ep.BaseURL] = true
		endpoints = append(endpoints, ep)
	}
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("API_ENDPOINTS is set but contains no endpoints")
	}
	return endpoints, nil
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
