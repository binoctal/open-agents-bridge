// Package api is the hosting-agent's transport to the API worker.
//
// Wire contract (agent-facing half of add-coolify-hosting task 3.2; the API
// side implements these routes in apps/api/src/routes — keep both in lockstep):
//
//	GET  /internal/hosting-agent/work            -> {items: [WorkItem]}
//	POST /internal/hosting-agent/presign-source  {deploymentId, shas} -> PresignResponse
//	POST /internal/hosting-agent/report          {deploymentId, event, repoPort, detail}
//	POST /internal/hosting-agent/watermark       Watermark
//	POST /internal/hosting-agent/usage          {agentId, samples: [UsageSample]}
//
// Every request carries X-Internal-Secret. The agent polls for work because
// the API worker cannot reach the ECS node directly (same reason node-runner
// used a poll + WS-push hybrid; polling alone is enough at this scale).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"time"
)

// Client talks to the API worker's /internal/hosting-agent/* endpoints.
type Client struct {
	BaseURL        string
	AgentID        string
	InternalSecret string
	HTTP           *http.Client
}

// New builds a client with sane timeouts.
func New(baseURL, agentID, internalSecret string) *Client {
	return &Client{
		BaseURL:        baseURL,
		AgentID:        agentID,
		InternalSecret: internalSecret,
		HTTP:           &http.Client{Timeout: 30 * time.Second},
	}
}

// File mirrors the source manifest entry the bridge declared (same shape the
// API validates in hosted-deployments.ts).
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Work actions. materialize carries the manifest; register_route carries the
// app port Coolify exposed; release tears the deployment's local footprint;
// probe is the ready-alive TCP dial (fix-seed-audit-blockers S4: a
// crash-looped container looks ready from the API side, so the API asks and
// the agent observes). sandbox_up / sandbox_destroy (add-cloud-executor 4.1)
// start and tear down a runsc-isolated cloud sandbox; they ride the same poll
// but are keyed by sandboxId, not deploymentId.
const (
	ActionMaterialize    = "materialize"
	ActionRegisterRoute  = "register_route"
	ActionRemoveRoute    = "remove_route"
	ActionRelease        = "release"
	ActionProbe          = "probe"
	ActionSandboxUp      = "sandbox_up"
	ActionSandboxDestroy = "sandbox_destroy"
)

// SizeClass is the tier payload of a sandbox_up item: the cgroup limits are
// generated from these on the node (vcpu -> --cpus, memory -> --memory,
// storage -> --tmpfs). Keep in lockstep with SandboxWorkItem in
// apps/api/src/services/cloud-sandboxes.ts.
type SizeClass struct {
	ID         string  `json:"id"`
	VCPU       float64 `json:"vcpu"`
	MemoryMiB  int     `json:"memoryMib"`
	StorageMiB int     `json:"storageMib"`
}

