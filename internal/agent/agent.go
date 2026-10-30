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
	"bytes"
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
	"syscall"
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

	// SP drives session preview instances (session-output-online-preview
	// group 4); nil = feature disabled. The API owns the Coolify build for
	// this tier -- SP only materializes, wires routes, sweeps and stops.
	SP *SessionPreviews

	WorkPollInterval  time.Duration
	WatermarkInterval time.Duration

	// WorkIdleMax caps the idle backoff of the work poll: an endpoint that
	// keeps answering "no items" is polled at base*2, base*4, ... up to this
	// value, and snaps back to the base interval on the first item or error.
	// Every poll is one billable Worker request (Workers Free is account-wide),
	// and an idle node previously burned ~8.6k/day per environment. Zero (or
	// <= the base interval) disables the backoff.
	WorkIdleMax time.Duration

	// now is the clock seam for the backoff; nil = time.Now.
	now func() time.Time

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
	// in a row sustained across probeKillWindow is a dead app and is reported
	// once per streak. probeFirstFail anchors that window per streak. Both
	// guarded by inFlightMu.
	probeFails     map[string]int
	probeFirstFail map[string]time.Time

	// epDisabled marks endpoints taken out by a 401 (D3): a rotated secret
	// on one environment must not degrade service to the others, so the
	// endpoint is skipped until process restart instead of exiting. Guarded
	// by inFlightMu.
	epDisabled map[*api.Client]bool

	// epIdle / epNext hold the per-endpoint idle backoff state (see
	// WorkIdleMax). Guarded by inFlightMu.
	epIdle map[*api.Client]int
	epNext map[*api.Client]time.Time
}

func (a *Agent) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// workDue reports whether ep should be polled on this tick.
func (a *Agent) workDue(c *api.Client) bool {
	if a.WorkIdleMax <= a.workInterval() {
		return true
	}
	a.inFlightMu.Lock()
	defer a.inFlightMu.Unlock()
	next, ok := a.epNext[c]
	return !ok || !a.clock().Before(next)
}

