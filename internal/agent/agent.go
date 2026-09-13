// Package agent is the hosting-agent's orchestration core: a work poll loop
// (materialize / route wiring / release, driven by the API) and a watermark
// report loop (node capacity for the API's reject-new governance, design D6).
//
// Both loops are best-effort per tick and iterate every configured API
// endpoint (dual-home): an Unauthorized from one endpoint retires that
// endpoint only (D3); the process exits when every endpoint is retired
// (restart churn a human sees, node-runner's fatalHeartbeat pattern).
package agent

import (
	"bufio"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/binoctal/open-agents/hosting-agent/internal/api"
	"github.com/binoctal/open-agents/hosting-agent/internal/githttp"
	"github.com/binoctal/open-agents/hosting-agent/internal/ingress"
	"github.com/binoctal/open-agents/hosting-agent/internal/store"
)

// Logf is the logger seam (main wires fmt.Printf; tests capture).
type Logf func(format string, args ...any)

// Ingress is the route-wiring seam (nil = route wiring disabled).
type Ingress interface {
	Upsert(ctx context.Context, deploymentID, containerAddr string) error
	Remove(ctx context.Context, deploymentID string) error
}

// APIEndpoint is one served API environment (dual-home-hosting-agent D1/D2).
// The single ECS node hosts both staging and production, so the agent serves
// every configured endpoint at once and routes each work item's reports,
// presigns and preview placement back to the endpoint that offered it.
type APIEndpoint struct {
	Client *api.Client
	// BaseURL is the endpoint's routing identity: runtime preview state
	// persists it so the idle sweep reports to the owning environment even
	// after a restart.
	BaseURL string
	// CoolifyProjectUUID is this environment's static runtime-preview
	// placement fallback (items without a userId).
	CoolifyProjectUUID string
}

// Agent wires the loops together.
type Agent struct {
	// Client is the single-endpoint shorthand: used exactly when Endpoints
	// is empty, preserving the pre-dual-home construction for local dev and
	// the existing tests.
	Client *api.Client
	// Endpoints is every served API environment (dual-home). Empty means
	// [Client].
	Endpoints []APIEndpoint

	Store   *store.Store
	GitHTTP *githttp.Manager
	Ingress Ingress // may be nil (local dev without Caddy)

	// RT drives runtime preview instances (add-dynamic-preview-compute
	// group 5); nil = feature disabled (no Coolify configured).
	RT *RuntimePreviews

	WorkPollInterval  time.Duration
	WatermarkInterval time.Duration

	// UsageInterval is the per-container memory sampling cadence
	// (add-hosting-quota-guardrails design D2; RAM·h accuracy needs minutes,
	// not seconds). Zero falls back to usageInterval()'s default.
	UsageInterval time.Duration

	// BootID identifies this process lifetime: the API diffs it against the
	// stored watermark value and, on change, resets every live deployment back
	// to R2 re-materialization (design D8 -- the node is disposable). Set by
	// main (or NewBootID in Run when nil); constant for the process lifetime.
	BootID string

	Logf Logf

	// probeDial is the TCP dial seam for the ready-alive probe (S4); tests
	// inject a fake. nil falls back to a real localhost dial.
	probeDial func(ctx context.Context, port int) error

	// dockerRun is the docker exec seam for sandbox lifecycle commands
	// (add-cloud-executor 4.1); tests inject a recorder. nil runs the real CLI.
	dockerRun dockerRunner

	// inFlight guards against overlapping materializes of one deployment
	// (a slow tree while the next poll re-offers the same work item).
	inFlightMu sync.Mutex
	inFlight   map[string]bool

	// probeFails counts consecutive probe failures per deployment (S4). A
	// single refused dial is noise (container restart, transient dip); three
	// in a row (~30s at the default poll) is a dead app and is reported once
	// per streak. Guarded by inFlightMu.
	probeFails map[string]int

	// epDisabled marks endpoints taken out by a 401 (D3): a rotated secret
	// on one environment must not degrade service to the others, so the
	// endpoint is skipped until process restart instead of exiting. Guarded
	// by inFlightMu.
	epDisabled map[*api.Client]bool
}

// endpoints resolves the served set: Endpoints when set, else the legacy
// single Client.
func (a *Agent) endpoints() []APIEndpoint {
	if len(a.Endpoints) > 0 {
		return a.Endpoints
	}
	return []APIEndpoint{{Client: a.Client}}
}

