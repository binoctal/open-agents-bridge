// Session output preview drive (session-output-online-preview group 4).
//
// The session tier's variant of RuntimePreviews with the Coolify ownership
// inverted: the AGENT materializes the bridge-packed work_dir snapshot into a
// bare repo and serves it over git-http (session_materialized + repoPort);
// the API then drives the Coolify application itself (build pack chosen from
// the snapshot's root: dockerfile-or-nixpacks) and flips the row ready. The
// agent's remaining halves are shared with the runtime tier: register_route
// wires the Caddy ingress + access log, the idle sweep re-asserts wiring and
// stops zero-access containers after the TTL, and stop tears container +
// route down. State persists per instance so a restart re-adopts the sweep.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/binoctal/open-agents/hosting-agent/internal/api"
	"github.com/binoctal/open-agents/hosting-agent/internal/githttp"
	"github.com/binoctal/open-agents/hosting-agent/internal/store"
)

// spCoolify is the Coolify seam this tier needs (stop only -- the API owns
// create/update/deploy).
type spCoolify interface {
	Stop(ctx context.Context, uuid string) error
}

// spAPI is the API worker seam (*api.Client in prod).
type spAPI interface {
	FetchSessionSnapshot(ctx context.Context, instanceID string) (*api.RuntimeSnapshotManifest, error)
	SessionPresign(ctx context.Context, instanceID string, shas []string) (*api.PresignResponse, error)
	ReportSessionPreview(ctx context.Context, instanceID, event string, repoPort int, coolifyAppID string, idle bool, detail string) error
}

// SessionPreviewConfig carries the session preview knobs (shared with the
// runtime tier's where noted).
type SessionPreviewConfig struct {
	// IdleTTL is the zero-access window before an idle stop (spec: 30min).
	IdleTTL time.Duration
	// LogDir holds the Caddy per-instance access logs (shared with runtime).
	LogDir string
	// DomainSuffix splits the {label}.{suffix} host back into its label.
	DomainSuffix string
	// StateDir holds {instanceId}.json sweep/idle state files.
	StateDir string
}

// spState is the persisted per-instance footprint (sweep only -- the repo
// port and app uuid live server-side once reported).
type spState struct {
	InstanceID   string    `json:"instanceId"`
	CoolifyAppID string    `json:"coolifyAppId"`
	Port         int       `json:"port"`
	Label        string    `json:"label"`
	Host         string    `json:"host"`
	StartedAt    time.Time `json:"startedAt"`
	APIBaseURL   string    `json:"apiBaseUrl"`

	// Idle tracking (in-memory re-baselined on load).
	LastAccess time.Time `json:"-"`
	LastOffset int64     `json:"-"`
	// ProbeFails counts consecutive dead-port probes (crash-loop signal).
	ProbeFails int `json:"-"`
}

// SessionPreviews owns the session preview agent half. Nil Ingress disables
// the route/log half (local dev without Caddy); nil Coolify disables stop's
// container half (reports still fire -- the API re-offers).
type SessionPreviews struct {
	Coolify spCoolify
	API     spAPI
	Ingress rtIngress // may be nil; shared seam with RuntimePreviews
	Store   *store.Store
	GitHTTP *githttp.Manager
	Cfg     SessionPreviewConfig

	// EndpointFor resolves a persisted APIBaseURL back to a live endpoint for
	// sweep-driven reports (dual-home parity with RuntimePreviews).
	EndpointFor func(baseURL string) SpEndpoint

	// Logf mirrors Agent.Logf; wired by main. Unset (tests) drops the line --
	// sweep-internal failures self-heal via re-offer, so the volume stays low.
	Logf func(format string, args ...any)

	// dial is the liveness seam (tests inject).
	dial func(ctx context.Context, port int) error

	mu         sync.Mutex
	byInstance map[string]*spState
	inFlight   map[string]bool
}

// SpEndpoint is the session tier's dispatch-time endpoint view: RtEndpoint's
// API field is typed to the runtime seam, and the session seam's method set
// differs (FetchSessionSnapshot/SessionPresign/ReportSessionPreview) even
// though *api.Client satisfies both.
type SpEndpoint struct {
	API     spAPI
	BaseURL string
}

// spRepoKeyFor is the per-instance repo key (git-http namespace).
func spRepoKeyFor(instanceID string) string { return "oa-session-preview-" + instanceID }

// ─── work dispatch ──────────────────────────────────────────────────────────

