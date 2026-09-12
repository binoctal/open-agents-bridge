// Runtime preview drive (add-dynamic-preview-compute group 5).
//
// One prebuilt Coolify application per mission (oa-preview-{missionId},
// dockerfile build pack: the agent writes a FROM node:{ver}-slim Dockerfile on
// top of the materialized snapshot tree). Activate/switch = same app
// redeployed over a fresh tree; stop = container stopped, app and repo kept.
// Idle detection reads per-host Caddy access logs (5.1 spike: metrics carry
// no per-host data); the sweep re-asserts route + log wiring every tick
// because a Caddyfile reload wipes admin-config changes.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/binoctal/open-agents/hosting-agent/internal/api"
	"github.com/binoctal/open-agents/hosting-agent/internal/coolify"
	"github.com/binoctal/open-agents/hosting-agent/internal/githttp"
	"github.com/binoctal/open-agents/hosting-agent/internal/store"
)

// rtCoolify is the Coolify seam (*coolify.Client in prod, faked in tests).
type rtCoolify interface {
	ListApplications(ctx context.Context) ([]coolify.Application, error)
	CreateApplication(ctx context.Context, in coolify.ApplicationInput) (string, error)
	EnsureProject(ctx context.Context, name string) (string, error)
	GetApplication(ctx context.Context, uuid string) (*coolify.Application, error)
	UpdateApplication(ctx context.Context, uuid string, in coolify.ApplicationInput) error
	Deploy(ctx context.Context, uuid string, force bool) (string, error)
	GetDeployment(ctx context.Context, uuid string) (*coolify.Deployment, error)
	Stop(ctx context.Context, uuid string) error
}

// rtIngress is the route + access-log seam (*ingress.Caddy in prod).
type rtIngress interface {
	Upsert(ctx context.Context, label, containerAddr string) error
	Remove(ctx context.Context, label string) error
	AttachAccessLog(ctx context.Context, instanceID, host string) error
	DetachAccessLog(ctx context.Context, instanceID, host string) error
}

// rtAPI is the API worker seam (*api.Client in prod).
type rtAPI interface {
	FetchRuntimeSnapshot(ctx context.Context, instanceID string) (*api.RuntimeSnapshotManifest, error)
	RuntimePresign(ctx context.Context, instanceID string, shas []string) (*api.PresignResponse, error)
	ReportRuntimePreview(ctx context.Context, instanceID, event, coolifyAppID string, idle bool, detail string) error
}

// RtEndpoint is the dispatch-time view of one served API environment
// (dual-home-hosting-agent D2): an instance's snapshot fetches, presigns and
// status reports all go through the endpoint that offered its work item, and
// ProjectUUID is that environment's static Coolify placement fallback. BaseURL
// persists into rtState so sweep- and recovery-driven reports still route to
// the owning environment without holding a client reference on disk.
type RtEndpoint struct {
	API         rtAPI
	BaseURL     string
	ProjectUUID string
}

// RuntimePreviewConfig carries the runtime preview knobs (from config).
type RuntimePreviewConfig struct {
	CoolifyProjectUUID     string
	CoolifyServerUUID      string
	CoolifyEnvironmentName string
	// GitBindHost builds the repo URL Coolify fetches from (docker0 gateway).
	GitBindHost string
	// PortMin/PortMax is the dedicated host port range (32100-32199), disjoint
	// from hosted and git http ranges.
	PortMin, PortMax int
	// IdleTTL is the zero-access window before an idle stop (design D3).
	IdleTTL time.Duration
	// LogDir holds the Caddy per-instance access logs.
	LogDir string
	// DomainSuffix splits the {label}.{suffix} host back into its label.
	DomainSuffix string
	// MemoryLimit is the Coolify container memory limit ("128m", preview-p1).
	MemoryLimit string
	// StateDir holds {missionId}.json discovery/idle state files.
	StateDir string
}

