// Package coolify is the hosting-agent's REST client for the same-host
// Coolify instance (add-dynamic-preview-compute 5.2).
//
// Ported from the API worker's coolify-client.ts (add-coolify-hosting 3.1),
// which encodes the S3 spike's measured 4.3.17 workarounds -- the create
// flow's two-step URL dance and the PATCH field shapes are load-bearing and
// must stay identical on both clients. The Go side exists because runtime
// preview apps are created and driven by the agent, not the API worker
// (design D5: per-mission prebuilt app, the agent owns the lifecycle).
package coolify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client talks to the Coolify REST API with a static bearer token.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New builds a client. Empty baseURL or token means "not configured"; callers
// keep that distinct from "configured but broken" (the TS client's rule).
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// Configured reports whether the drive can be used at all.
func (c *Client) Configured() bool { return c.BaseURL != "" && c.Token != "" }

// Error carries the HTTP status so callers can branch on 404 etc.
type Error struct {
	Status   int
	Endpoint string
	Body     string
}

func (e *Error) Error() string {
	return fmt.Sprintf("coolify %s -> %d: %s", e.Endpoint, e.Status, truncate(e.Body, 2000))
}

// NotFound reports whether err is a Coolify 404.
func NotFound(err error) bool {
	var ce *Error
	return errors.As(err, &ce) && ce.Status == http.StatusNotFound
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Application is the subset of a Coolify application the agent reads.
type Application struct {
	UUID          string `json:"uuid"`
	Name          string `json:"name"`
	FQDN          string `json:"fqdn"`
	PortsExposes  string `json:"ports_exposes"`
	PortsMappings string `json:"ports_mappings"`
	Status        string `json:"status"`
}

// Deployment is one ApplicationDeploymentQueue row.
type Deployment struct {
	UUID   string `json:"uuid"`
	Status string `json:"status"`
	Logs   string `json:"logs"`
}

func (c *Client) request(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("coolify marshal %s: %w", path, err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("coolify %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &Error{Status: resp.StatusCode, Endpoint: method + " " + path, Body: string(raw)}
	}
	if out == nil {
		return nil
	}
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &Error{Status: resp.StatusCode, Endpoint: method + " " + path, Body: "non-JSON response: " + truncate(string(raw), 200)}
	}
	return nil
}

// Fixed name so the placeholder key is created once and reused (same literal
// as the TS client -- one key per Coolify instance, shared by both clients).
const placeholderKeyName = "open-agents-loopback-placeholder"

// A throwaway ed25519 key committed on purpose: private-deploy-key apps
// require a private_key_uuid even when the repo is plain http (no auth). The
// key is public knowledge and grants nothing.
const placeholderPrivateKey = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACA6jGN87Mt47Wyi/y9Kvy2qHpz+PGeDfl+jxK09NTBvRAAAAKAdzYMOHc2D
DgAAAAtzc2gtZWQyNTUxOQAAACA6jGN87Mt47Wyi/y9Kvy2qHpz+PGeDfl+jxK09NTBvRA
AAAEAZnH7kqeYotwU+L9vea8ndvlrycarwEBtzCW5+Kp2FqzqMY3zsy3jtbKL/L0q/Laoe
nP48Z4N+X6PErT01MG9EAAAAF29wZW4tYWdlbnRzLXBsYWNlaG9sZGVyAQIDBAUG
-----END OPENSSH PRIVATE KEY-----
`

// placeholderKeyUUID returns the placeholder deploy-key uuid, creating the
// key on first use (idempotent by name).
func (c *Client) placeholderKeyUUID(ctx context.Context) (string, error) {
	var keys []struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	}
	if err := c.request(ctx, http.MethodGet, "/api/v1/security/keys", nil, &keys); err != nil {
		return "", err
	}
	for _, k := range keys {
		if k.Name == placeholderKeyName {
			return k.UUID, nil
		}
	}
	var created struct {
		UUID string `json:"uuid"`
	}
	err := c.request(ctx, http.MethodPost, "/api/v1/security/keys", map[string]any{
		"name":        placeholderKeyName,
		"description": "placeholder for loopback http git (no auth needed)",
		"private_key": placeholderPrivateKey,
	}, &created)
	if err != nil {
		return "", err
	}
	return created.UUID, nil
}

// ApplicationInput is what CreateApplication needs at creation time.
type ApplicationInput struct {
	ProjectUUID     string
	ServerUUID      string
	EnvironmentName string
	// GitRepository is the docker0-gateway http URL of the agent-served repo.
	GitRepository string
	GitBranch     string
	// BuildPack is "dockerfile" for runtime previews (the agent writes the
	// Dockerfile into the tree) or "nixpacks" for hosted deployments.
	BuildPack    string
	PortsExposes int
	Domains      string
	LimitsMemory string
}

// CreateApplication creates an app pointing at the agent's git http-backend,
// per the S3 spike's measured flow (same workarounds as the TS client):
//
//  1. placeholder key via /security/keys -- private-deploy-key apps demand a
//     private_key_uuid even for unauthenticated http git.
//  2. POST /applications/private-deploy-key -- the only create endpoint that
//     accepts an arbitrary git URL. ValidGitRepositoryUrl rejects localhost /
//     127.0.0.1 / bare IPs as a string check, so the initial URL must use a
//     hostname; host.docker.internal passes.
//  3. PATCH /applications/{uuid} -- rewrite git_repository to the docker0
//     gateway (git fetch runs inside the coolify-helper container, which
//     cannot resolve host.docker.internal). The PATCH path carries no
//     ValidGitRepositoryUrl check; everything else rides the same PATCH.
//
// Coolify derives the application NAME from the git repository basename, so
// repos named oa-preview-{missionId}.git surface as oa-preview-{missionId}.
func (c *Client) CreateApplication(ctx context.Context, in ApplicationInput) (string, error) {
	keyUUID, err := c.placeholderKeyUUID(ctx)
	if err != nil {
		return "", fmt.Errorf("placeholder key: %w", err)
	}
	hostnameURL, err := hostnameForm(in.GitRepository)
	if err != nil {
		return "", err
	}
	var app struct {
		UUID string `json:"uuid"`
	}
	err = c.request(ctx, http.MethodPost, "/api/v1/applications/private-deploy-key", map[string]any{
		"project_uuid":     in.ProjectUUID,
		"server_uuid":      in.ServerUUID,
		"environment_name": in.EnvironmentName,
		"private_key_uuid": keyUUID,
		"git_repository":   hostnameURL,
		"git_branch":       in.GitBranch,
		"build_pack":       in.BuildPack,
		"instant_deploy":   false,
	}, &app)
	if err != nil {
		return "", err
	}
	if err := c.UpdateApplication(ctx, app.UUID, in); err != nil {
		return "", err
	}
	return app.UUID, nil
}

// hostnameForm rewrites http://{ip}:{port}/x.git to
// http://host.docker.internal:{port}/x.git (the create-time validation
// workaround; the PATCH later restores the real gateway IP).
func hostnameForm(gitRepository string) (string, error) {
	u, err := url.Parse(gitRepository)
	if err != nil {
		return "", fmt.Errorf("parse git url %q: %w", gitRepository, err)
	}
	out := "http://host.docker.internal"
	if p := u.Port(); p != "" {
		out += ":" + p
	}
	return out + u.Path, nil
}

// UpdateApplication re-asserts the settings the deployment depends on
// (create or reuse): 4.3.17 keeps stale values on existing applications
// until they are explicitly PATCHed.
func (c *Client) UpdateApplication(ctx context.Context, applicationUUID string, in ApplicationInput) error {
	return c.request(ctx, http.MethodPatch, "/api/v1/applications/"+url.PathEscape(applicationUUID), map[string]any{
		"git_repository": in.GitRepository,
		"domains":        in.Domains,
		"ports_exposes":  fmt.Sprintf("%d", in.PortsExposes),
		// ports_exposes only EXPOSEs in the container; without a mapping
		// Coolify publishes nothing to the host. Same value on both sides
		// keeps the allocated host slot == app port.
		"ports_mappings":         fmt.Sprintf("%d:%d", in.PortsExposes, in.PortsExposes),
		"limits_memory":          in.LimitsMemory,
		"is_auto_deploy_enabled": false,
	}, nil)
}

// ListApplications returns all applications (the name scan's input).
func (c *Client) ListApplications(ctx context.Context) ([]Application, error) {
	var apps []Application
	if err := c.request(ctx, http.MethodGet, "/api/v1/applications", nil, &apps); err != nil {
		return nil, err
	}
	return apps, nil
}

// GetApplication returns nil on 404 (deleted/unknown uuid).
func (c *Client) GetApplication(ctx context.Context, applicationUUID string) (*Application, error) {
	var app Application
	err := c.request(ctx, http.MethodGet, "/api/v1/applications/"+url.PathEscape(applicationUUID), nil, &app)
	if err != nil {
		if NotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &app, nil
}

// Deploy triggers a (re)deployment and returns the queue's deployment uuid.
// POST /deploy with a JSON body -- /applications/{uuid}/deploy does not exist.
func (c *Client) Deploy(ctx context.Context, applicationUUID string, force bool) (string, error) {
	var out struct {
		Deployments []struct {
			DeploymentUUID string `json:"deployment_uuid"`
		} `json:"deployments"`
	}
	err := c.request(ctx, http.MethodPost, "/api/v1/deploy", map[string]any{
		"uuid":          applicationUUID,
		"force_rebuild": force,
	}, &out)
	if err != nil {
		return "", err
	}
	if len(out.Deployments) == 0 || out.Deployments[0].DeploymentUUID == "" {
		return "", &Error{Status: 200, Endpoint: "deploy", Body: "no deployment_uuid in response"}
	}
	return out.Deployments[0].DeploymentUUID, nil
}

// GetDeployment returns nil on 404 -- the queue row ages out after success;
// callers treat nil as "unknown", never as success/failure on its own.
func (c *Client) GetDeployment(ctx context.Context, deploymentUUID string) (*Deployment, error) {
	var d Deployment
	err := c.request(ctx, http.MethodGet, "/api/v1/deployments/"+url.PathEscape(deploymentUUID), nil, &d)
	if err != nil {
		if NotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &d, nil
}

// Stop / Start / Restart must be POST -- GET returns post_required.
func (c *Client) Stop(ctx context.Context, applicationUUID string) error {
	return c.request(ctx, http.MethodPost, "/api/v1/applications/"+url.PathEscape(applicationUUID)+"/stop", nil, nil)
}

func (c *Client) Start(ctx context.Context, applicationUUID string) error {
	return c.request(ctx, http.MethodPost, "/api/v1/applications/"+url.PathEscape(applicationUUID)+"/start", nil, nil)
}

// DeleteApplication hard-deletes the application and its containers.
func (c *Client) DeleteApplication(ctx context.Context, applicationUUID string) error {
	return c.request(ctx, http.MethodDelete, "/api/v1/applications/"+url.PathEscape(applicationUUID), nil, nil)
}

// --- Projects (coolify-project-per-user) -----------------------------------

// PerUserProjectName is the per-user project naming convention, identical to
// the API worker's perUserProjectName (apps/api coolify-client.ts).
func PerUserProjectName(userID string) string { return "oa-user-" + userID }

// projectUUIDByName caches resolved projects for this process (a project's
// uuid never changes once created); a restart rebuilds it at one list call.
var projectUUIDByName sync.Map

// ListProjects returns the team's projects (uuid + name only).
func (c *Client) ListProjects(ctx context.Context) ([]struct{ UUID, Name string }, error) {
	var raw []struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	}
	if err := c.request(ctx, http.MethodGet, "/api/v1/projects", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]struct{ UUID, Name string }, 0, len(raw))
	for _, p := range raw {
		out = append(out, struct{ UUID, Name string }{p.UUID, p.Name})
	}
	return out, nil
}

// CreateProject makes a new project. Coolify auto-creates the default
// `production` environment via the Project model's created hook, so callers
// keep EnvironmentName "production" with no extra call.
func (c *Client) CreateProject(ctx context.Context, name string) (string, error) {
	var created struct {
		UUID string `json:"uuid"`
	}
	err := c.request(ctx, http.MethodPost, "/api/v1/projects", map[string]any{
		"name":        name,
		"description": "per-user placement (coolify-project-per-user)",
	}, &created)
	if err != nil {
		return "", err
	}
	return created.UUID, nil
}

// EnsureProject resolves the named project, creating it when missing. Exact
// name match; a concurrent double-create can leave a redundant same-name
// project (accepted, design D1). Errors propagate -- callers must fail the
// deployment rather than fall back to the shared project (design D5).
func (c *Client) EnsureProject(ctx context.Context, name string) (string, error) {
	if cached, ok := projectUUIDByName.Load(name); ok {
		return cached.(string), nil
	}
	projects, err := c.ListProjects(ctx)
	if err != nil {
		return "", err
	}
	for _, p := range projects {
		if p.Name == name {
			projectUUIDByName.Store(name, p.UUID)
			return p.UUID, nil
		}
	}
	uuid, err := c.CreateProject(ctx, name)
	if err != nil {
		return "", err
	}
	projectUUIDByName.Store(name, uuid)
	return uuid, nil
}