// dispatch routes session preview work items. up runs async (the snapshot
// fetch + repo build take minutes); register_route and stop are quick and
// synchronous. Every exchange goes through the offering endpoint (dual-home).
func (sp *SessionPreviews) dispatch(item api.WorkItem, ep SpEndpoint) {
	if sp == nil || item.InstanceID == "" {
		return
	}
	if !sp.claim(item.InstanceID) {
		return
	}
	switch item.Action {
	case api.ActionSessionPreviewUp:
		go func() {
			defer sp.release(item.InstanceID)
			ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Minute)
			defer cancel()
			if err := sp.up(ctx, item, ep); err != nil {
				sp.reportTo(ctx, ep.API, item.InstanceID, api.EventSessionMaterializeFail, 0, "", false, err.Error())
			}
		}()
	case api.ActionSessionPreviewRegisterRoute:
		defer sp.release(item.InstanceID)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := sp.registerRoute(ctx, item, ep); err != nil {
			sp.reportTo(ctx, ep.API, item.InstanceID, api.EventSessionFailed, 0, item.CoolifyAppID, false, err.Error())
		}
	case api.ActionSessionPreviewStop:
		defer sp.release(item.InstanceID)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := sp.stop(ctx, item, ep); err != nil {
			// A failed stop leaves the row stopping; the API re-offers.
			sp.logf("session preview stop failed for %s: %v", item.InstanceID, err)
		}
	default:
		sp.release(item.InstanceID)
	}
}

func (sp *SessionPreviews) claim(instanceID string) bool {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.inFlight == nil {
		sp.inFlight = map[string]bool{}
	}
	if sp.inFlight[instanceID] {
		return false
	}
	sp.inFlight[instanceID] = true
	return true
}

func (sp *SessionPreviews) release(instanceID string) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	delete(sp.inFlight, instanceID)
}

func (sp *SessionPreviews) logf(format string, args ...any) {
	// Wired by main to the process stdout shape; unset (tests) drops the
	// line. Volume is deliberately low: sweep-internal failures self-heal via
	// re-offer, but a stop that can never succeed must at least be visible
	// (the dead Coolify token would have been a 6-minute silent stall).
	if sp.Logf != nil {
		sp.Logf(format, args...)
	}
}

// ─── up: materialize + git-http (the API drives Coolify from here) ──────────

// up materializes the snapshot tree into a bare repo and serves it over
// git-http. No Dockerfile injection and no Coolify calls here -- the API
// owns the build (dockerfile-or-nixpacks from the snapshot's root). Every
// step is idempotent, so a stale-materializing re-offer re-converges.
func (sp *SessionPreviews) up(ctx context.Context, item api.WorkItem, ep SpEndpoint) error {
	manifest, err := ep.API.FetchSessionSnapshot(ctx, item.InstanceID)
	if err != nil {
		return fmt.Errorf("fetch snapshot: %w", err)
	}
	files := make([]api.File, 0, len(manifest.Files))
	for path, sha := range manifest.Files {
		files = append(files, api.File{Path: path, SHA256: sha})
	}
	presign := func(ctx context.Context, shas []string) (*api.PresignResponse, error) {
		return ep.API.SessionPresign(ctx, item.InstanceID, shas)
	}
	if err := sp.Store.FetchMissing(ctx, files, presign); err != nil {
		return fmt.Errorf("fetch objects: %w", err)
	}

	repoKey := spRepoKeyFor(item.InstanceID)
	if _, err := sp.Store.BuildRepo(ctx, repoKey, files, nil, false); err != nil {
		return fmt.Errorf("build repo: %w", err)
	}
	gitPort, err := sp.GitHTTP.Serve(repoKey)
	if err != nil {
		return fmt.Errorf("serve repo: %w", err)
	}

	// From here the API drives Coolify against this port; the row leaves
	// materializing only on the API's own transition.
	sp.reportTo(ctx, ep.API, item.InstanceID, api.EventSessionMaterialized, gitPort, "", false, "")
	return nil
}

// ─── register_route (row ready -> agent wires ingress -> running) ───────────