// noteWork records the outcome of a poll: an empty answer lengthens the next
// gap, anything else (items, error) resets it so a pending deploy is picked up
// at the base cadence.
func (a *Agent) noteWork(c *api.Client, empty bool) {
	base := a.workInterval()
	if a.WorkIdleMax <= base {
		return
	}
	a.inFlightMu.Lock()
	defer a.inFlightMu.Unlock()
	if a.epIdle == nil {
		a.epIdle = map[*api.Client]int{}
		a.epNext = map[*api.Client]time.Time{}
	}
	if !empty {
		a.epIdle[c] = 0
		delete(a.epNext, c)
		return
	}
	if a.epIdle[c] < 8 {
		a.epIdle[c]++
	}
	gap := base << a.epIdle[c]
	if gap > a.WorkIdleMax {
		gap = a.WorkIdleMax
	}
	// Half a base interval of slack: the ticker fires on a fixed grid, so
	// without it a tick landing a few ms early would be skipped and the real
	// gap would round up a whole extra interval.
	a.epNext[c] = a.clock().Add(gap - base/2)
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

	// Session previews share the runtime tier's tick discipline: adopt
	// persisted instances, then sweep idle/liveness every minute. No image
	// pre-pull -- the API-driven Coolify build pulls its own base image.
	if a.SP != nil {
		a.SP.Recover()
		if rtIdle == nil {
			rt := time.NewTicker(time.Minute)
			defer rt.Stop()
			rtIdle = rt.C
		}
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
			if a.RT != nil {
				a.RT.SweepIdle(ctx)
			}
			a.SP.SweepIdle(ctx) // nil-safe
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
		if !a.workDue(ep.Client) {
			continue
		}
		items, err := ep.Client.GetWork(ctx)
		if err != nil {
			if api.Unauthorized(err) {
				a.disableEndpoint(ep)
				continue
			}
			a.logf("work poll failed (%s): %v", ep.BaseURL, err)
			a.noteWork(ep.Client, false)
			continue
		}
		a.noteWork(ep.Client, len(items) == 0)
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
		// Session preview items are named apart (session_preview_*) and manage
		// their own in-flight claims like the runtime tier; both dispatches
		// are nil-safe for the disabled configuration.
		if strings.HasPrefix(item.Action, "session_preview_") {
			a.SP.dispatch(item, SpEndpoint{API: ep.Client, BaseURL: ep.BaseURL})
			return nil
		}
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
	commit, err := a.materializeTree(ctx, ep, item, manifest)
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

// materializeTree routes between the two materialize shapes
// (hosted-compose-stacks 5.2): single-container trees pass through; compose
// items rewrite the stack file (re-validated limits/ports/profiles) into the
// commit Coolify pulls. Same failure contract as before -- an error here is a
// materialize_failed event and the API re-offers or fails the row.
func (a *Agent) materializeTree(ctx context.Context, ep APIEndpoint, item api.WorkItem, manifest []api.File) (string, error) {
	if item.Compose == nil {
		return a.Store.MaterializeVia(ctx, ep.Client, item.DeploymentID, item.SourcePrefix, manifest)
	}
	c := *item.Compose
	commit, err := a.Store.MaterializeCompose(ctx, ep.Client, item.DeploymentID, item.SourcePrefix, manifest, c)
	if err == nil {
		a.logf("deploy %s: compose %s rewritten (ingress %s:%d -> 127.0.0.1:%d, %d active service(s))",
			item.DeploymentID, c.ComposePath, c.IngressService, c.IngressPort, c.HostPort, len(c.Services))
	}
	return commit, err
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
	// Compose project purge (hosted-compose-stacks 8.17): Coolify's
	// delete_volumes only removes volumes carrying its own labels, and the
	// compose stack's volumes carry none -- docker compose named them
	// <stack-uuid>_<volume> with only com.docker.compose.* labels, so the
	// delete path leaked every data volume on the node (2026-10-30 drill F:
	// containers/networks/route cleared, 3 volumes stayed). The release item
	// carries the stack uuid; tear the project down by its label. Absent uuid
	// (old API or single-container rows) skips the purge entirely.
	if id := item.CoolifyAppID; id != "" {
		a.purgeComposeProject(ctx, item.DeploymentID, id)
	}
	a.logf("deploy %s: released (route + repo + port recycled)", item.DeploymentID)
	return a.report(ctx, ep, item.DeploymentID, api.EventReleased, 0, "")
}

// purgeComposeProject force-removes everything docker compose created for
// one project label: containers first (a mounted container pins its volume),
// then networks, then volumes. Idempotent -- resources Coolify's own async
// cleanup already took are the common case, and misses just log.
func (a *Agent) purgeComposeProject(ctx context.Context, deploymentID, project string) {
	label := "com.docker.compose.project=" + project
	// dockerRun captures stderr: "exit status 1" alone once hid the actual
	// CLI error and cost a blind debugging round (8.17).
	dockerRun := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "docker", args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			if stderr.Len() > 0 {
				err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
			}
		}
		return string(out), err
	}
	for _, k := range []struct {
		name string
		list []string
		rm   []string
	}{
		{"container", []string{"ps", "-aq", "--filter", label}, []string{"rm", "-f"}},
		{"network", []string{"network", "ls", "-q", "--filter", label}, []string{"network", "rm"}},
		{"volume", []string{"volume", "ls", "-q", "--filter", label}, []string{"volume", "rm"}},
	} {
		out, err := dockerRun(k.list...)
		if err != nil {
			a.logf("deploy %s: compose %s list for purge failed: %v", deploymentID, k.name, err)
			continue
		}
		for _, id := range strings.Fields(out) {
			if _, err := dockerRun(append(append([]string{}, k.rm...), id)...); err != nil {
				a.logf("deploy %s: compose %s %s removal failed: %v", deploymentID, k.name, id, err)
			}
		}
	}
	a.logf("deploy %s: compose project %s purge swept", deploymentID, project)
}

