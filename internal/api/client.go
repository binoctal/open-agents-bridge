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
	"net/url"
	"strings"
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

	// Runtime preview actions (add-dynamic-preview-compute 5.2): an
	// `activating` row is offered as runtime_up (materialize the snapshot tree
	// into the mission's prebuilt Coolify app and swap it in), a `stopping`
	// row as runtime_stop. Keyed by instanceId, not deploymentId.
	ActionRuntimeUp   = "runtime_up"
	ActionRuntimeStop = "runtime_stop"

	// Session output preview actions (session-output-online-preview): an
	// `up` is the bridge-packed work_dir snapshot to materialize + serve over
	// git-http (the API then drives the Coolify build itself -- unlike
	// runtime_up, the agent does NOT create the Coolify app); register_route
	// wires the ingress after the API-side build lands (row `ready`); stop
	// tears container + route down. Keyed by instanceId.
	ActionSessionPreviewUp            = "session_preview_up"
	ActionSessionPreviewRegisterRoute = "session_preview_register_route"
	ActionSessionPreviewStop          = "session_preview_stop"
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

	// Compose is set on compose-stack materialize items (hosted-compose-stacks
	// 5.1): the rewrite spec -- the agent re-injects resource limits, strips
	// non-ingress host ports, binds the ingress container port to the
	// negotiated 127.0.0.1 host port and activates the requested profiles,
	// all inside the materialized repo. nil on single-container items.
	Compose *ComposeSpec `json:"compose,omitempty"`

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
	// TimeoutAt is the sandbox's absolute wall-clock deadline (RFC3339,
	// cloud_sandboxes.timeout_at). The node hands it to the container as
	// SANDBOX_DEADLINE_EPOCH_MS so the runner derives its CLI kill time from
	// the authoritative window instead of a hardcoded 25min
	// (fix-runner-etimedout-long-turns). Absolute, not remaining: a stale
	// re-offer must not extend the window by re-deriving it later.
	TimeoutAt string `json:"timeoutAt,omitempty"`
	// TaskPrompt is the composed prompt the runner executes (TASK_PROMPT env,
	// add-cloud-executor 5.2). Attempt is the dispatch generation the runner
	// echoes back on its terminal callbacks (G19 attempt pairing). Model is
	// the mission's catalog model (MODEL env -> CLI --model, 7.3): without it
	// the CLI names its built-in default and the outlet's catalog check
	// refuses the call.
	TaskPrompt string `json:"taskPrompt,omitempty"`
	Attempt    int    `json:"attempt,omitempty"`
	Model      string `json:"model,omitempty"`

	// Interactive cloud-session fields (cloud-session-shadow-machine). Empty
	// LifecycleMode means a task sandbox. SessionToken is the plaintext sst_
	// credential: like TaskToken it lives only in this poll response and the
	// container env, never on disk or in logs. ExecutorID (above) is the shadow
	// machine id the container's bridge connects as.
	LifecycleMode      string `json:"lifecycleMode,omitempty"`
	SessionID          string `json:"sessionId,omitempty"`
	SessionToken       string `json:"sessionToken,omitempty"`
	IdleReclaimSeconds int    `json:"idleReclaimSeconds,omitempty"`
	MaxLifetimeSeconds int    `json:"maxLifetimeSeconds,omitempty"`
	// ArtifactUploadURL (interactive-session destroy items, 4.5): presigned
	// R2 PUT the agent uploads the /workspace export to BEFORE removing the
	// container. Empty = export nothing (task sandboxes, R2 off, old API).
	ArtifactUploadURL string `json:"artifactUploadUrl,omitempty"`

	// Runtime preview fields (add-dynamic-preview-compute 3.3); empty on
	// deployment and sandbox items. Host is the {label}.deploy suffix host the
	// agent routes; PreviewID/SnapshotTaskID identify the R2 snapshot the tree
	// materializes from (shas presign against previews/{previewId}/{sha}).
	InstanceID     string `json:"instanceId,omitempty"`
	PreviewID      string `json:"previewId,omitempty"`
	SnapshotTaskID string `json:"snapshotTaskId,omitempty"`
	// UserID is the server-side owner (runtime_preview_instances.user_id)
	// the agent uses to place new Coolify applications in a per-user project
	// (coolify-project-per-user). Empty on old-API wire and non-preview
	// items: the agent then falls back to the static configured project.
	UserID string `json:"userId,omitempty"`

	// CoolifyAppID (session preview register_route/stop items): the
	// application the API drove for this instance. The agent stops this app
	// on session_preview_stop; empty on up items (the app does not exist yet).
	CoolifyAppID string `json:"coolifyAppId,omitempty"`
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
	// DiskAvailableBytes is free space on the docker data root
	// (hosted-compose-stacks 4.4/5.4): feeds the stack-placement watermark
	// gate. Same unknown contract: 0 when the stat failed, and the API's
	// gate trusts unknown rather than reading it as "full".
	DiskAvailableBytes uint64 `json:"diskAvailableBytes,omitempty"`
}