func (a *Agent) endpointDisabled(c *api.Client) bool {
	a.inFlightMu.Lock()
	defer a.inFlightMu.Unlock()
	return a.epDisabled[c]
}

// disableEndpoint retires an endpoint whose credential was rejected and
// announces it in the log every time (journal visibility for a human, D3).
func (a *Agent) disableEndpoint(ep APIEndpoint) {
	a.inFlightMu.Lock()
	if a.epDisabled == nil {
		a.epDisabled = map[*api.Client]bool{}
	}
	already := a.epDisabled[ep.Client]
	a.epDisabled[ep.Client] = true
	a.inFlightMu.Unlock()
	if !already {
		a.logf("endpoint %s disabled: credential rejected (401); restart the agent with a fresh secret to re-enable", ep.BaseURL)
	}
}

// fatalIfAllDisabled is the process exit condition (D3): with every endpoint
// retired the agent can serve nothing, so it exits for systemd to restart.
func (a *Agent) fatalIfAllDisabled() error {
	for _, ep := range a.endpoints() {
		if !a.endpointDisabled(ep.Client) {
			return nil
		}
	}
	return fmt.Errorf("all API endpoints disabled (credential rejected)")
}

// Run drives both loops until ctx is done.
func (a *Agent) Run(ctx context.Context) error {
	if a.inFlight == nil {
		a.inFlight = make(map[string]bool)
	}
	if a.BootID == "" {
		a.BootID = NewBootID()
	}
	work := time.NewTicker(a.workInterval())
	defer work.Stop()
	wm := time.NewTicker(a.watermarkInterval())
	defer wm.Stop()
	usage := time.NewTicker(a.usageInterval())
	defer usage.Stop()

	// Runtime previews: adopt persisted instances after a restart, warm the
	// base image cache (both best-effort), and sweep idle/liveness on a tick.
	var rtIdle <-chan time.Time
	if a.RT != nil {
		a.RT.Recover()
		go a.RT.PrePullImages(ctx)
		rt := time.NewTicker(time.Minute)
		defer rt.Stop()
		rtIdle = rt.C
	}

	// First tick immediate: don't sit idle for a full interval at boot.
	if err := a.pollWork(ctx); err != nil {
		return err
	}
	if err := a.reportWatermark(ctx); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-work.C:
			if err := a.pollWork(ctx); err != nil {
				return err
			}
		case <-wm.C:
			if err := a.reportWatermark(ctx); err != nil {
				return err
			}
		case <-usage.C:
			if err := a.reportUsage(ctx); err != nil {
				return err
			}
		case <-rtIdle:
			a.RT.SweepIdle(ctx)
		}
	}
}

func (a *Agent) workInterval() time.Duration {
	if a.WorkPollInterval > 0 {
		return a.WorkPollInterval
	}
	return 10 * time.Second
}

func (a *Agent) watermarkInterval() time.Duration {
	if a.WatermarkInterval > 0 {
		return a.WatermarkInterval
	}
	return time.Minute
}

func (a *Agent) usageInterval() time.Duration {
	if a.UsageInterval > 0 {
		return a.UsageInterval
	}
	return 5 * time.Minute
}

// pollWork fetches one batch per endpoint and handles each item sequentially
// -- a single node serves at most a handful of whitelist deployments; per-item
// failures are reported to the API and never abort the batch. Per-endpoint
// failures (including a 401, which retires that endpoint per D3) never abort
// the other endpoints; only every-endpoint-disabled returns an error to Run
// (exit 1, systemd restart).
func (a *Agent) pollWork(ctx context.Context) error {
	for _, ep := range a.endpoints() {
		if a.endpointDisabled(ep.Client) {
			continue
		}
		items, err := ep.Client.GetWork(ctx)
		if err != nil {
			if api.Unauthorized(err) {
				a.disableEndpoint(ep)
				continue
			}
			a.logf("work poll failed (%s): %v", ep.BaseURL, err)
			continue
		}
		for _, item := range items {
			if err := a.handle(ctx, ep, item); err != nil {
				return err
			}
		}
	}
	return a.fatalIfAllDisabled()
}