// probeKillWindow is how long a port must stay unreachable, across at least
// three consecutive probe failures, before the row is killed (8.12). A row
// flips ready when the Coolify deployment finishes, but the ingress container
// may start listening well after that (vnivni 2026-10-30: row ready 06:47:16,
// openfga container StartedAt 06:48:34 -- three probes ~10s apart killed the
// row 37s before the container even existed). Cold-start seconds are not an
// outage; 90s sustained unreachability is.
const probeKillWindow = 90 * time.Second

// handleProbe is the ready-alive check (S4): dial the app's published host
// port on localhost. Success clears the streak; three consecutive failures
// sustained across probeKillWindow report runtime_fail once (the API's
// transition table takes ready -> failed, and the streak entry is dropped so
// a retry/restart that fails again starts a fresh streak instead of
// double-reporting). Short bursts inside the window are treated as cold
// start and only counted, not believed.
func (a *Agent) handleProbe(ctx context.Context, ep APIEndpoint, item api.WorkItem) error {
	if a.probeDial == nil {
		a.probeDial = defaultProbeDial
	}
	err := a.probeDial(ctx, item.AppPort)
	a.inFlightMu.Lock()
	if a.probeFails == nil {
		a.probeFails = make(map[string]int)
		a.probeFirstFail = make(map[string]time.Time)
	}
	if err == nil {
		delete(a.probeFails, item.DeploymentID)
		delete(a.probeFirstFail, item.DeploymentID)
		a.inFlightMu.Unlock()
		return nil
	}
	now := a.clock()
	a.probeFails[item.DeploymentID]++
	streak := a.probeFails[item.DeploymentID]
	if streak == 1 {
		a.probeFirstFail[item.DeploymentID] = now
	}
	since := now.Sub(a.probeFirstFail[item.DeploymentID])
	if streak < 3 || since < probeKillWindow {
		a.inFlightMu.Unlock()
		a.logf("deploy %s: probe port %d failed (%d consecutive, %s into streak): %v",
			item.DeploymentID, item.AppPort, streak, since.Round(time.Second), err)
		return nil
	}
	delete(a.probeFails, item.DeploymentID)
	delete(a.probeFirstFail, item.DeploymentID)
	a.inFlightMu.Unlock()
	a.logf("deploy %s: port %d dead after %d consecutive probes over %s, reporting runtime_fail",
		item.DeploymentID, item.AppPort, streak, since.Round(time.Second))
	return a.report(ctx, ep, item.DeploymentID, api.EventRuntimeFail, 0, fmt.Sprintf("tcp dial 127.0.0.1:%d failed %d consecutive probes over %s: %v", item.AppPort, streak, since.Round(time.Second), err))
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
		DiskAvailableBytes:   diskAvailableBytes(),
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

// composeServiceLabelRe matches the service-scoped coolify.name label Coolify
// stamps on compose application containers: "<service>-<stackUuid>-<ordinal>"
// (live capture 2026-10-30: "openfga-6parewue1tdmclhr6o1cudjn-070527871608").
// The stack uuid -- the Coolify application uuid the API's row stores as
// coolify_application_id -- embeds as a 24-char lowercase-base62 run; the
// service name varies per container and the ordinal changes every rebuild,
// so the raw label is neither a stable key nor a row match (samples were
// dropped as orphans and the volume quota never fired). Single-container
// apps label containers with the bare app uuid, which the pattern cannot
// match (no -<ordinal> suffix), so they pass through untouched (8.13).
var composeServiceLabelRe = regexp.MustCompile(`^[a-z0-9_-]+-([0-9a-z]{24})-\d+$`)

// normalizeCoolifyAppID folds a compose service label down to its stack uuid.
// The greedy prefix keeps the uuid capture on the last 24-char run before the
// trailing ordinal, which is always the stack uuid Coolify appended.
func normalizeCoolifyAppID(label string) string {
	if m := composeServiceLabelRe.FindStringSubmatch(label); m != nil {
		return m[1]
	}
	return label
}

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
				CoolifyAppID: normalizeCoolifyAppID(fields[1]),
				MemoryBytes:  mem,
				TxBytes:      txByID[fields[0]],
				TS:           ts,
			})
		}
	}
	stampVolumeBytes(ctx, psOut, samples)
	return samples
}