// rtState is the persisted per-mission runtime preview footprint. Persisted
// under StateDir so an agent restart rediscovers the Coolify app uuid and the
// idle sweep re-baselines (offset = current log size, lastAccess = now --
// conservative early-stop direction per design D3).
type rtState struct {
	InstanceID   string    `json:"instanceId"`
	MissionID    string    `json:"missionId"`
	CoolifyAppID string    `json:"coolifyAppId"`
	Port         int       `json:"port"`
	Label        string    `json:"label"`
	Host         string    `json:"host"`
	StartedAt    time.Time `json:"startedAt"`
	// APIBaseURL is the owning environment's API origin (dual-home D2):
	// persisted so idle-sweep and post-restart reports route home even
	// though the sweep holds no dispatch-time endpoint reference.
	APIBaseURL string `json:"apiBaseUrl"`

	// Idle tracking (in-memory re-baselined on load).
	LastAccess time.Time `json:"-"`
	LastOffset int64     `json:"-"`
	// ProbeFails counts consecutive dead-port probes (crash-loop signal).
	ProbeFails int `json:"-"`
}

// RuntimePreviews owns the runtime preview lifecycle. Nil field seams disable
// their half (no Caddy locally -> no routes/logs, only the Coolify drive).
type RuntimePreviews struct {
	Coolify rtCoolify
	API     rtAPI
	Ingress rtIngress // may be nil
	Store   *store.Store
	GitHTTP *githttp.Manager
	Cfg     RuntimePreviewConfig

	// EndpointFor resolves a persisted APIBaseURL back to a live endpoint
	// for sweep- and recovery-driven reports (dual-home D2). nil (or a
	// miss) falls back to API + the static project -- single-endpoint
	// setups and the existing tests keep working unchanged.
	EndpointFor func(baseURL string) RtEndpoint

	// dial is the liveness/readiness seam (tests inject).
	dial func(ctx context.Context, port int) error

	mu        sync.Mutex
	byMission map[string]*rtState
	inFlight  map[string]bool
}

// repoKeyFor is the per-mission repo + Coolify app name (design D5).
func repoKeyFor(missionID string) string { return "oa-preview-" + missionID }

// ─── manifest / Dockerfile (pure, unit-tested) ─────────────────────────────

// runtimeManifest is the runtime.json anchor contract (bridge v0: version,
// startCommand, port, healthCheckPath; the optional `node` major field is the
// agent-side extension -- absent means the default 22).
type runtimeManifest struct {
	Version         int      `json:"version"`
	StartCommand    []string `json:"startCommand"`
	Port            int      `json:"port"`
	HealthCheckPath string   `json:"healthCheckPath"`
	Node            int      `json:"node"`
}

// supportedNodeMajors is the pre-pulled base image set (task 5.6). Anything
// else fails honestly -- never guess the nearest version.
var supportedNodeMajors = map[int]bool{20: true, 22: true}

// parseRuntimeManifest validates the anchor. An absent/zero node field means
// the default 22 (the v0 contract has no node field at all).
func parseRuntimeManifest(raw []byte) (runtimeManifest, error) {
	var m runtimeManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("runtime.json: %w", err)
	}
	if m.Version != 1 {
		return m, fmt.Errorf("runtime.json: unsupported version %d", m.Version)
	}
	if len(m.StartCommand) == 0 {
		return m, errors.New("runtime.json: startCommand is empty")
	}
	if m.Port <= 0 || m.Port > 65535 {
		return m, fmt.Errorf("runtime.json: port %d out of range", m.Port)
	}
	if m.Node != 0 && !supportedNodeMajors[m.Node] {
		return m, fmt.Errorf("runtime.json: unsupported node major %d (supported: 20, 22)", m.Node)
	}
	return m, nil
}

// baseImageFor picks the pre-pulled base image; absent node field -> 22.
func baseImageFor(m runtimeManifest) string {
	major := m.Node
	if major == 0 {
		major = 22
	}
	return fmt.Sprintf("node:%d-slim", major)
}