// ComposeSpec is the rewrite payload on compose-stack materialize items
// (hosted-compose-stacks 5.1). Wire-pinned against the API's AgentComposeSpec.
type ComposeSpec struct {
	ComposePath      string         `json:"composePath"`
	IngressService   string         `json:"ingressService"`
	IngressPort      int            `json:"ingressPort"`
	ActivateProfiles []string       `json:"activateProfiles,omitempty"`
	HostPort         int            `json:"hostPort"`
	Services         []ServiceLimit `json:"services"`
}

// ServiceLimit is one ACTIVE service's post-min resource ceiling; the rewrite
// injects it verbatim as deploy.resources.limits.
type ServiceLimit struct {
	Name      string  `json:"name"`
	MemoryMiB int     `json:"memoryMiB"`
	CPUs      float64 `json:"cpus"`
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

	// Runtime preview report events (add-dynamic-preview-compute 3.3): keyed
	// by instanceId on /report; replays against moved rows are no-ops (the
	// API status-CASes each transition).
	EventRuntimeRunning = "runtime_running"
	EventRuntimeStopped = "runtime_stopped"
	EventRuntimeFailed  = "runtime_failed"

	// Session preview report events (session-output-online-preview): same
	// instanceId key; the session_* namespace is disjoint from runtime_* so
	// one report can never land in the wrong tier.
	EventSessionMaterialized    = "session_materialized"
	EventSessionMaterializeFail = "session_materialize_failed"
	EventSessionRouteRegistered = "session_route_registered"
	EventSessionStopped         = "session_stopped"
	EventSessionFailed          = "session_failed"
)