func (a *Agent) handle(ctx context.Context, ep APIEndpoint, item api.WorkItem) error {
	// Runtime preview items manage their own in-flight claims: runtime_up
	// runs async (a Coolify build takes minutes), so the claim must outlive
	// this call -- the generic claim/release below cannot express that.
	if item.InstanceID != "" {
		a.RT.dispatch(item, RtEndpoint{API: ep.Client, BaseURL: ep.BaseURL, ProjectUUID: ep.CoolifyProjectUUID})
		return nil
	}
	// Sandbox items have no deploymentId; the in-flight key must still be
	// per-object so a re-offer during a slow docker run does not double-start.
	key := item.DeploymentID
	if key == "" {
		key = "sbx:" + item.SandboxID
	}
	if !a.claim(key) {
		a.logf("%s: %s already in flight, skipping", key, item.Action)
		return nil
	}
	defer a.release(key)

	switch item.Action {
	case api.ActionMaterialize:
		return a.handleMaterialize(ctx, ep, item)
	case api.ActionRegisterRoute:
		return a.handleRegisterRoute(ctx, ep, item)
	case api.ActionRemoveRoute:
		return a.handleRemoveRoute(ctx, ep, item)
	case api.ActionRelease:
		return a.handleRelease(ctx, ep, item)
	case api.ActionProbe:
		return a.handleProbe(ctx, ep, item)
	case api.ActionSandboxUp:
		return a.handleSandboxUp(ctx, ep, item)
	case api.ActionSandboxDestroy:
		return a.handleSandboxDestroy(ctx, ep, item)
	default:
		a.logf("%s: unknown action %q", key, item.Action)
		return nil
	}
}

func (a *Agent) handleMaterialize(ctx context.Context, ep APIEndpoint, item api.WorkItem) error {
	// The manifest rides R2, not the work item (D1 statement cap): fetch it
	// first -- a manifest the agent cannot read is a materialize failure,
	// same as an unreadable object later in the loop. The presign exchange
	// below MUST use the offering endpoint: each environment has its own R2
	// bucket and credentials (D2), so MaterializeVia takes the client.
	manifest, err := ep.Client.FetchManifest(ctx, item.ManifestURL)
	if err != nil {
		a.logf("deploy %s: manifest fetch failed: %v", item.DeploymentID, err)
		return a.report(ctx, ep, item.DeploymentID, api.EventMaterializeFailed, 0, err.Error())
	}
	commit, err := a.Store.MaterializeVia(ctx, ep.Client, item.DeploymentID, item.SourcePrefix, manifest)
	if err != nil {
		a.logf("deploy %s: materialize failed: %v", item.DeploymentID, err)
		return a.report(ctx, ep, item.DeploymentID, api.EventMaterializeFailed, 0, err.Error())
	}
	port, err := a.GitHTTP.Serve(item.DeploymentID)
	if err != nil {
		a.logf("deploy %s: git http serve failed: %v", item.DeploymentID, err)
		return a.report(ctx, ep, item.DeploymentID, api.EventMaterializeFailed, 0, err.Error())
	}
	a.logf("deploy %s: materialized %s at http://127.0.0.1:%d/%s.git",
		item.DeploymentID, commit[:12], port, item.DeploymentID)
	return a.report(ctx, ep, item.DeploymentID, api.EventMaterialized, port, commit)
}

func (a *Agent) handleRegisterRoute(ctx context.Context, ep APIEndpoint, item api.WorkItem) error {
	if a.Ingress == nil {
		a.logf("deploy %s: route wiring disabled (no Caddy admin), reporting failure", item.DeploymentID)
		return a.report(ctx, ep, item.DeploymentID, api.EventMaterializeFailed, 0, "route wiring disabled")
	}
	addr := fmt.Sprintf("127.0.0.1:%d", item.AppPort)
	if err := a.Ingress.Upsert(ctx, item.DeploymentID, addr); err != nil {
		a.logf("deploy %s: route upsert failed: %v", item.DeploymentID, err)
		return a.report(ctx, ep, item.DeploymentID, api.EventMaterializeFailed, 0, err.Error())
	}
	a.logf("deploy %s: route %s -> %s registered", item.DeploymentID, item.Host, addr)
	return a.report(ctx, ep, item.DeploymentID, api.EventRouteRegistered, 0, item.Host)
}

func (a *Agent) handleRemoveRoute(ctx context.Context, ep APIEndpoint, item api.WorkItem) error {
	if a.Ingress != nil {
		if err := a.Ingress.Remove(ctx, item.DeploymentID); err != nil {
			a.logf("deploy %s: route remove failed: %v", item.DeploymentID, err)
			return a.report(ctx, ep, item.DeploymentID, api.EventMaterializeFailed, 0, err.Error())
		}
	}
	a.logf("deploy %s: route removed", item.DeploymentID)
	return a.report(ctx, ep, item.DeploymentID, api.EventRouteRemoved, 0, "")
}

