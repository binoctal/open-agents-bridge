// Package agent is the hosting-agent's orchestration core: a work poll loop
// (materialize / route wiring / release, driven by the API) and a watermark
// report loop (node capacity for the API's reject-new governance, design D6).
//
// Both loops are best-effort per tick with one exception: an Unauthorized
// from the API is fatal and exits the process (rotated secret -> restart
// churn a human sees, node-runner's fatalHeartbeat pattern).
package agent

import (
	"bufio"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
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

// Agent wires the loops together.
type Agent struct {
	Client  *api.Client
	Store   *store.Store
	GitHTTP *githttp.Manager
	Ingress Ingress // may be nil (local dev without Caddy)

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

	// inFlight guards against overlapping materializes of one deployment
	// (a slow tree while the next poll re-offers the same work item).
	inFlightMu sync.Mutex
	inFlight   map[string]bool
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

// pollWork fetches one batch and handles each item sequentially -- a single
// node serves at most a handful of whitelist deployments; per-item failures
// are reported to the API and never abort the batch. A credential rejection
// aborts everything: it returns to Run, which returns it to main (exit 1).
func (a *Agent) pollWork(ctx context.Context) error {
	items, err := a.Client.GetWork(ctx)
	if err != nil {
		if api.Unauthorized(err) {
			return err
		}
		a.logf("work poll failed: %v", err)
		return nil
	}
	for _, item := range items {
		if err := a.handle(ctx, item); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) handle(ctx context.Context, item api.WorkItem) error {
	if !a.claim(item.DeploymentID) {
		a.logf("deploy %s: %s already in flight, skipping", item.DeploymentID, item.Action)
		return nil
	}
	defer a.release(item.DeploymentID)

	switch item.Action {
	case api.ActionMaterialize:
		return a.handleMaterialize(ctx, item)
	case api.ActionRegisterRoute:
		return a.handleRegisterRoute(ctx, item)
	case api.ActionRemoveRoute:
		return a.handleRemoveRoute(ctx, item)
	case api.ActionRelease:
		return a.handleRelease(ctx, item)
	default:
		a.logf("deploy %s: unknown action %q", item.DeploymentID, item.Action)
		return nil
	}
}

func (a *Agent) handleMaterialize(ctx context.Context, item api.WorkItem) error {
	// The manifest rides R2, not the work item (D1 statement cap): fetch it
	// first -- a manifest the agent cannot read is a materialize failure,
	// same as an unreadable object later in the loop.
	manifest, err := a.Client.FetchManifest(ctx, item.ManifestURL)
	if err != nil {
		a.logf("deploy %s: manifest fetch failed: %v", item.DeploymentID, err)
		return a.report(ctx, item.DeploymentID, api.EventMaterializeFailed, 0, err.Error())
	}
	commit, err := a.Store.Materialize(ctx, item.DeploymentID, item.SourcePrefix, manifest)
	if err != nil {
		a.logf("deploy %s: materialize failed: %v", item.DeploymentID, err)
		return a.report(ctx, item.DeploymentID, api.EventMaterializeFailed, 0, err.Error())
	}
	port, err := a.GitHTTP.Serve(item.DeploymentID)
	if err != nil {
		a.logf("deploy %s: git http serve failed: %v", item.DeploymentID, err)
		return a.report(ctx, item.DeploymentID, api.EventMaterializeFailed, 0, err.Error())
	}
	a.logf("deploy %s: materialized %s at http://127.0.0.1:%d/%s.git",
		item.DeploymentID, commit[:12], port, item.DeploymentID)
	return a.report(ctx, item.DeploymentID, api.EventMaterialized, port, commit)
}

func (a *Agent) handleRegisterRoute(ctx context.Context, item api.WorkItem) error {
	if a.Ingress == nil {
		a.logf("deploy %s: route wiring disabled (no Caddy admin), reporting failure", item.DeploymentID)
		return a.report(ctx, item.DeploymentID, api.EventMaterializeFailed, 0, "route wiring disabled")
	}
	addr := fmt.Sprintf("127.0.0.1:%d", item.AppPort)
	if err := a.Ingress.Upsert(ctx, item.DeploymentID, addr); err != nil {
		a.logf("deploy %s: route upsert failed: %v", item.DeploymentID, err)
		return a.report(ctx, item.DeploymentID, api.EventMaterializeFailed, 0, err.Error())
	}
	a.logf("deploy %s: route %s -> %s registered", item.DeploymentID, item.Host, addr)
	return a.report(ctx, item.DeploymentID, api.EventRouteRegistered, 0, item.Host)
}

func (a *Agent) handleRemoveRoute(ctx context.Context, item api.WorkItem) error {
	if a.Ingress != nil {
		if err := a.Ingress.Remove(ctx, item.DeploymentID); err != nil {
			a.logf("deploy %s: route remove failed: %v", item.DeploymentID, err)
			return a.report(ctx, item.DeploymentID, api.EventMaterializeFailed, 0, err.Error())
		}
	}
	a.logf("deploy %s: route removed", item.DeploymentID)
	return a.report(ctx, item.DeploymentID, api.EventRouteRemoved, 0, "")
}

func (a *Agent) handleRelease(ctx context.Context, item api.WorkItem) error {
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
		return a.report(ctx, item.DeploymentID, api.EventMaterializeFailed, 0, err.Error())
	}
	a.logf("deploy %s: released (route + repo + port recycled)", item.DeploymentID)
	return a.report(ctx, item.DeploymentID, api.EventReleased, 0, "")
}

// report posts one event; a 401 is fatal (returned so Run/main exit), anything
// else is logged (the API re-offers the work item on the next poll, so a
// dropped report self-heals).
func (a *Agent) report(ctx context.Context, deploymentID, event string, repoPort int, detail string) error {
	if err := a.Client.Report(ctx, deploymentID, event, repoPort, detail); err != nil {
		if api.Unauthorized(err) {
			return err
		}
		a.logf("deploy %s: report %s failed: %v", deploymentID, event, err)
	}
	return nil
}

func (a *Agent) reportWatermark(ctx context.Context) error {
	w := CollectWatermark(ctx)
	w.BootID = a.BootID
	if err := a.Client.ReportWatermark(ctx, w); err != nil {
		if api.Unauthorized(err) {
			return err
		}
		a.logf("watermark report failed: %v", err)
	}
	return nil
}

// reportUsage samples per-container memory and posts the batch. Skipped when
// no Coolify app containers are running (zero usage, and the watermark loop
// already carries liveness); a non-401 failure is logged, not fatal -- the
// next tick's batch covers the gap and the API's rollup treats a sample-less
// ready hour conservatively by design.
func (a *Agent) reportUsage(ctx context.Context) error {
	samples := CollectUsageSamples(ctx, time.Now().UTC())
	if len(samples) == 0 {
		return nil
	}
	if err := a.Client.ReportUsage(ctx, samples); err != nil {
		if api.Unauthorized(err) {
			return err
		}
		a.logf("usage report failed: %v", err)
	}
	return nil
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

// CollectWatermark reads node capacity: MemAvailable/MemTotal from
// /proc/meminfo and the running-container count from docker (best-effort --
// a docker hiccup reports 0 containers, never blocks the loop).
func CollectWatermark(ctx context.Context) api.Watermark {
	w := api.Watermark{
		MemoryAvailableBytes: meminfo("MemAvailable"),
		MemoryTotalBytes:     meminfo("MemTotal"),
	}
	if out, err := exec.CommandContext(ctx, "docker", "ps", "--filter", "status=running", "-q").Output(); err == nil {
		w.ContainersRunning = len(strings.Fields(string(out)))
	}
	return w
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
		"--format", "{{.ID}}\t{{.MemUsage}}").Output()
	if err != nil {
		return nil
	}
	memByID := make(map[string]uint64)
	for _, line := range strings.Split(string(statsOut), "\n") {
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 {
			continue
		}
		// MemUsage renders as "used / limit"; the used half is the reading.
		used := strings.SplitN(fields[1], "/", 2)[0]
		if b, ok := parseDockerSize(used); ok {
			memByID[fields[0]] = b
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
				TS:           ts,
			})
		}
	}
	return samples
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