// GetWork fetches the current work list. agent_id keys the deployment and
// runtime-preview halves to THIS node (multi-node scheduling): the API only
// returns rows whose node_id matches. Older APIs ignore the parameter, so
// the upgrade order is free.
func (c *Client) GetWork(ctx context.Context) ([]WorkItem, error) {
	var out struct {
		Items []WorkItem `json:"items"`
	}
	path := "/internal/hosting-agent/work?agent_id=" + url.QueryEscape(c.AgentID)
	if err := c.getJSON(ctx, path, &out); err != nil {
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
// reason for *_failed events; repoPort for materialized. observedAt stamps
// when the outcome was OBSERVED, not when the report got through: retries
// over a lossy link can deliver a verdict minutes late, and the API's
// runtime_fail guard uses the stamp to drop verdicts whose evidence predates
// the row's latest transition (staging 2026-10-30 stale-verdict kill).
func (c *Client) Report(ctx context.Context, deploymentID, event string, repoPort int, detail string) error {
	payload := map[string]any{
		"agentId":      c.AgentID,
		"deploymentId": deploymentID,
		"event":        event,
		"repoPort":     repoPort,
		"detail":       detail,
		"observedAt":   time.Now().UTC().Format(time.RFC3339Nano),
	}
	return c.postJSON(ctx, "/internal/hosting-agent/report", payload, nil)
}

// ReportSandbox posts the outcome of a sandbox work item (keyed by sandboxId;
// the API's /report route dispatches on that field). containerID is required
// for sandbox_up -- it is what the metering side joins on.
func (c *Client) ReportSandbox(ctx context.Context, sandboxID, event, containerID, detail string) error {
	return c.ReportSandboxOpts(ctx, sandboxID, event, containerID, detail, "")
}

// ReportSandboxOpts is ReportSandbox plus the workspace-export outcome
// (cloud-session-shadow-machine 4.5). artifactsStatus is "available" or
// "failed" on an interactive session's sandbox_destroy; empty omits the field
// (task sandboxes, and every non-destroy event).
func (c *Client) ReportSandboxOpts(ctx context.Context, sandboxID, event, containerID, detail, artifactsStatus string) error {
	payload := map[string]any{
		"agentId":     c.AgentID,
		"sandboxId":   sandboxID,
		"event":       event,
		"containerId": containerID,
		"detail":      detail,
	}
	if artifactsStatus != "" {
		payload["artifactsStatus"] = artifactsStatus
	}
	return c.postJSON(ctx, "/internal/hosting-agent/report", payload, nil)
}

// ArtifactHTTP is the upload client for presigned artifact PUTs. Separate
// from Client.HTTP on purpose: the JSON client's 30s timeout cannot carry a
// multi-hundred-MB workspace to R2. A var so tests can swap it.
var ArtifactHTTP = &http.Client{Timeout: 15 * time.Minute}

// PutArtifact uploads a workspace export (tar.gz) to a presigned R2 PUT URL.
// The URL is opaque and short-lived; the body is streamed from disk so a large
// workspace never sits in agent memory.
func (c *Client) PutArtifact(ctx context.Context, url string, body io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := ArtifactHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("artifact PUT %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// RuntimeSnapshotManifest is GET /runtime-snapshot's answer: the full
// path->sha256 map of the instance's current snapshot plus its identity. The
// agent pages /runtime-presign from this (batches of <=500 shas).
type RuntimeSnapshotManifest struct {
	PreviewID      string            `json:"previewId"`
	SnapshotTaskID string            `json:"snapshotTaskId"`
	TaskLabel      string            `json:"taskLabel"`
	Files          map[string]string `json:"files"`
}

// FetchRuntimeSnapshot fetches the manifest of an instance's current snapshot.
func (c *Client) FetchRuntimeSnapshot(ctx context.Context, instanceID string) (*RuntimeSnapshotManifest, error) {
	var out RuntimeSnapshotManifest
	if err := c.getJSON(ctx, "/internal/hosting-agent/runtime-snapshot?instanceId="+url.QueryEscape(instanceID), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RuntimePresign exchanges snapshot object hashes for presigned GETs. Keys are
// the full R2 keys previews/{previewId}/{sha}; the sha is the tail segment.
func (c *Client) RuntimePresign(ctx context.Context, instanceID string, shas []string) (*PresignResponse, error) {
	payload := map[string]any{
		"agentId":    c.AgentID,
		"instanceId": instanceID,
		"shas":       shas,
	}
	var out PresignResponse
	if err := c.postJSON(ctx, "/internal/hosting-agent/runtime-presign", payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReportRuntimePreview posts a runtime preview event (keyed by instanceId;
// the API's /report route dispatches on that field). coolifyAppID rides
// runtime_running (the app the swap landed in); idle=true marks an idle
// auto-stop (drives the API's idle_stopped_at).
func (c *Client) ReportRuntimePreview(ctx context.Context, instanceID, event, coolifyAppID string, idle bool, detail string) error {
	payload := map[string]any{
		"agentId":      c.AgentID,
		"instanceId":   instanceID,
		"event":        event,
		"coolifyAppId": coolifyAppID,
		"idle":         idle,
		"detail":       detail,
	}
	return c.postJSON(ctx, "/internal/hosting-agent/report", payload, nil)
}

// FetchSessionSnapshot fetches the manifest of a session preview instance's
// current snapshot (same shape as FetchRuntimeSnapshot, session-previews/ R2
// prefix behind it).
func (c *Client) FetchSessionSnapshot(ctx context.Context, instanceID string) (*RuntimeSnapshotManifest, error) {
	var out RuntimeSnapshotManifest
	if err := c.getJSON(ctx, "/internal/hosting-agent/session-snapshot?instanceId="+url.QueryEscape(instanceID), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SessionPresign exchanges snapshot object hashes for presigned GETs. Keys
// are the full R2 keys session-previews/{instanceId}/{sha}.
func (c *Client) SessionPresign(ctx context.Context, instanceID string, shas []string) (*PresignResponse, error) {
	payload := map[string]any{
		"agentId":    c.AgentID,
		"instanceId": instanceID,
		"shas":       shas,
	}
	var out PresignResponse
	if err := c.postJSON(ctx, "/internal/hosting-agent/session-presign", payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReportSessionPreview posts a session preview event. repoPort rides
// session_materialized (the git-http port the API bakes into the Coolify app
// it drives); idle=true marks an idle auto-stop.
func (c *Client) ReportSessionPreview(ctx context.Context, instanceID, event string, repoPort int, coolifyAppID string, idle bool, detail string) error {
	payload := map[string]any{
		"agentId":      c.AgentID,
		"instanceId":   instanceID,
		"event":        event,
		"repoPort":     repoPort,
		"coolifyAppId": coolifyAppID,
		"idle":         idle,
		"detail":       detail,
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
	// Cumulative egress counter since container start (docker stats NetIO tx
	// half; launch-plus-plan-guardrails D1). NOT a delta -- the API's hourly
	// rollup derives per-hour deltas and guards counter resets. Zero when the
	// stats line carried no parseable NetIO column.
	TxBytes uint64 `json:"txBytes"`
	// VolumeBytes is the app's named-volume usage in bytes
	// (hosted-compose-stacks 4.5): summed docker system df sizes of the named
	// volumes the app's containers mount. nil when volume accounting failed
	// (single-container rows, docker hiccup) -- the API's quota read skips
	// unknown rather than treating it as zero.
	VolumeBytes *uint64 `json:"volumeBytes,omitempty"`
	TS          string  `json:"ts"`
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