// generateDockerfile renders the controlled Dockerfile that rides the tree.
// The start command comes verbatim from runtime.json; PORT is Coolify's
// exposed-port convention (the container must listen on it).
func generateDockerfile(m runtimeManifest) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "FROM %s\n", baseImageFor(m))
	b.WriteString("WORKDIR /app\n")
	b.WriteString("COPY . .\n")
	b.WriteString("ENV NODE_ENV=production\n")
	fmt.Fprintf(&b, "EXPOSE %d\n", m.Port)
	b.WriteString("CMD [")
	for i, arg := range m.StartCommand {
		if i > 0 {
			b.WriteString(", ")
		}
		q, _ := json.Marshal(arg)
		b.Write(q)
	}
	b.WriteString("]\n")
	return []byte(b.String())
}

// ─── work dispatch ──────────────────────────────────────────────────────────

// dispatch routes runtime preview work items. runtime_up runs async (a
// Coolify build takes minutes; the shared poll loop must not block on it):
// the in-flight claim is held by the goroutine, so re-offers during the build
// are skipped, and the API re-offers after an agent restart (status-driven
// convergence, design D6). ep is the offering endpoint (dual-home D2): every
// exchange the drive makes -- snapshot, presign, reports -- goes through it.
func (rp *RuntimePreviews) dispatch(item api.WorkItem, ep RtEndpoint) {
	if rp == nil || item.InstanceID == "" {
		return
	}
	if !rp.claim(item.InstanceID) {
		return
	}
	switch item.Action {
	case api.ActionRuntimeUp:
		go func() {
			defer rp.release(item.InstanceID)
			// Generous fixed window: a dockerfile build is minutes, not seconds.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Minute)
			defer cancel()
			if err := rp.up(ctx, item, ep); err != nil {
				rp.reportTo(ctx, ep.API, item.InstanceID, api.EventRuntimeFailed, "", false, err.Error())
			}
		}()
	case api.ActionRuntimeStop:
		defer rp.release(item.InstanceID)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := rp.stop(ctx, item, ep); err != nil {
			rp.reportTo(ctx, ep.API, item.InstanceID, api.EventRuntimeFailed, "", false, err.Error())
		}
	default:
		rp.release(item.InstanceID)
	}
}

// claim/release track in-flight instances (guarded separately from byMission
// so the two concerns never deadlock).
func (rp *RuntimePreviews) claim(instanceID string) bool {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if rp.inFlight == nil {
		rp.inFlight = map[string]bool{}
	}
	if rp.inFlight[instanceID] {
		return false
	}
	rp.inFlight[instanceID] = true
	return true
}

func (rp *RuntimePreviews) release(instanceID string) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	delete(rp.inFlight, instanceID)
}

// ─── activate (task 5.2) ────────────────────────────────────────────────────