func (a *Agent) handleRelease(ctx context.Context, ep APIEndpoint, item api.WorkItem) error {
	// Full local teardown: route first (a released repo must stop answering),
	// then the git port, then the repo itself. Route errors are logged but do
	// not block the rest -- a route that was never registered (delete before
	// ready) is the normal case, not an error.
	if a.Ingress != nil {
		if err := a.Ingress.Remove(ctx, item.DeploymentID); err != nil {
			a.logf("deploy %s: route remove on release failed: %v", item.DeploymentID, err)
		}
	}
	if err := a.GitHTTP.Release(item.DeploymentID); err != nil {
		a.logf("deploy %s: port release failed: %v", item.DeploymentID, err)
	}
	if err := a.Store.Remove(item.DeploymentID); err != nil {
		a.logf("deploy %s: repo removal failed: %v", item.DeploymentID, err)
		return a.report(ctx, ep, item.DeploymentID, api.EventMaterializeFailed, 0, err.Error())
	}
	a.logf("deploy %s: released (route + repo + port recycled)", item.DeploymentID)
	return a.report(ctx, ep, item.DeploymentID, api.EventReleased, 0, "")
}

// handleProbe is the ready-alive check (S4): dial the app's published host
// port on localhost. Success clears the streak; three consecutive failures
// report runtime_fail once (the API's transition table takes ready -> failed,
// and the streak entry is dropped so a retry/restart that fails again starts
// a fresh streak instead of double-reporting).
func (a *Agent) handleProbe(ctx context.Context, ep APIEndpoint, item api.WorkItem) error {
	if a.probeDial == nil {
		a.probeDial = defaultProbeDial
	}
	err := a.probeDial(ctx, item.AppPort)
	a.inFlightMu.Lock()
	if a.probeFails == nil {
		a.probeFails = make(map[string]int)
	}
	if err == nil {
		delete(a.probeFails, item.DeploymentID)
		a.inFlightMu.Unlock()
		return nil
	}
	a.probeFails[item.DeploymentID]++
	streak := a.probeFails[item.DeploymentID]
	if streak < 3 {
		a.inFlightMu.Unlock()
		a.logf("deploy %s: probe port %d failed (%d/3): %v", item.DeploymentID, item.AppPort, streak, err)
		return nil
	}
	delete(a.probeFails, item.DeploymentID)
	a.inFlightMu.Unlock()
	a.logf("deploy %s: port %d dead after %d consecutive probes, reporting runtime_fail",
		item.DeploymentID, item.AppPort, streak)
	return a.report(ctx, ep, item.DeploymentID, api.EventRuntimeFail, 0, fmt.Sprintf("tcp dial 127.0.0.1:%d failed %d consecutive probes: %v", item.AppPort, streak, err))
}

// defaultProbeDial opens a short-lived TCP connection to the app port. Any
// accepted connection proves a listener is alive; the connection is closed
// immediately -- the probe never speaks the app's protocol.
func defaultProbeDial(ctx context.Context, port int) error {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	return conn.Close()
}

// report posts one event to the offering endpoint; a 401 retires that
// endpoint (D3), anything else is logged (the API re-offers the work item on
// the next poll, so a dropped report self-heals).
func (a *Agent) report(ctx context.Context, ep APIEndpoint, deploymentID, event string, repoPort int, detail string) error {
	if err := ep.Client.Report(ctx, deploymentID, event, repoPort, detail); err != nil {
		if api.Unauthorized(err) {
			a.disableEndpoint(ep)
		} else {
			a.logf("deploy %s: report %s failed: %v", deploymentID, event, err)
		}
	}
	return nil
}

// reportWatermark fans the one node-level watermark out to every active
// endpoint (D4): each environment's allocator then sees the full node port
// occupancy, so cross-environment deployments can never race for a port.
func (a *Agent) reportWatermark(ctx context.Context) error {
	w := CollectWatermark(ctx)
	w.BootID = a.BootID
	for _, ep := range a.endpoints() {
		if a.endpointDisabled(ep.Client) {
			continue
		}
		if err := ep.Client.ReportWatermark(ctx, w); err != nil {
			if api.Unauthorized(err) {
				a.disableEndpoint(ep)
				continue
			}
			a.logf("watermark report failed (%s): %v", ep.BaseURL, err)
		}
	}
	return a.fatalIfAllDisabled()
}