func (sp *SessionPreviews) registerRoute(ctx context.Context, item api.WorkItem, ep SpEndpoint) error {
	if item.AppPort <= 0 {
		return fmt.Errorf("register_route without appPort")
	}
	label := sp.labelOf(item.Host)
	if label == "" {
		return fmt.Errorf("host %q does not match domain suffix %q", item.Host, sp.Cfg.DomainSuffix)
	}
	if sp.Ingress != nil {
		if err := sp.Ingress.Upsert(ctx, label, net.JoinHostPort("127.0.0.1", strconv.Itoa(item.AppPort))); err != nil {
			return fmt.Errorf("route: %w", err)
		}
		if err := sp.Ingress.AttachAccessLog(ctx, item.InstanceID, item.Host); err != nil {
			return fmt.Errorf("access log: %w", err)
		}
	}

	st := &spState{
		InstanceID:   item.InstanceID,
		CoolifyAppID: item.CoolifyAppID,
		Port:         item.AppPort,
		Label:        label,
		Host:         item.Host,
		StartedAt:    time.Now().UTC(),
		APIBaseURL:   ep.BaseURL,
		LastAccess:   time.Now().UTC(),
	}
	sp.setActive(item.InstanceID, st)
	if err := sp.persist(st); err != nil {
		// Live and routed; a lost state file only costs the idle sweep's
		// re-baseline. Not fatal.
		_ = err
	}
	sp.reportTo(ctx, ep.API, item.InstanceID, api.EventSessionRouteRegistered, 0, item.CoolifyAppID, false, "")
	return nil
}

// ─── stop ────────────────────────────────────────────────────────────────────

// stop tears one instance down: container stopped (the API-driven Coolify app
// the item carries), route + access log detached, state dropped, reported.
// The app itself is kept server-side (a snapshot swap reuses it).
func (sp *SessionPreviews) stop(ctx context.Context, item api.WorkItem, ep SpEndpoint) error {
	st := sp.activeFor(item.InstanceID)
	appID := item.CoolifyAppID
	if st != nil && appID == "" {
		appID = st.CoolifyAppID
	}
	if err := sp.teardown(ctx, st, item, appID); err != nil {
		return err
	}
	sp.reportTo(ctx, ep.API, item.InstanceID, api.EventSessionStopped, 0, appID, false, "")
	return nil
}

// teardown is the shared stop machinery (container + wiring + state). The
// item's fields cover the untracked path (state lost to a restart): route
// removal needs only the host, access-log detach only the instance id.
func (sp *SessionPreviews) teardown(ctx context.Context, st *spState, item api.WorkItem, appID string) error {
	if appID != "" && sp.Coolify != nil {
		if err := sp.Coolify.Stop(ctx, appID); err != nil {
			return fmt.Errorf("coolify stop: %w", err)
		}
	}
	if sp.Ingress != nil {
		label := sp.labelOf(item.Host)
		if label == "" && st != nil {
			label = st.Label
		}
		if label != "" {
			if err := sp.Ingress.Remove(ctx, label); err != nil {
				return fmt.Errorf("route remove: %w", err)
			}
		}
		if item.InstanceID != "" {
			host := item.Host
			if host == "" && st != nil {
				host = st.Host
			}
			if host != "" {
				if err := sp.Ingress.DetachAccessLog(ctx, item.InstanceID, host); err != nil {
					return fmt.Errorf("access log detach: %w", err)
				}
			}
		}
	}
	sp.clearActive(item.InstanceID)
	sp.unpersist(item.InstanceID)
	return nil
}

// idleStop is stop driven by the sweep: same teardown, report carries
// idle=true so the API stamps idle_stopped_at.
func (sp *SessionPreviews) idleStop(ctx context.Context, st *spState) {
	item := api.WorkItem{InstanceID: st.InstanceID, Host: st.Host, CoolifyAppID: st.CoolifyAppID}
	if err := sp.teardown(ctx, st, item, st.CoolifyAppID); err != nil {
		// The sweep retries next tick; the instance stays tracked meanwhile.
		_ = err
		return
	}
	sp.reportTo(ctx, sp.endpointFor(st.APIBaseURL).API, st.InstanceID, api.EventSessionStopped, 0, st.CoolifyAppID, true, "idle")
}

// ─── idle sweep + liveness (tasks 4.2) ──────────────────────────────────────

// SweepIdle mirrors RuntimePreviews.SweepIdle: re-assert route + access log
// (a Caddyfile reload wipes admin config), read the access log for new
// bytes, idle-stop past the TTL, probe the port (3-fail streak ->
// session_failed).
func (sp *SessionPreviews) SweepIdle(ctx context.Context) {
	if sp == nil {
		return
	}
	sp.mu.Lock()
	states := make([]*spState, 0, len(sp.byInstance))
	for _, st := range sp.byInstance {
		states = append(states, st)
	}
	sp.mu.Unlock()
	for _, st := range states {
		if sp.Ingress != nil && st.Label != "" {
			if err := sp.Ingress.Upsert(ctx, st.Label, net.JoinHostPort("127.0.0.1", strconv.Itoa(st.Port))); err != nil {
				continue // Caddy unreachable; skip the sweep for this one
			}
			_ = sp.Ingress.AttachAccessLog(ctx, st.InstanceID, st.Host)
		}
		if sp.noteAccess(st) {
			st.ProbeFails = 0
			continue
		}
		if idleExpired(time.Now().UTC(), st.LastAccess, sp.Cfg.IdleTTL) {
			sp.idleStop(ctx, st)
			continue
		}
		if sp.probeAlive(ctx, st) {
			continue
		}
	}
}