// up drives one runtime_up to running: snapshot fetch -> materialize repo ->
// find-or-create the Coolify app -> deploy -> readiness -> route + access log
// -> report. Every step is idempotent, so an agent restart's re-offer of the
// same activating row re-converges instead of duplicating state.
func (rp *RuntimePreviews) up(ctx context.Context, item api.WorkItem, ep RtEndpoint) error {
	manifest, err := ep.API.FetchRuntimeSnapshot(ctx, item.InstanceID)
	if err != nil {
		return fmt.Errorf("fetch snapshot: %w", err)
	}
	files := make([]api.File, 0, len(manifest.Files))
	for path, sha := range manifest.Files {
		files = append(files, api.File{Path: path, SHA256: sha})
	}
	rj, ok := manifest.Files["runtime.json"]
	if !ok {
		return errors.New("snapshot has no runtime.json anchor")
	}

	presign := func(ctx context.Context, shas []string) (*api.PresignResponse, error) {
		return ep.API.RuntimePresign(ctx, item.InstanceID, shas)
	}
	if err := rp.Store.FetchMissing(ctx, files, presign); err != nil {
		return fmt.Errorf("fetch objects: %w", err)
	}
	raw, err := rp.Store.ObjectBytes(rj)
	if err != nil {
		return fmt.Errorf("read runtime.json: %w", err)
	}
	rm, err := parseRuntimeManifest(raw)
	if err != nil {
		return err // honest runtime_failed: bad anchor, unsupported node, ...
	}

	repoKey := repoKeyFor(item.MissionID)
	if _, err := rp.Store.BuildRepo(ctx, repoKey, files, map[string][]byte{
		"Dockerfile": generateDockerfile(rm),
	}, false); err != nil {
		return fmt.Errorf("build repo: %w", err)
	}
	gitPort, err := rp.GitHTTP.Serve(repoKey)
	if err != nil {
		return fmt.Errorf("serve repo: %w", err)
	}

	appUUID, err := rp.findOrCreateApp(ctx, item.MissionID, item.UserID, repoKey, gitPort, ep.ProjectUUID)
	if err != nil {
		return fmt.Errorf("coolify app: %w", err)
	}
	hostPort, err := rp.allocatePort(item.MissionID)
	if err != nil {
		return fmt.Errorf("allocate port: %w", err)
	}

	// Re-assert every drive-critical setting on every up (the 8.1 live
	// lesson: stale values survive on existing apps until PATCHed).
	gitURL := fmt.Sprintf("http://%s:%d/%s.git", rp.Cfg.GitBindHost, gitPort, repoKey)
	if err := rp.Coolify.UpdateApplication(ctx, appUUID, coolify.ApplicationInput{
		GitRepository: gitURL,
		GitBranch:     "main",
		BuildPack:     "dockerfile",
		PortsExposes:  hostPort,
		Domains:       "https://" + item.Host,
		LimitsMemory:  rp.Cfg.MemoryLimit,
	}); err != nil {
		return fmt.Errorf("update app: %w", err)
	}

	depUUID, err := rp.Coolify.Deploy(ctx, appUUID, false)
	if err != nil {
		return fmt.Errorf("deploy: %w", err)
	}
	if err := rp.awaitDeployment(ctx, depUUID); err != nil {
		return fmt.Errorf("deployment: %w", err)
	}
	if err := rp.awaitReadiness(ctx, hostPort); err != nil {
		return fmt.Errorf("readiness: %w", err)
	}

	if rp.Ingress != nil {
		label := rp.labelOf(item.Host)
		if label == "" {
			return fmt.Errorf("host %q does not match domain suffix %q", item.Host, rp.Cfg.DomainSuffix)
		}
		if err := rp.Ingress.Upsert(ctx, label, net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort))); err != nil {
			return fmt.Errorf("route: %w", err)
		}
		if err := rp.Ingress.AttachAccessLog(ctx, item.InstanceID, item.Host); err != nil {
			return fmt.Errorf("access log: %w", err)
		}
	}

	st := &rtState{
		InstanceID:   item.InstanceID,
		MissionID:    item.MissionID,
		CoolifyAppID: appUUID,
		Port:         hostPort,
		Label:        rp.labelOf(item.Host),
		Host:         item.Host,
		StartedAt:    time.Now().UTC(),
		APIBaseURL:   ep.BaseURL,
		LastAccess:   time.Now().UTC(),
	}
	rp.setActive(item.MissionID, st)
	if err := rp.persist(st); err != nil {
		// The footprint is live; a lost state file only costs rediscovery
		// (name scan) and an idle-window reset. Log-worthy, not fatal.
		_ = err
	}
	rp.reportTo(ctx, ep.API, item.InstanceID, api.EventRuntimeRunning, appUUID, false, "")
	return nil
}

// labelOf splits {label}.{suffix} back into the label ("" when the host does
// not carry the configured suffix).
func (rp *RuntimePreviews) labelOf(host string) string {
	suffix := "." + rp.Cfg.DomainSuffix
	if !strings.HasSuffix(host, suffix) {
		return ""
	}
	return strings.TrimSuffix(host, suffix)
}