// reportUsage samples per-container memory and posts the batch. Skipped when
// no Coolify app containers are running (zero usage, and the watermark loop
// already carries liveness); a non-401 failure is logged, not fatal -- the
// next tick's batch covers the gap and the API's rollup treats a sample-less
// ready hour conservatively by design.
// reportUsage fans the node-wide sample batch out to every active endpoint
// (D4): each API keeps the samples whose coolify app id it owns and drops the
// rest as orphans, so dual-environment billing never crosses.
func (a *Agent) reportUsage(ctx context.Context) error {
	samples := CollectUsageSamples(ctx, time.Now().UTC())
	if len(samples) == 0 {
		return nil
	}
	for _, ep := range a.endpoints() {
		if a.endpointDisabled(ep.Client) {
			continue
		}
		if err := ep.Client.ReportUsage(ctx, samples); err != nil {
			if api.Unauthorized(err) {
				a.disableEndpoint(ep)
				continue
			}
			a.logf("usage report failed (%s): %v", ep.BaseURL, err)
		}
	}
	return a.fatalIfAllDisabled()
}

// NewBootID mints a fresh process-lifetime identity (design D8). It only ever
// needs to be unique per restart, not secret: the API compares it for
// inequality, never trusts it for authentication.
func NewBootID() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		// crypto/rand failing is unrecoverable on a healthy node; a
		// time-derived id still changes across restarts, which is the
		// property that matters.
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func (a *Agent) claim(deploymentID string) bool {
	a.inFlightMu.Lock()
	defer a.inFlightMu.Unlock()
	if a.inFlight == nil {
		a.inFlight = make(map[string]bool)
	}
	if a.inFlight[deploymentID] {
		return false
	}
	a.inFlight[deploymentID] = true
	return true
}

func (a *Agent) release(deploymentID string) {
	a.inFlightMu.Lock()
	defer a.inFlightMu.Unlock()
	delete(a.inFlight, deploymentID)
}

func (a *Agent) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
	}
}

// psDocker is the exec seam for the watermark's docker ps (add-cloud-executor
// 4.6): CollectWatermark goes through it, tests swap it to fake container
// listings without docker on the box.
var psDocker dockerRunner = realDocker

// CollectWatermark reads node capacity: MemAvailable/MemTotal from
// /proc/meminfo and the running-container count from docker (best-effort --
// a docker hiccup reports 0 containers, never blocks the loop). PortsInUse
// (S3) parses the published host ports out of the `docker ps` output -- lines
// like "name 0.0.0.0:30001->3000/tcp" -- so the API's allocation scan sees
// occupants its own table knows nothing about. SandboxesRunning (4.6) counts
// containers whose name starts with sbx- (containerNameFor), sent -- even as
// zero -- only when the ps succeeded; on failure the field stays nil so the
// API stores unknown instead of a false "no sandboxes". Best-effort like the
// container count: a docker hiccup yields no ports and no sandbox count,
// never blocks the loop.
func CollectWatermark(ctx context.Context) api.Watermark {
	w := api.Watermark{
		MemoryAvailableBytes: meminfo("MemAvailable"),
		MemoryTotalBytes:     meminfo("MemTotal"),
	}
	if out, err := psDocker(ctx, "ps", "--filter", "status=running", "--format", "{{.Names}} {{.ID}} {{.Ports}}"); err == nil {
		w.ContainersRunning = 0
		seen := map[int]bool{}
		sbx := 0
		for _, line := range strings.Split(out, "\n") {
			// The .ID half keeps one non-empty line per container, including
			// those that publish no ports (whose .Ports half is blank).
			if strings.TrimSpace(line) == "" {
				continue
			}
			w.ContainersRunning++
			// The .Names half (first field, comma-joined aliases) is the
			// sandbox discriminator; break so multi-alias names count once.
			for _, name := range strings.Split(strings.Fields(line)[0], ",") {
				if strings.HasPrefix(name, "sbx-") {
					sbx++
					break
				}
			}
			for _, m := range hostPortRe.FindAllStringSubmatch(line, -1) {
				if p, err := strconv.Atoi(m[1]); err == nil && p > 0 && p < 65536 && !seen[p] {
					seen[p] = true
					w.PortsInUse = append(w.PortsInUse, p)
				}
			}
		}
		w.SandboxesRunning = &sbx
	}
	return w
}

