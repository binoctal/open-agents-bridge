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
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/binoctal/open-agents-bridge/node/api"
	composepkg "github.com/binoctal/open-agents-bridge/node/compose"
	"github.com/binoctal/open-agents-bridge/node/githttp"
	"github.com/binoctal/open-agents-bridge/node/ingress"
	"github.com/binoctal/open-agents-bridge/node/store"
)

// Logf is the logger seam (main wires fmt.Printf; tests capture).
type Logf func(format string, args ...any)

// Ingress is the route-wiring seam (nil = route wiring disabled).
type Ingress interface {
	Upsert(ctx context.Context, deploymentID, containerAddr string) error
	Remove(ctx context.Context, deploymentID string) error
	// Ping reports whether the edge proxy is answering (fix-caddy-route-
	// reassert D4); one probe, not a debounced verdict.
	Ping(ctx context.Context) error
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

	// caddyFails counts consecutive failed liveness probes (fix-caddy-route-
	// reassert D4). Written only from the watermark loop goroutine.
	caddyFails int

	// RT drives runtime preview instances (add-dynamic-preview-compute
	// group 5); nil = feature disabled (no Coolify configured).
	RT *RuntimePreviews

	// SP drives session preview instances (session-output-online-preview
	// group 4); nil = feature disabled. The API owns the Coolify build for
	// this tier -- SP only materializes, wires routes, sweeps and stops.
	SP *SessionPreviews

	// PollNow short-circuits the work poll's idle backoff (hosted-deploy-
	// push-wakeup): the wake goroutine nudges it whenever the platform pokes
	// the node room, and Run polls immediately instead of waiting out the
	// backoff window. Nil (platform form, older wiring) keeps the pure-poll
	// behavior -- a nil channel never fires in select. Buffered 1 so nudges
	// coalesce while a poll is in flight.
	PollNow chan struct{}

	WorkPollInterval  time.Duration
	WatermarkInterval time.Duration

	// UserNode marks the BYO user-node form (byo-hosting-node D1): node-
	// credential auth, deployment-only command surface (enum whitelist +
	// ownership re-check), agent-driven compose up / docker run (no Coolify,
	// no git http), no usage sampling. Set by main from the credentials file.
	UserNode bool

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
		case <-a.PollNow:
			// Wake nudge (hosted-deploy-push-wakeup): poll now, bypassing the
			// idle backoff gate. pollWork's own noteWork keeps the backoff
			// state honest: work found resets it, an empty answer costs one
			// billable request and deepens the idle gap by one step.
			if err := a.pollWorkForce(ctx); err != nil {
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
	return a.pollWorkOpt(ctx, false)
}

// pollWorkForce is the wake-nudge entry (hosted-deploy-push-wakeup): same
// batch, but the idle-backoff gate is bypassed -- a nudge arriving mid-backoff
// must poll immediately, not be swallowed by the very gap it exists to cut.
func (a *Agent) pollWorkForce(ctx context.Context) error {
	return a.pollWorkOpt(ctx, true)
}

func (a *Agent) pollWorkOpt(ctx context.Context, force bool) error {
	for _, ep := range a.endpoints() {
		if a.endpointDisabled(ep.Client) {
			continue
		}
		if !force && !a.workDue(ep.Client) {
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

// userNodeActions is the user-node command surface (byo-hosting-node D1):
// the deployment lifecycle enum ONLY. Everything else -- the sandbox fleet,
// runtime previews, session previews, any future action -- is a platform
// surface a user machine must refuse even if a compromised or buggy platform
// dispatches it. Refusal is loud: log line + (for deployment-keyed items) a
// materialize_failed report so the owner's audit trail shows the refusal.
var userNodeActions = map[string]bool{
	api.ActionMaterialize:   true,
	api.ActionRegisterRoute: true,
	api.ActionAssertRoute:   true,
	api.ActionRemoveRoute:   true,
	api.ActionRelease:       true,
	api.ActionProbe:         true,
}

// userNodeGate enforces the agent-side half of the D1 double check on every
// item before dispatch: action inside the enum, no sandbox/instance payload,
// and (route actions) the item's host really is this deployment's label --
// a hostile dispatch must not be able to wire a FOREIGN label onto this
// node's ingress.
func (a *Agent) userNodeGate(ctx context.Context, ep APIEndpoint, item api.WorkItem) bool {
	if item.SandboxID != "" || item.InstanceID != "" {
		a.refuseItem(ctx, ep, item, "sandbox/preview work items are platform-only; this is a user node")
		return false
	}
	if !userNodeActions[item.Action] {
		a.refuseItem(ctx, ep, item, fmt.Sprintf("action %q is outside the user-node command enum", item.Action))
		return false
	}
	if item.Host != "" {
		label := strings.ToLower(item.DeploymentID)
		if !strings.HasPrefix(strings.ToLower(item.Host), label+".") {
			a.refuseItem(ctx, ep, item, fmt.Sprintf("route host %q does not match deployment %s (foreign-label dispatch refused)", item.Host, item.DeploymentID))
			return false
		}
	}
	return true
}

// refuseItem logs the refusal (journal-visible audit) and reports
// materialize_failed for deployment-keyed items the API will accept (rows on
// THIS node owned by THIS owner -- a foreign row's report 404s server-side,
// which is the correct silent drop; the log line above remains the record).
func (a *Agent) refuseItem(ctx context.Context, ep APIEndpoint, item api.WorkItem, reason string) {
	key := item.DeploymentID
	if key == "" {
		key = item.SandboxID + item.InstanceID
	}
	a.logf("REFUSED item %s action=%q host=%q: %s", key, item.Action, item.Host, reason)
	if item.DeploymentID != "" {
		_ = a.report(ctx, ep, item.DeploymentID, api.EventMaterializeFailed, 0, "refused on user node: "+reason)
	}
}

func (a *Agent) handle(ctx context.Context, ep APIEndpoint, item api.WorkItem) error {
	// User-node command gate (byo-hosting-node D1, 2.2): before ANY dispatch.
	if a.UserNode && !a.userNodeGate(ctx, ep, item) {
		return nil
	}
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

	if item.Action == api.ActionMaterialize {
		// Image-tier tars stream for many minutes (a 2.4 GiB tar at 10-31
		// rates). handleMaterialize on the loop would stall watermark/work
		// ticks until the row reads agent-lost (10-31: heartbeat froze at
		// the claim, API failed the row agent_unreachable mid-download).
		// The goroutine owns this claim and releases from inside; the defer
		// below only covers the synchronous handlers.
		go func() {
			defer a.release(key)
			if err := a.handleMaterialize(ctx, ep, item); err != nil {
				a.logf("%s: materialize handler error: %v", key, err)
			}
		}()
		return nil
	}
	// Synchronous handlers release on return. A single defer for every
	// action would free the claim while the materialize goroutine still
	// streams (10-31: register_route leaked its claim, so the next
	// materialize offer skipped forever as "already in flight" with no
	// goroutine running).
	defer a.release(key)

	switch item.Action {
	case api.ActionRegisterRoute:
		return a.handleRegisterRoute(ctx, ep, item)
	case api.ActionAssertRoute:
		return a.handleAssertRoute(ctx, ep, item)
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
	// User node (byo-hosting-node 2.3): the agent itself brings the workload
	// up -- compose up from a plain worktree (or docker run for a future
	// single-container shape) -- and reports the host port. No Coolify, no
	// git http; readiness is this report (the API's BYO branch folds
	// materialize_done+build_done and flips the row ready).
	if a.UserNode {
		port, err := a.nodeMaterialize(ctx, ep, item, manifest)
		if err != nil {
			a.logf("deploy %s: node materialize failed: %v", item.DeploymentID, err)
			return a.report(ctx, ep, item.DeploymentID, api.EventMaterializeFailed, 0, err.Error())
		}
		a.logf("deploy %s: up on 127.0.0.1:%d (user node)", item.DeploymentID, port)
		return a.report(ctx, ep, item.DeploymentID, api.EventMaterialized, port, "")
	}
	commit, err := a.materializeTree(ctx, ep, item, manifest)
	if err != nil {
		a.logf("deploy %s: materialize failed: %v", item.DeploymentID, err)
		return a.report(ctx, ep, item.DeploymentID, api.EventMaterializeFailed, 0, err.Error())
	}
	// Image tier (docker-image-deploy-tier D6): every manifest tar is
	// docker-loaded into the node's local store AFTER the tree materialized
	// and BEFORE the materialized event lets Coolify compose the stack up --
	// compose resolves the oa-deploy-* references from the local daemon, no
	// registry involved. A load failure fails materialize; the re-offer
	// re-loads, and `docker load` over an existing image is idempotent.
	if c := item.Compose; c != nil && c.Tier == "image" {
		if err := a.loadImageTars(ctx, item.DeploymentID, manifest); err != nil {
			a.logf("deploy %s: image load failed: %v", item.DeploymentID, err)
			return a.report(ctx, ep, item.DeploymentID, api.EventMaterializeFailed, 0, err.Error())
		}
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

// deployImageTag mirrors the bridge's ImageTag three-end contract (bridge
// internal/deploysource/imagebuild.go): oa-deploy-<deploymentID>-<service>
// with the whole string folded lowercase — docker rejects uppercase
// repository names, and deployment ids are mixed-case base62. The hosting
// side derives the service from the manifest tar filename minus .tar, the
// same spelling the platform's complete-time validator uses.
func deployImageTag(deploymentID, service string) string {
	return strings.ToLower("oa-deploy-" + deploymentID + "-" + service)
}

// nodeGOArch is the arch validation baseline (fix-image-tier-arch-mismatch
// D4); a package var only so tests can pin both sides of the comparison.
var nodeGOArch = runtime.GOARCH

// loadImageTars (docker-image-deploy-tier D6) docker-loads each manifest tar
// from the object store. Loads are independent of each other -- no ordering
// -- and the failure carries the failing tar plus docker's output tail, the
// only place the load error is visible before the compose up fails on a
// missing local image.
//
// fix-image-tier-arch-mismatch D4: right after each load, the image's
// architecture is inspected and compared against the node's. A mismatch
// fails materialize HERE with a readable reason -- docker load happily
// accepts a foreign-arch image and the failure only surfaces at compose up
// as a bare "exec format error" with no image name. Unknown/empty arch
// (older daemon output, inspect miss) passes: verification must not become
// a new failure mode for working packs.
func (a *Agent) loadImageTars(ctx context.Context, deploymentID string, manifest []api.File) error {
	for _, f := range manifest {
		if !strings.HasSuffix(f.Path, ".tar") {
			continue
		}
		path, err := a.Store.ObjectPath(f.SHA256)
		if err != nil {
			return fmt.Errorf("tar %s: %w", f.Path, err)
		}
		out, err := a.docker()(ctx, "load", "-i", path)
		if err != nil {
			tail := strings.TrimSpace(out)
			if len(tail) > 200 {
				tail = tail[len(tail)-200:]
			}
			return fmt.Errorf("docker load %s: %v: %s", f.Path, err, tail)
		}
		if err := a.checkLoadedArch(ctx, deploymentID, f.Path); err != nil {
			return err
		}
		a.logf("deploy %s: loaded image tar %s", deploymentID, f.Path)
	}
	return nil
}

// checkLoadedArch inspects the just-loaded image's Architecture and fails on
// a foreign-arch load (the exec-format trap). An unreadable answer passes --
// see loadImageTars.
func (a *Agent) checkLoadedArch(ctx context.Context, deploymentID, tarPath string) error {
	service := strings.TrimSuffix(tarPath, ".tar")
	tag := deployImageTag(deploymentID, service)
	out, err := a.docker()(ctx, "image", "inspect", tag, "--format", "{{.Architecture}}")
	if err != nil {
		a.logf("deploy %s: arch check skipped for %s (inspect failed: %v)", deploymentID, tag, err)
		return nil
	}
	imageArch := strings.ToLower(strings.TrimSpace(out))
	if imageArch == "" || imageArch == "unknown" {
		return nil
	}
	if imageArch != nodeGOArch {
		return fmt.Errorf(
			"image %s is arch %s but this node is %s -- compose up would fail with exec format error; redeploy with an up-to-date bridge (its image-tier build pins --platform linux/%s to the node arch) or build the image for linux/%s",
			tag, imageArch, nodeGOArch, nodeGOArch, nodeGOArch)
	}
	return nil
}

// rmiDeployImages (docker-image-deploy-tier 3.2) removes the deployment's
// local oa-deploy-* images on release. Coolify's app delete never sees them
// (compose up resolved the references locally), so this is the only hook.
// Best-effort: a failed rmi logs and is forgotten -- the dangling image
// falls to the node's routine prune instead of blocking teardown.
func (a *Agent) rmiDeployImages(ctx context.Context, deploymentID string) {
	out, err := a.docker()(ctx, "images", "--filter", "reference=oa-deploy-"+strings.ToLower(deploymentID)+"-*", "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		a.logf("deploy %s: image listing for rmi failed (left for routine prune): %v", deploymentID, err)
		return
	}
	for _, ref := range strings.Fields(out) {
		if _, err := a.docker()(ctx, "rmi", ref); err != nil {
			a.logf("deploy %s: rmi %s failed (left for routine prune): %v", deploymentID, ref, err)
		} else {
			a.logf("deploy %s: removed image %s", deploymentID, ref)
		}
	}
}

// nodeComposeProject is the docker compose project name a user node runs a
// deployment under (and the label release purges by). Lowercase-folded with
// the deployment id like deployImageTag -- compose project names reject
// uppercase the same way repository names do.
func nodeComposeProject(deploymentID string) string {
	return strings.ToLower("oa-" + deploymentID)
}

// nodeMaterialize brings one deployment up on a user node (byo-hosting-node
// 2.3): compose rows rewrite the stack file (same rule table as the platform
// path -- limits, port stripping, loopback ingress binding, profiles),
// materialize a PLAIN worktree (R2 objects or declare-only local tars), then
// `docker compose up -d --wait`. Returns the ingress host port the report
// carries (the API folds it into app_port).
func (a *Agent) nodeMaterialize(ctx context.Context, ep APIEndpoint, item api.WorkItem, manifest []api.File) (int, error) {
	if c := item.Compose; c != nil {
		spec := *c
		// Same pipeline as Store.MaterializeCompose, minus the git repo: the
		// rewritten compose rides the worktree directly.
		presign := func(ctx context.Context, shas []string) (*api.PresignResponse, error) {
			return ep.Client.PresignSource(ctx, item.DeploymentID, shas)
		}
		if err := a.Store.FetchMissing(ctx, manifest, presign); err != nil {
			return 0, err
		}
		var composeSHA string
		for _, f := range manifest {
			if f.Path == spec.ComposePath {
				composeSHA = f.SHA256
				break
			}
		}
		if composeSHA == "" {
			return 0, fmt.Errorf("compose path %s not in manifest", spec.ComposePath)
		}
		raw, err := a.Store.ObjectBytes(composeSHA)
		if err != nil {
			return 0, fmt.Errorf("read compose source: %w", err)
		}
		rewritten, err := composepkg.Rewrite(raw, spec)
		if err != nil {
			return 0, err
		}
		tree := manifest
		if spec.Tier == "image" {
			// Tars never enter the tree (docker load reads them from the
			// object store); same filter discipline as the platform path.
			tree = make([]api.File, 0, len(manifest))
			for _, f := range manifest {
				if !strings.HasSuffix(f.Path, ".tar") {
					tree = append(tree, f)
				}
			}
		}
		work, err := a.Store.Worktree(ctx, ep.Client, item.DeploymentID, item.SourcePrefix, tree, map[string][]byte{spec.ComposePath: rewritten})
		if err != nil {
			return 0, err
		}
		if spec.Tier == "image" {
			if err := a.loadImageTars(ctx, item.DeploymentID, manifest); err != nil {
				return 0, err
			}
		}
		if err := a.composeUp(ctx, item.DeploymentID, filepath.Join(work, filepath.FromSlash(spec.ComposePath))); err != nil {
			return 0, err
		}
		return spec.HostPort, nil
	}
	// Non-compose rows: today the platform rejects non-compose tiers onto
	// user nodes at admission (source tier is Coolify/nixpacks-bound), so
	// this branch is the future-proofing path for a single-container image
	// row -- load the tar, run it pinned to the pre-allocated host port.
	return a.nodeRunSingle(ctx, item, manifest)
}

// composeUp runs `docker compose up -d --wait` for one deployment's worktree.
// --wait turns "containers exist" into "ingress container answers", which is
// what the platform's probe then keeps honest. The project name is stable per
// deployment so a re-materialize converges the same project instead of
// stacking orphan copies.
func (a *Agent) composeUp(ctx context.Context, deploymentID, composeFile string) error {
	args := []string{"compose", "-p", nodeComposeProject(deploymentID), "-f", composeFile, "up", "-d", "--wait", "--remove-orphans"}
	out, err := a.docker()(ctx, args...)
	if err != nil {
		tail := strings.TrimSpace(out)
		if len(tail) > 300 {
			tail = tail[len(tail)-300:]
		}
		if tail != "" {
			return fmt.Errorf("docker compose up: %v: %s", err, tail)
		}
		return fmt.Errorf("docker compose up: %w", err)
	}
	a.logf("deploy %s: compose up complete (%s)", deploymentID, composeFile)
	return nil
}

// nodeRunSingle loads the single image tar and runs it bound to the
// pre-allocated loopback port (container port = host port, mirroring the
// platform's Coolify ports_exposes convention).
func (a *Agent) nodeRunSingle(ctx context.Context, item api.WorkItem, manifest []api.File) (int, error) {
	if item.AppPort <= 0 {
		return 0, fmt.Errorf("single-container item carries no appPort; the platform must pre-allocate one")
	}
	var tars []api.File
	for _, f := range manifest {
		if strings.HasSuffix(f.Path, ".tar") {
			tars = append(tars, f)
		}
	}
	if len(tars) != 1 {
		return 0, fmt.Errorf("single-container rows expect exactly one image tar, manifest has %d", len(tars))
	}
	if err := a.loadImageTars(ctx, item.DeploymentID, manifest); err != nil {
		return 0, err
	}
	service := strings.TrimSuffix(tars[0].Path, ".tar")
	tag := deployImageTag(item.DeploymentID, service)
	name := nodeComposeProject(item.DeploymentID)
	// Converge, not stack: a re-materialize replaces the old container.
	if _, err := a.docker()(ctx, "rm", "-f", name); err != nil {
		// absent container is the common case; docker prints nothing useful
		// here, a.logf keeps the trail without failing the run.
		a.logf("deploy %s: pre-run rm %s: %v", item.DeploymentID, name, err)
	}
	if _, err := a.docker()(ctx, "run", "-d", "--name", name,
		"--restart", "unless-stopped",
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", item.AppPort, item.AppPort),
		tag); err != nil {
		return 0, fmt.Errorf("docker run %s: %w", tag, err)
	}
	return item.AppPort, nil
}

// nodeTeardown is the user-node release path: no Coolify app to delete, so
// the agent tears down what IT created, converging by name without needing
// the compose file back -- the single container carries the project name, and
// compose stacks carry the com.docker.compose.project label
// (purgeComposeProject: containers, then networks, then volumes; same end
// state as the platform's release). Local images go last.
func (a *Agent) nodeTeardown(ctx context.Context, deploymentID string) {
	project := nodeComposeProject(deploymentID)
	if _, err := a.docker()(ctx, "rm", "-f", project); err != nil {
		// Absent is the common case for compose rows; log only.
		a.logf("deploy %s: container rm %s: %v", deploymentID, project, err)
	}
	a.purgeComposeProject(ctx, deploymentID, project)
	a.rmiDeployImages(ctx, deploymentID)
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

// handleAssertRoute is the periodic re-assert of an already-registered route
// (fix-caddy-route-reassert D2). Deliberately silent on every path: no event,
// no report, no failure. First registration reports EventRouteRegistered and
// a lost route would be a deployment failure -- but this is neither; it is
// routine maintenance over a healthy row, so a Caddy that happens to be mid-
// restart must not kill the deployment (EventMaterializeFailed here would
// flip a ready row to failed), and a successful re-assert must not write the
// row either (the event consumer UPDATEs on every report). Upsert is
// idempotent (PATCH-in-place first), so the steady state is a no-op.
func (a *Agent) handleAssertRoute(ctx context.Context, ep APIEndpoint, item api.WorkItem) error {
	if a.Ingress == nil {
		a.logf("deploy %s: assert skipped (no Caddy admin)", item.DeploymentID)
		return nil
	}
	addr := fmt.Sprintf("127.0.0.1:%d", item.AppPort)
	if err := a.Ingress.Upsert(ctx, item.DeploymentID, addr); err != nil {
		a.logf("deploy %s: route assert failed (will retry next cycle): %v", item.DeploymentID, err)
		return nil
	}
	a.logf("deploy %s: route %s -> %s asserted", item.DeploymentID, item.Host, addr)
	return nil
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
	// User node (byo-hosting-node 2.2): same order (route off first), then
	// the agent tears down what it created itself -- compose project down
	// (volumes included) or the single container, images, worktree. No
	// Coolify app, no git http port.
	if a.UserNode {
		if a.Ingress != nil {
			if err := a.Ingress.Remove(ctx, item.DeploymentID); err != nil {
				a.logf("deploy %s: route remove on release failed: %v", item.DeploymentID, err)
			}
		}
		a.nodeTeardown(ctx, item.DeploymentID)
		if err := a.Store.Remove(item.DeploymentID); err != nil {
			a.logf("deploy %s: worktree removal failed: %v", item.DeploymentID, err)
		}
		a.logf("deploy %s: released (route + containers + volumes + worktree recycled)", item.DeploymentID)
		return a.report(ctx, ep, item.DeploymentID, api.EventReleased, 0, "")
	}
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
	// Image-tier rows leave oa-deploy-* images in the local daemon; the
	// purge above only touches compose artifacts (containers/volumes/nets).
	a.rmiDeployImages(ctx, item.DeploymentID)
	a.logf("deploy %s: released (route + repo + port recycled)", item.DeploymentID)
	return a.report(ctx, ep, item.DeploymentID, api.EventReleased, 0, "")
}

// purgeComposeProject force-removes everything docker compose created for
// one project label: containers first (a mounted container pins its volume),
// then networks, then volumes. Idempotent -- resources Coolify's own async
// cleanup already took are the common case, and misses just log.
func (a *Agent) purgeComposeProject(ctx context.Context, deploymentID, project string) {
	// The daemon rejects a bare "com.docker.compose.project=x" filter key
	// ("invalid filter"); it must go through the "label=" key.
	label := "label=com.docker.compose.project=" + project
	// a.docker() keeps stderr in the error ("exit status 1" alone once hid
	// the actual CLI error and cost a blind debugging round, 8.17) and gives
	// tests the injectable seam.
	dockerRun := a.docker()
	for _, k := range []struct {
		name string
		list []string
		rm   []string
	}{
		{"container", []string{"ps", "-aq", "--filter", label}, []string{"rm", "-f"}},
		{"network", []string{"network", "ls", "-q", "--filter", label}, []string{"network", "rm"}},
		{"volume", []string{"volume", "ls", "-q", "--filter", label}, []string{"volume", "rm"}},
	} {
		out, err := dockerRun(ctx, k.list...)
		if err != nil {
			a.logf("deploy %s: compose %s list for purge failed: %v", deploymentID, k.name, err)
			continue
		}
		for _, id := range strings.Fields(out) {
			if _, err := dockerRun(ctx, append(append([]string{}, k.rm...), id)...); err != nil {
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
	w.CaddyAlive = a.caddyVerdict(ctx)
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

// caddyVerdict debounces the edge-proxy liveness probe (fix-caddy-route-
// reassert D4): nil means no confirmed verdict yet -- route wiring disabled,
// or fewer than 3 consecutive failed probes -- so the API stores unknown and
// does not alarm; false means 3 probes in a row failed; true means the last
// probe succeeded (recovery is reported immediately, decay is not).
func (a *Agent) caddyVerdict(ctx context.Context) *bool {
	if a.Ingress == nil {
		return nil
	}
	if err := a.Ingress.Ping(ctx); err != nil {
		a.caddyFails++
		if a.caddyFails < 3 {
			return nil
		}
		dead := false
		return &dead
	}
	a.caddyFails = 0
	alive := true
	return &alive
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
	// User nodes never sample usage (byo-hosting-node D1/D4): the platform's
	// usage surface is INTERNAL_SECRET-gated by design, and BYO rows bill the
	// flat management fee, not RAM·h -- there is nothing to sample for.
	if a.UserNode {
		return nil
	}
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