// findOrCreateApp resolves the per-mission Coolify application: live in-memory
// state first, then the persisted state file, then a name scan (state file
// lost but the app lives), then create. Validated with a GET before use --
// a manually deleted app must not 404-loop the drive. The create branch
// places the app in the owner's per-user project when the work item carries
// a userId (coolify-project-per-user); without one (old-API wire) it falls
// back to fallbackProject -- the offering endpoint's project (dual-home D2),
// not the global config value.
func (rp *RuntimePreviews) findOrCreateApp(ctx context.Context, missionID, userID, repoKey string, gitPort int, fallbackProject string) (string, error) {
	name := repoKey // Coolify derives the name from the repo basename
	if st := rp.activeFor(missionID); st != nil && st.CoolifyAppID != "" {
		if app, err := rp.Coolify.GetApplication(ctx, st.CoolifyAppID); err == nil && app != nil {
			return st.CoolifyAppID, nil
		}
	}
	if loaded := rp.loadState(missionID); loaded != nil && loaded.CoolifyAppID != "" {
		if app, err := rp.Coolify.GetApplication(ctx, loaded.CoolifyAppID); err == nil && app != nil {
			return loaded.CoolifyAppID, nil
		}
	}
	apps, err := rp.Coolify.ListApplications(ctx)
	if err != nil {
		return "", err
	}
	// Coolify derives the name from the repo URL basename plus a random
	// suffix ("<repoKey>.git:main-<suffix>"), so the scan matches on the
	// repoKey prefix -- an exact match never hits and a re-activate would
	// create a duplicate app that then 409s on the domain (staging 7.1
	// live finding, 2026-09-11).
	for _, app := range apps {
		if app.Name == name || strings.HasPrefix(app.Name, name+".git") {
			return app.UUID, nil
		}
	}
	gitURL := fmt.Sprintf("http://%s:%d/%s.git", rp.Cfg.GitBindHost, gitPort, repoKey)
	projectUUID := fallbackProject
	if userID != "" {
		var err error
		projectUUID, err = rp.Coolify.EnsureProject(ctx, coolify.PerUserProjectName(userID))
		if err != nil {
			// Fail the instance, never silently place it in the shared
			// project (design D5).
			return "", fmt.Errorf("per-user coolify project: %w", err)
		}
	}
	return rp.Coolify.CreateApplication(ctx, coolify.ApplicationInput{
		ProjectUUID:     projectUUID,
		ServerUUID:      rp.Cfg.CoolifyServerUUID,
		EnvironmentName: rp.Cfg.CoolifyEnvironmentName,
		GitRepository:   gitURL,
		GitBranch:       "main",
		BuildPack:       "dockerfile",
		PortsExposes:    rp.Cfg.PortMin,
		LimitsMemory:    rp.Cfg.MemoryLimit,
	})
}