// hostPortRe matches the host side of a docker ps port publication, e.g.
// "0.0.0.0:30001->3000/tcp" or "[::]:30001->3000/tcp". Duplicated ports
// across containers are collapsed by the caller's seen map.
var hostPortRe = regexp.MustCompile(`(?:\d+\.\d+\.\d+\.\d+|\[::\]):(\d+)->`)

// CollectUsageSamples reads per-container memory for Coolify-managed
// containers (add-hosting-quota-guardrails design D1): docker ps filtered on
// the coolify.name label (the application uuid the API stores), joined
// against one docker stats --no-stream sweep. Best-effort like
// CollectWatermark: a docker hiccup yields no samples, never blocks the loop.
// Coolify's own housekeeping containers may carry the label too -- the API
// drops those as orphans by design.
func CollectUsageSamples(ctx context.Context, now time.Time) []api.UsageSample {
	const appLabel = "coolify.name"
	psOut, err := exec.CommandContext(ctx, "docker", "ps", "--filter", "label="+appLabel,
		"--format", "{{.ID}}\t{{.Label \""+appLabel+"\"}}").Output()
	if err != nil {
		return nil
	}
	statsOut, err := exec.CommandContext(ctx, "docker", "stats", "--no-stream",
		"--format", "{{.ID}}\t{{.MemUsage}}\t{{.NetIO}}").Output()
	if err != nil {
		return nil
	}
	memByID := make(map[string]uint64)
	txByID := make(map[string]uint64)
	for _, line := range strings.Split(string(statsOut), "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 2 {
			continue
		}
		// MemUsage renders as "used / limit"; the used half is the reading.
		used := strings.SplitN(fields[1], "/", 2)[0]
		if b, ok := parseDockerSize(used); ok {
			memByID[fields[0]] = b
		}
		// NetIO renders as "rx / tx" (launch-plus-plan-guardrails D1): the tx
		// half after "/" is the cumulative egress counter since container
		// start. Missing/unparseable column leaves 0 -- the rollup treats a
		// zero-to-N jump as a fresh counter, which over-counts nothing.
		if len(fields) == 3 {
			if tx, ok := splitNetIOTx(fields[2]); ok {
				txByID[fields[0]] = tx
			}
		}
	}
	ts := now.Format(time.RFC3339Nano)
	var samples []api.UsageSample
	for _, line := range strings.Split(string(psOut), "\n") {
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 || fields[1] == "" {
			continue
		}
		if mem, ok := memByID[fields[0]]; ok && mem > 0 {
			samples = append(samples, api.UsageSample{
				CoolifyAppID: fields[1],
				MemoryBytes:  mem,
				TxBytes:      txByID[fields[0]],
				TS:           ts,
			})
		}
	}
	return samples
}

// splitNetIOTx extracts the tx half of docker stats' NetIO column ("rx / tx",
// e.g. "1.5MiB / 3.2GiB") as a byte count.
func splitNetIOTx(s string) (uint64, bool) {
	halves := strings.SplitN(s, "/", 2)
	if len(halves) != 2 {
		return 0, false
	}
	return parseDockerSize(halves[1])
}

// parseDockerSize converts docker's human sizes ("77.3MiB", "1.5GiB", "512B")
// to bytes.
func parseDockerSize(s string) (uint64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	mult := uint64(1)
	switch {
	case strings.HasSuffix(s, "GiB"):
		mult, s = 1024*1024*1024, s[:len(s)-3]
	case strings.HasSuffix(s, "MiB"):
		mult, s = 1024*1024, s[:len(s)-3]
	case strings.HasSuffix(s, "KiB"):
		mult, s = 1024, s[:len(s)-3]
	case strings.HasSuffix(s, "B"):
		s = s[:len(s)-1]
	default:
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return uint64(v * float64(mult)), true
}

func meminfo(key string) uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == key+":" {
			if kb, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				return kb * 1024
			}
		}
	}
	return 0
}

// NewCaddyAdaptor exists so main can build the Ingress without importing the
// ingress package's concrete type twice; nil when addr is empty.
func NewCaddyIngress(addr, domainSuffix string) Ingress {
	if addr == "" {
		return nil
	}
	return ingress.NewCaddy(addr, domainSuffix)
}