// WorkItem is one unit of work the API wants this agent to perform.
type WorkItem struct {
	DeploymentID string `json:"deploymentId"`
	MissionID    string `json:"missionId"`
	Action       string `json:"action"`

	// Host is the public hostname for register_route ({label}.{suffix}).
	Host string `json:"host,omitempty"`
	// AppPort is the app's local port for register_route.
	AppPort int `json:"appPort,omitempty"`

	// ManifestURL and SourcePrefix are set for materialize. The manifest is
	// NOT inlined: D1's 100KB statement cap cannot hold a real tree, so the
	// API stores it as deploys/{id}/manifest.json and hands out a presigned
	// GET (15min) -- the agent fetches it before touching the store.
	ManifestURL  string `json:"manifestUrl,omitempty"`
	SourcePrefix string `json:"sourcePrefix,omitempty"`

	// Sandbox fields (add-cloud-executor 4.1); empty on deployment items.
	SandboxID     string     `json:"sandboxId,omitempty"`
	ExecutorID    string     `json:"executorId,omitempty"`
	TaskID        string     `json:"taskId,omitempty"`
	ContainerName string     `json:"containerName,omitempty"`
	SizeClass     *SizeClass `json:"sizeClass,omitempty"`
	ImageRef      string     `json:"imageRef,omitempty"`
	// TaskToken is the plaintext single-task credential; it exists only in
	// this poll response and the container env -- never on disk, never logged.
	TaskToken      string `json:"taskToken,omitempty"`
	APIBaseURL     string `json:"apiBaseUrl,omitempty"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}

// PresignedObject maps an R2 key to its 15-minute GET URL.
type PresignedObject struct {
	Key string `json:"key"`
	URL string `json:"url"`
}

// PresignResponse is the batch presign answer.
type PresignResponse struct {
	URLs []PresignedObject `json:"urls"`
}

// Watermark is the periodic node capacity report (design D6: the reject-new
// decision lives in the API, which needs the numbers). BootID is the agent's
// process-start identity (design D8): the API diffs it against the stored
// value and, on change, treats the node as rebuilt -- every live deployment
// is re-offered from R2 with no user action. Empty on agents that predate it.
// PortsInUse (fix-seed-audit-blockers S3) lists host ports actually bound by
// running containers: the API unions it into its app-port allocation scan so
// a container outside its own table can no longer win a port race. Nil on
// agents that predate it -- unknown, deliberately NOT "no ports taken".
// Keep in lockstep with apps/api/src/services/hosting-orchestrator.ts
// (AgentWatermark) and the /watermark route validation.
type Watermark struct {
	MemoryAvailableBytes uint64 `json:"memoryAvailableBytes"`
	MemoryTotalBytes     uint64 `json:"memoryTotalBytes"`
	ContainersRunning    int    `json:"containersRunning"`
	BootID               string `json:"bootId,omitempty"`
	PortsInUse           []int  `json:"portsInUse,omitempty"`
	// SandboxesRunning counts running sandbox containers (sbx- name prefix,
	// add-cloud-executor 4.6). Set -- including to zero -- whenever the docker
	// ps behind the watermark succeeded, so the API can distinguish "no
	// sandboxes" (0) from both "agent predates sandbox support" (field absent
	// -> stored NULL) and "docker hiccup, count unknown" (nil -> NULL again;
	// the launch gate must refuse on unknown, never read it as free).
	SandboxesRunning *int `json:"sandboxesRunning,omitempty"`
}

// Report events (terminal states of each work action).
const (
	EventMaterialized      = "materialized"
	EventMaterializeFailed = "materialize_failed"
	EventRouteRegistered   = "route_registered"
	EventRouteRemoved      = "route_removed"
	EventReleased          = "released"
	EventRuntimeFail       = "runtime_fail"

	EventSandboxUp            = "sandbox_up"
	EventSandboxUpFailed      = "sandbox_up_failed"
	EventSandboxDestroy       = "sandbox_destroy"
	EventSandboxDestroyFailed = "sandbox_destroy_failed"
)

// GetWork fetches the current work list.
func (c *Client) GetWork(ctx context.Context) ([]WorkItem, error) {
	var out struct {
		Items []WorkItem `json:"items"`
	}
	if err := c.getJSON(ctx, "/internal/hosting-agent/work", &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// FetchManifest downloads a work item's presigned manifest.json. The URL is
// self-authenticating (sigv4), so no internal secret rides this request.
func (c *Client) FetchManifest(ctx context.Context, url string) ([]File, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch manifest: %d", resp.StatusCode)
	}
	var m struct {
		Files []File `json:"files"`
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if len(m.Files) == 0 {
		return nil, fmt.Errorf("manifest has no files")
	}
	return m.Files, nil
}

// PresignSource exchanges manifest object hashes for presigned GETs. Keys are
// deploys/{deploymentId}/{sha}; the sha is the tail segment, same convention
// the preview presign path uses.
func (c *Client) PresignSource(ctx context.Context, deploymentID string, shas []string) (*PresignResponse, error) {
	payload := map[string]any{
		"agentId":      c.AgentID,
		"deploymentId": deploymentID,
		"shas":         shas,
	}
	var out PresignResponse
	if err := c.postJSON(ctx, "/internal/hosting-agent/presign-source", payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Report posts the outcome of one work item. detail carries the failure
// reason for *_failed events; repoPort for materialized.
func (c *Client) Report(ctx context.Context, deploymentID, event string, repoPort int, detail string) error {
	payload := map[string]any{
		"agentId":      c.AgentID,
		"deploymentId": deploymentID,
		"event":        event,
		"repoPort":     repoPort,
		"detail":       detail,
	}
	return c.postJSON(ctx, "/internal/hosting-agent/report", payload, nil)
}

// ReportSandbox posts the outcome of a sandbox work item (keyed by sandboxId;
// the API's /report route dispatches on that field). containerID is required
// for sandbox_up -- it is what the metering side joins on.
func (c *Client) ReportSandbox(ctx context.Context, sandboxID, event, containerID, detail string) error {
	payload := map[string]any{
		"agentId":     c.AgentID,
		"sandboxId":   sandboxID,
		"event":       event,
		"containerId": containerID,
		"detail":      detail,
	}
	return c.postJSON(ctx, "/internal/hosting-agent/report", payload, nil)
}

// ReportWatermark posts the periodic capacity snapshot.
func (c *Client) ReportWatermark(ctx context.Context, w Watermark) error {
	payload := map[string]any{
		"agentId":   c.AgentID,
		"watermark": w,
	}
	return c.postJSON(ctx, "/internal/hosting-agent/watermark", payload, nil)
}

// UsageSample is one per-container memory reading (add-hosting-quota-guardrails
// design D1/D2). CoolifyAppID is the Coolify application uuid, read from the
// container's coolify.name label -- NOT the numeric coolify.applicationId
// label, which is Coolify's internal id and matches nothing the API stores.
type UsageSample struct {
	CoolifyAppID string `json:"coolifyAppId"`
	MemoryBytes  uint64 `json:"memoryBytes"`
	TS           string `json:"ts"`
}

// ReportUsage posts a batch of per-container samples. The API resolves tenant
// ownership; samples for apps it does not know are dropped server-side.
func (c *Client) ReportUsage(ctx context.Context, samples []UsageSample) error {
	payload := map[string]any{
		"agentId": c.AgentID,
		"samples": samples,
	}
	return c.postJSON(ctx, "/internal/hosting-agent/usage", payload, nil)
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Internal-Secret", c.InternalSecret)
	return c.do(ctx, req, path, out)
}

func (c *Client) postJSON(ctx context.Context, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Secret", c.InternalSecret)
	return c.do(ctx, req, path, out)
}

func (c *Client) do(ctx context.Context, req *http.Request, path string, out any) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", req.Method, path, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		// Fatal signal for the supervisor: retrying cannot fix credentials.
		return &UnauthorizedError{Path: path, Body: string(raw)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{Path: path, Status: resp.StatusCode, Body: string(raw)}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	return nil
}

// UnauthorizedError means the internal secret was rejected.
type UnauthorizedError struct{ Path, Body string }

func (e *UnauthorizedError) Error() string {
	return fmt.Sprintf("401 from %s: %s", e.Path, e.Body)
}

// Unauthorized reports whether err is a credential rejection.
func Unauthorized(err error) bool {
	_, ok := err.(*UnauthorizedError)
	return ok
}

// HTTPError is any other non-2xx outcome.
type HTTPError struct {
	Path   string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%d from %s: %s", e.Status, e.Path, e.Body)
}

// Retry wraps fn with exponential backoff + jitter (node-runner pattern). It
// gives up when ctx is done, when attempts are exhausted, or -- immediately --
// on a credential rejection (retrying cannot help).
func Retry(ctx context.Context, attempts int, fn func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			return nil
		}
		if Unauthorized(err) || ctx.Err() != nil {
			return err
		}
		delay := time.Duration(math.Min(float64(time.Second)*math.Pow(2, float64(i)), float64(30*time.Second)))
		jitter := time.Duration(float64(delay) * 0.1 * (rand.Float64()*2 - 1))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay + jitter):
		}
	}
	return err
}