// allocatePort picks an unused host port from the dedicated range: skip ports
// other live runtime previews hold, then prove the port free by binding it
// (the watermark's PortsInUse union covers cross-allocator strays).
func (rp *RuntimePreviews) allocatePort(missionID string) (int, error) {
	rp.mu.Lock()
	taken := map[int]bool{}
	for m, st := range rp.byMission {
		if m != missionID {
			taken[st.Port] = true
		}
	}
	rp.mu.Unlock()
	for port := rp.Cfg.PortMin; port <= rp.Cfg.PortMax; port++ {
		if taken[port] {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err == nil {
			_ = ln.Close()
			return port, nil
		}
	}
	return 0, fmt.Errorf("runtime preview port range %d-%d exhausted", rp.Cfg.PortMin, rp.Cfg.PortMax)
}

// awaitDeployment polls the queue row to a terminal status. A 404 after a
// sighting is the aged-out success case; a 404 with no prior sighting means
// the row vanished before we ever saw it -- surfaced as an error so the
// readiness dial (the real gate) is never skipped on a guess.
func (rp *RuntimePreviews) awaitDeployment(ctx context.Context, depUUID string) error {
	sawRow := false
	deadline := time.Now().Add(8 * time.Minute)
	for {
		d, err := rp.Coolify.GetDeployment(ctx, depUUID)
		if err != nil {
			return err
		}
		if d == nil {
			if sawRow {
				return nil // finished rows age out of the queue
			}
			return fmt.Errorf("deployment %s unknown before first sighting", depUUID)
		}
		sawRow = true
		switch d.Status {
		case "finished":
			return nil
		case "failed", "cancelled_by_system", "cancelled_by_user":
			return fmt.Errorf("status %s: %s", d.Status, tail(d.Logs, 500))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout in status %s", d.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// tail keeps failure details bounded (Coolify logs can be huge).
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// awaitReadiness dials the published host port until the app accepts a
// connection (up to 60s; the container may still be starting post-deploy).
func (rp *RuntimePreviews) awaitReadiness(ctx context.Context, port int) error {
	dial := rp.dial
	if dial == nil {
		dial = defaultProbeDial
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		err := dial(ctx, port)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("port %d not accepting connections after 60s: %v", port, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// ─── stop (tasks 5.3/5.4) ───────────────────────────────────────────────────

// stop tears one instance down: container stopped (app + repo kept for the
// next activate), route and access log detached, state dropped, reported with
// idle=false (a user-visible stop). The report goes to the endpoint that
// offered the stop item (dual-home D2).
func (rp *RuntimePreviews) stop(ctx context.Context, item api.WorkItem, ep RtEndpoint) error {
	st := rp.activeFor(item.MissionID)
	if st == nil {
		// Not tracked (already stopped, or a restart lost the state): the
		// Coolify app is still discoverable by name -- stop whatever runs
		// under it so the report we send is true.
		apps, err := rp.Coolify.ListApplications(ctx)
		if err != nil {
			return fmt.Errorf("find app for stop: %w", err)
		}
		for _, app := range apps {
			if name := repoKeyFor(item.MissionID); app.Name == name || strings.HasPrefix(app.Name, name+".git") {
				st = &rtState{CoolifyAppID: app.UUID, Host: item.Host, Label: rp.labelOf(item.Host)}
				break
			}
		}
	}
	appID, err := rp.teardown(ctx, st, item.MissionID)
	if err != nil {
		return err
	}
	rp.reportTo(ctx, ep.API, item.InstanceID, api.EventRuntimeStopped, appID, false, "")
	return nil
}

// teardown is the shared stop machinery (container + wiring + state). Exactly
// one runtime_stopped report must follow a successful teardown -- the API's
// status-CAS drops replays against an already-stopped row, so a second report
// with a corrected idle flag would be silently discarded (idle_stopped_at
// would never land). Returns the coolify app id ("" when nothing was found).
func (rp *RuntimePreviews) teardown(ctx context.Context, st *rtState, missionID string) (string, error) {
	if st != nil && st.CoolifyAppID != "" {
		if err := rp.Coolify.Stop(ctx, st.CoolifyAppID); err != nil {
			return "", fmt.Errorf("coolify stop: %w", err)
		}
	}
	if rp.Ingress != nil && st != nil {
		if st.Label != "" {
			if err := rp.Ingress.Remove(ctx, st.Label); err != nil {
				return "", fmt.Errorf("route remove: %w", err)
			}
		}
		if st.InstanceID != "" {
			if err := rp.Ingress.DetachAccessLog(ctx, st.InstanceID, st.Host); err != nil {
				return "", fmt.Errorf("access log detach: %w", err)
			}
		}
	}
	rp.clearActive(missionID)
	rp.unpersist(missionID)
	if st != nil {
		return st.CoolifyAppID, nil
	}
	return "", nil
}

// idleStop is stop driven by the sweep: same teardown, but the single report
// carries idle=true so the API stamps idle_stopped_at (design D3). The sweep
// holds no dispatch-time endpoint, so the report routes via the instance's
// persisted owner (dual-home D2).
func (rp *RuntimePreviews) idleStop(ctx context.Context, st *rtState) {
	appID, err := rp.teardown(ctx, st, st.MissionID)
	if err != nil {
		// The sweep retries next tick; the instance stays tracked meanwhile.
		_ = err
		return
	}
	rp.reportTo(ctx, rp.endpointFor(st.APIBaseURL).API, st.InstanceID, api.EventRuntimeStopped, appID, true, "idle")
}

// ─── idle sweep + liveness (tasks 5.4/5.5) ─────────────────────────────────

// SweepIdle runs every tick over every active instance: re-assert the route +
// access log wiring (a Caddyfile reload wipes admin config), read the access
// log for new bytes, idle-stop past the TTL, and probe the port (a 3-fail
// streak reports runtime_failed -- the crash-loop the API cannot see).
func (rp *RuntimePreviews) SweepIdle(ctx context.Context) {
	if rp == nil {
		return
	}
	rp.mu.Lock()
	states := make([]*rtState, 0, len(rp.byMission))
	for _, st := range rp.byMission {
		states = append(states, st)
	}
	rp.mu.Unlock()
	for _, st := range states {
		if rp.Ingress != nil && st.Label != "" {
			// Self-heal: cheap re-asserts, same discipline as routes.
			if err := rp.Ingress.Upsert(ctx, st.Label, net.JoinHostPort("127.0.0.1", strconv.Itoa(st.Port))); err != nil {
				continue // Caddy unreachable; skip the sweep for this one
			}
			_ = rp.Ingress.AttachAccessLog(ctx, st.InstanceID, st.Host)
		}
		if rp.noteAccess(st) {
			st.ProbeFails = 0
			continue // fresh traffic: alive and not idle
		}
		if idleExpired(time.Now().UTC(), st.LastAccess, rp.Cfg.IdleTTL) {
			rp.idleStop(ctx, st)
			continue
		}
		if rp.probeAlive(ctx, st) {
			continue
		}
	}
}

// noteAccess marks st accessed when its log file grew past the tracked
// offset; true means "alive signal seen". A shrunk file (recreated after a
// reload) re-baselines at the new size without counting it as access --
// growth after that is what counts.
func (rp *RuntimePreviews) noteAccess(st *rtState) bool {
	fi, err := os.Stat(rp.logPath(st.InstanceID))
	if err != nil {
		return false
	}
	size := fi.Size()
	if size > st.LastOffset {
		st.LastOffset = size
		st.LastAccess = time.Now().UTC()
		return true
	}
	if size < st.LastOffset {
		st.LastOffset = size
	}
	return false
}

func (rp *RuntimePreviews) logPath(instanceID string) string {
	return filepath.Join(rp.Cfg.LogDir, instanceID+".log")
}

// idleExpired is the pure judgment (design D3: 30min zero-access -> stop).
func idleExpired(now, lastAccess time.Time, ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	return !lastAccess.IsZero() && now.Sub(lastAccess) >= ttl
}

// probeAlive dials the published port; the 3-fail streak reports
// runtime_failed once (the streak resets, matching the hosted probe's
// discipline). Returns true while the instance looks alive or is still
// within its streak budget.
func (rp *RuntimePreviews) probeAlive(ctx context.Context, st *rtState) bool {
	dial := rp.dial
	if dial == nil {
		dial = defaultProbeDial
	}
	if err := dial(ctx, st.Port); err == nil {
		st.ProbeFails = 0
		return true
	}
	st.ProbeFails++
	if st.ProbeFails < 3 {
		return true
	}
	st.ProbeFails = 0
	rp.mu.Lock()
	delete(rp.byMission, st.MissionID)
	rp.mu.Unlock()
	rp.unpersist(st.MissionID)
	rp.reportTo(ctx, rp.endpointFor(st.APIBaseURL).API, st.InstanceID, api.EventRuntimeFailed, st.CoolifyAppID, false,
		fmt.Sprintf("port %d dead after %d consecutive probes (crash-loop)", st.Port, 3))
	return false
}

// ─── persistence + restart recovery (task 5.5) ──────────────────────────────

func (rp *RuntimePreviews) statePath(missionID string) string {
	return filepath.Join(rp.Cfg.StateDir, missionID+".json")
}

func (rp *RuntimePreviews) persist(st *rtState) error {
	if err := os.MkdirAll(rp.Cfg.StateDir, 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(rp.statePath(st.MissionID), raw, 0o644)
}

func (rp *RuntimePreviews) unpersist(missionID string) {
	_ = os.Remove(rp.statePath(missionID))
}

// loadState reads one mission's persisted state (nil when absent).
func (rp *RuntimePreviews) loadState(missionID string) *rtState {
	raw, err := os.ReadFile(rp.statePath(missionID))
	if err != nil {
		return nil
	}
	var st rtState
	if json.Unmarshal(raw, &st) != nil {
		return nil
	}
	return &st
}

func (rp *RuntimePreviews) setActive(missionID string, st *rtState) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if rp.byMission == nil {
		rp.byMission = map[string]*rtState{}
	}
	rp.byMission[missionID] = st
}

func (rp *RuntimePreviews) activeFor(missionID string) *rtState {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	return rp.byMission[missionID]
}

func (rp *RuntimePreviews) clearActive(missionID string) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	delete(rp.byMission, missionID)
}

// Recover re-adopts persisted instances after an agent restart: the state
// files come back into memory with the idle window re-baselined at "now"
// (offset = current log size -- the conservative early-stop direction; a
// busy preview goes idle-stopped 30min early at worst, never runs unmetered
// forever). The API separately re-offers unconverged rows (status-driven).
func (rp *RuntimePreviews) Recover() {
	if rp == nil {
		return
	}
	entries, err := os.ReadDir(rp.Cfg.StateDir)
	if err != nil {
		return // first boot: no state yet
	}
	now := time.Now().UTC()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		missionID := strings.TrimSuffix(e.Name(), ".json")
		st := rp.loadState(missionID)
		if st == nil || st.InstanceID == "" {
			continue
		}
		st.LastAccess = now
		st.ProbeFails = 0
		if fi, err := os.Stat(rp.logPath(st.InstanceID)); err == nil {
			st.LastOffset = fi.Size()
		}
		rp.setActive(missionID, st)
	}
}

// ─── base image pre-pull (task 5.6) ────────────────────────────────────────

// pullDocker is the exec seam (tests inject; the default runs the real CLI).
var pullDocker dockerRunner = realDocker

// PrePullImages warms the node's image cache so a first activate does not pay
// the registry pull. Best-effort: a failed pull only costs minutes later, the
// deploy still fetches the image itself.
func (rp *RuntimePreviews) PrePullImages(ctx context.Context) {
	if rp == nil {
		return
	}
	for _, image := range []string{"node:22-slim", "node:20-slim"} {
		pullCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		if _, err := pullDocker(pullCtx, "pull", image); err != nil {
			_ = err // best-effort by design
		}
		cancel()
	}
}

// ─── report ────────────────────────────────────────────────────────────────

// reportTo posts one runtime preview event through the given endpoint
// (dual-home D2: the endpoint that offered the item, or the persisted owner
// for sweep-driven reports); failures are absorbed (the API re-offers
// unconverged rows, so a dropped report self-heals).
func (rp *RuntimePreviews) reportTo(ctx context.Context, cl rtAPI, instanceID, event, coolifyAppID string, idle bool, detail string) {
	if cl == nil {
		cl = rp.API
	}
	if err := cl.ReportRuntimePreview(ctx, instanceID, event, coolifyAppID, idle, detail); err != nil {
		// Nothing to log through here without a Logf seam; the in-flight
		// claim is released either way and the API re-offers.
		_ = err
	}
}

// endpointFor resolves a persisted APIBaseURL back to a live endpoint for
// sweep- and recovery-driven reports (dual-home D2). No resolver (or a miss:
// an endpoint removed from the config while its instances still live) falls
// back to the single configured API -- the report may 404 there, but a lost
// report self-heals via re-offer, unlike a silently dropped one.
func (rp *RuntimePreviews) endpointFor(baseURL string) RtEndpoint {
	if rp.EndpointFor != nil {
		if ep := rp.EndpointFor(baseURL); ep.API != nil {
			return ep
		}
	}
	return RtEndpoint{API: rp.API, ProjectUUID: rp.Cfg.CoolifyProjectUUID}
}