// stampVolumeBytes attaches per-app named-volume totals to the batch's
// samples (hosted-compose-stacks 4.5/5.4): docker inspect maps each labeled
// container to the named volumes it mounts, docker system df -v sizes every
// local volume, and every sample row of an app carries the union total
// across its containers -- compose services mount disjoint volumes, so a
// per-container sum would undercount the stack, and the API's quota read
// takes the latest single row. Best-effort: any step failing leaves
// VolumeBytes nil on every row -- memory/egress billing must not be held
// hostage by volume accounting.
func stampVolumeBytes(ctx context.Context, psOut []byte, samples []api.UsageSample) {
	if len(samples) == 0 {
		return
	}
	idsByApp := map[string][]string{}
	for _, line := range strings.Split(string(psOut), "\n") {
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) == 2 && fields[1] != "" {
			// Same normalization as the sample loop above: compose service
			// labels fold to the stack uuid so the volume union spans every
			// service of the stack, not one service per group (8.13).
			appID := normalizeCoolifyAppID(fields[1])
			idsByApp[appID] = append(idsByApp[appID], fields[0])
		}
	}
	var ids []string
	for _, list := range idsByApp {
		ids = append(ids, list...)
	}

	// One inspect sweep: each container's volume-type mount names (binds are
	// skipped -- only named volumes count against the quota). The format drops
	// the id entirely and the output lines are paired with `ids` positionally:
	// docker inspect echoes one block per argument in argument order, and the
	// full-length id it can print never equals docker ps's 12-char ids that key
	// idsByApp -- matching on the printed id silently zeroed every volume union
	// (8.14). A missing container fails the whole inspect (non-zero exit),
	// which the error path above already treats as "no stamp this tick".
	inspectCtx, cancelInspect := context.WithTimeout(ctx, 30*time.Second)
	defer cancelInspect()
	inspectArgs := append([]string{"inspect", "--format", `{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}} {{end}}{{end}}`}, ids...)
	out, err := exec.CommandContext(inspectCtx, "docker", inspectArgs...).Output()
	if err != nil {
		return
	}
	volumesByID := map[string][]string{}
	inspectLines := strings.Split(string(out), "\n")
	for k, id := range ids {
		if k >= len(inspectLines) {
			break
		}
		volumesByID[id] = strings.Fields(inspectLines[k])
	}

	// One df sweep for per-volume sizes. df -v walks every volume's data, so
	// it can take seconds -- bounded, and worth it once per usage tick.
	dfCtx, cancelDF := context.WithTimeout(ctx, 60*time.Second)
	defer cancelDF()
	dfOut, err := exec.CommandContext(dfCtx, "docker", "system", "df", "-v").Output()
	if err != nil {
		return
	}
	sizes := parseVolumeSizes(string(dfOut))
	// Containers mount named volumes but df parsed to nothing: the CLI's
	// table format moved under us. Unknown beats a confident false zero.
	mountedAny := false
	for _, vols := range volumesByID {
		if len(vols) > 0 {
			mountedAny = true
			break
		}
	}
	if mountedAny && len(sizes) == 0 {
		return
	}

	totals := volumeTotalsByApp(idsByApp, volumesByID, sizes)
	for i := range samples {
		if _, ok := idsByApp[samples[i].CoolifyAppID]; ok {
			total := totals[samples[i].CoolifyAppID]
			samples[i].VolumeBytes = &total
		}
	}
}