// noteAccess marks st accessed when its log file grew past the tracked
// offset (same re-baseline discipline as the runtime tier).
func (sp *SessionPreviews) noteAccess(st *spState) bool {
	fi, err := os.Stat(sp.logPath(st.InstanceID))
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

func (sp *SessionPreviews) logPath(instanceID string) string {
	return filepath.Join(sp.Cfg.LogDir, instanceID+".log")
}

// probeAlive dials the published port; the 3-fail streak reports
// session_failed once (streak resets, matching both sibling tiers).
func (sp *SessionPreviews) probeAlive(ctx context.Context, st *spState) bool {
	dial := sp.dial
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
	sp.mu.Lock()
	delete(sp.byInstance, st.InstanceID)
	sp.mu.Unlock()
	sp.unpersist(st.InstanceID)
	sp.reportTo(ctx, sp.endpointFor(st.APIBaseURL).API, st.InstanceID, api.EventSessionFailed, 0, st.CoolifyAppID, false,
		fmt.Sprintf("port %d dead after 3 consecutive probes (crash-loop)", st.Port))
	return false
}

// ─── persistence + restart recovery ──────────────────────────────────────────

func (sp *SessionPreviews) statePath(instanceID string) string {
	return filepath.Join(sp.Cfg.StateDir, instanceID+".json")
}

func (sp *SessionPreviews) persist(st *spState) error {
	if err := os.MkdirAll(sp.Cfg.StateDir, 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(sp.statePath(st.InstanceID), raw, 0o644)
}

func (sp *SessionPreviews) unpersist(instanceID string) {
	_ = os.Remove(sp.statePath(instanceID))
}

func (sp *SessionPreviews) setActive(instanceID string, st *spState) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.byInstance == nil {
		sp.byInstance = map[string]*spState{}
	}
	sp.byInstance[instanceID] = st
}

func (sp *SessionPreviews) activeFor(instanceID string) *spState {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.byInstance[instanceID]
}

func (sp *SessionPreviews) clearActive(instanceID string) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	delete(sp.byInstance, instanceID)
}

// Recover re-adopts persisted instances after an agent restart (idle window
// re-baselined at now -- conservative early-stop direction).
func (sp *SessionPreviews) Recover() {
	if sp == nil {
		return
	}
	entries, err := os.ReadDir(sp.Cfg.StateDir)
	if err != nil {
		return // first boot: no state yet
	}
	now := time.Now().UTC()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		instanceID := strings.TrimSuffix(e.Name(), ".json")
		raw, err := os.ReadFile(sp.statePath(instanceID))
		if err != nil {
			continue
		}
		var st spState
		if json.Unmarshal(raw, &st) != nil || st.InstanceID == "" {
			continue
		}
		st.LastAccess = now
		st.ProbeFails = 0
		if fi, err := os.Stat(sp.logPath(st.InstanceID)); err == nil {
			st.LastOffset = fi.Size()
		}
		sp.setActive(instanceID, &st)
	}
}

// ─── report ──────────────────────────────────────────────────────────────────

func (sp *SessionPreviews) reportTo(ctx context.Context, cl spAPI, instanceID, event string, repoPort int, coolifyAppID string, idle bool, detail string) {
	if cl == nil {
		return
	}
	if err := cl.ReportSessionPreview(ctx, instanceID, event, repoPort, coolifyAppID, idle, detail); err != nil {
		// The API re-offers unconverged rows; a dropped report self-heals.
		_ = err
	}
}

func (sp *SessionPreviews) endpointFor(baseURL string) SpEndpoint {
	if sp.EndpointFor != nil {
		if ep := sp.EndpointFor(baseURL); ep.API != nil {
			return ep
		}
	}
	return SpEndpoint{API: sp.API}
}

// labelOf splits {label}.{suffix} back into the label ("" when the host does
// not sit under the configured domain).
func (sp *SessionPreviews) labelOf(host string) string {
	suffix := "." + sp.Cfg.DomainSuffix
	if !strings.HasSuffix(host, suffix) {
		return ""
	}
	return strings.TrimSuffix(host, suffix)
}