// volumeTotalsByApp unions each app's containers' mounted volumes and sums
// their sizes. A mounted volume missing from the sizes table contributes 0
// (best-effort undercount, never a skip of the whole app). Full inspect ids
// are matched against the ps short ids by prefix.
func volumeTotalsByApp(idsByApp map[string][]string, volumesByID map[string][]string, sizes map[string]uint64) map[string]uint64 {
	totals := map[string]uint64{}
	for app, ids := range idsByApp {
		seen := map[string]bool{}
		var total uint64
		for _, short := range ids {
			for full, vols := range volumesByID {
				if !strings.HasPrefix(full, short) {
					continue
				}
				for _, vol := range vols {
					if seen[vol] {
						continue
					}
					seen[vol] = true
					total += sizes[vol]
				}
			}
		}
		totals[app] = total
	}
	return totals
}

// parseVolumeSizes reads the "Local Volumes space usage:" table out of
// `docker system df -v`: rows of "VOLUME NAME LINKS SIZE" where volume names
// and links carry no spaces, so the first field is the name and the last the
// size. The section ends at the first blank line.
func parseVolumeSizes(out string) map[string]uint64 {
	sizes := map[string]uint64{}
	// Section boundaries are the "X space usage:" headers, not blank lines:
	// the real docker output (dang, 2026-10-30) puts a blank line between the
	// "Local Volumes space usage:" header and its table, which a blank-line
	// reset treated as end-of-section -- every volume line was skipped and df
	// parsed to nothing (8.15). Blank lines are simply ignored; the next
	// "space usage:" header flips the section off.
	inVolumes := false
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasSuffix(t, "space usage:") {
			inVolumes = strings.HasPrefix(t, "Local Volumes")
			continue
		}
		if t == "" || !inVolumes {
			continue
		}
		fields := strings.Fields(t)
		if len(fields) < 2 {
			continue
		}
		if fields[0] == "VOLUME" && fields[1] == "NAME" {
			continue // header row
		}
		if size, ok := parseDockerSize(fields[len(fields)-1]); ok {
			sizes[fields[0]] = size
		}
	}
	return sizes
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

// parseDockerSize converts docker's human sizes to bytes. Two suffix families
// are live on the node (docker 26.1.3, verified 2026-09-13): MemUsage renders
// IEC ("77.3MiB", "1.5GiB") while NetIO renders decimal SI ("1.46MB", "454kB",
// "20.7GB") -- the SI family silently parsed as 0 until a staging sample
// cross-check caught it (launch-plus-plan-guardrails 5.3).
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
	case strings.HasSuffix(s, "TB"):
		mult, s = 1000*1000*1000*1000, s[:len(s)-2]
	case strings.HasSuffix(s, "GB"):
		mult, s = 1000*1000*1000, s[:len(s)-2]
	case strings.HasSuffix(s, "MB"):
		mult, s = 1000*1000, s[:len(s)-2]
	case strings.HasSuffix(s, "kB"):
		mult, s = 1000, s[:len(s)-2]
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

// diskAvailableBytes reports free space for the stack-placement watermark
// gate (hosted-compose-stacks 4.4/5.4), preferring the docker data root --
// that is the filesystem stack volumes and images actually land on -- and
// falling back to / for single-disk nodes with a relocated root. Zero on
// total failure: the wire field is omitempty, so the API stores unknown and
// its gate refuses on unknown rather than reading it as "full" or "free".
func diskAvailableBytes() uint64 {
	for _, path := range []string{"/var/lib/docker", "/"} {
		var st syscall.Statfs_t
		if err := syscall.Statfs(path, &st); err == nil {
			return uint64(st.Bavail) * uint64(st.Bsize)
		}
	}
	return 0
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
