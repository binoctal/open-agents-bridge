// Package node carries the node runtime that used to ship as the standalone
// hosting-agent binary. It runs in two forms:
//
//   - platform form (add-coolify-hosting design D2): systemd-managed on the
//     Coolify ECS node. It materializes deployment source trees from R2 into
//     bare git repos, serves them over loopback git http for Coolify to pull,
//     wires public hostnames through the Caddy admin API, drives runtime and
//     session preview instances, and reports node capacity to the API worker.
//
//   - user-node form (byo-hosting-node D1): credentials from a completed
//     `node register` handshake -- no INTERNAL_SECRET, no Coolify,
//     deployment-only command surface, agent-driven docker compose up, and a
//     Caddy on-demand-TLS ask forwarder.
//
// Process contract: systemd (or the operator) restarts on failure; exit code
// 1 with a clear message is the loud-failure channel (missing secret,
// rejected secret). Entry points are wired into the bridge CLI as the `node`
// subcommand (hosting-agent-into-bridge D2); the process model is unchanged
// -- this is a binary/distribution merge, not a daemon merge.
package node

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/binoctal/open-agents-bridge/node/agent"
	"github.com/binoctal/open-agents-bridge/node/api"
	"github.com/binoctal/open-agents-bridge/node/config"
	"github.com/binoctal/open-agents-bridge/node/coolify"
	"github.com/binoctal/open-agents-bridge/node/githttp"
	"github.com/binoctal/open-agents-bridge/node/ingress"
	"github.com/binoctal/open-agents-bridge/node/store"
)

// RunDaemon is the `node run` entry: it resolves config from the environment
// and dispatches to the user-node or platform form. nodeCredsPath, when set,
// is the --node-credentials flag and selects the user-node form.
func RunDaemon(nodeCredsPath string) error {
	// --node-credentials PATH selects the user-node form (also settable via
	// HOSTING_NODE_CREDENTIALS, which config reads directly).
	if nodeCredsPath != "" {
		os.Setenv("HOSTING_NODE_CREDENTIALS", nodeCredsPath)
	}

	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	for _, dir := range []string{cfg.CacheDir, cfg.CacheDir + "/objects", cfg.CacheDir + "/repos", cfg.CacheDir + "/runtime-previews", cfg.CacheDir + "/worktrees"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("prepare %s: %w", dir, err)
		}
	}

	logf := func(format string, args ...any) {
		fmt.Printf(time.Now().UTC().Format(time.RFC3339)+" "+format+"\n", args...)
	}

	// User-node form: one endpoint, node credentials, no Coolify surfaces,
	// ask forwarder + Caddy scaffold. Everything below branches on this.
	if cfg.NodeCreds != nil {
		return runUserNode(cfg, logf)
	}

	// One client per endpoint (dual-home-hosting-agent D1): each carries its
	// environment's base URL + internal secret, so work items report and
	// presign home to the environment that offered them (D2). The store's
	// default Presigner is the first endpoint -- hosted deployments pin the
	// offering endpoint per call via MaterializeVia.
	eps := make([]agent.APIEndpoint, 0, len(cfg.Endpoints))
	for _, ep := range cfg.Endpoints {
		eps = append(eps, agent.APIEndpoint{
			Client:             api.New(ep.BaseURL, cfg.AgentID, ep.InternalSecret),
			BaseURL:            ep.BaseURL,
			CoolifyProjectUUID: ep.CoolifyProjectUUID,
		})
	}
	primary := eps[0].Client
	st := store.New(cfg.CacheDir, primary)
	git := githttp.New(cfg.CacheDir+"/repos", cfg.GitBindHost, cfg.GitPortMin, cfg.GitPortMax)

	// One concrete Caddy writer serves both seams: the route-only Ingress
	// interface (deployments) and the access-log methods (runtime previews).
	var caddy *ingress.Caddy
	var routes agent.Ingress
	if cfg.CaddyAdminAddr != "" {
		caddy = ingress.NewCaddy(cfg.CaddyAdminAddr, cfg.DomainSuffix)
		caddy.LogDir = cfg.RuntimePreviewLogDir
		routes = caddy
	}

	// Runtime previews require the Coolify drive; an unconfigured token keeps
	// the whole feature off rather than 401-looping every runtime_up.
	var rt *agent.RuntimePreviews
	coolifyClient := coolify.New(cfg.CoolifyBaseURL, cfg.CoolifyAPIToken)
	if coolifyClient.Configured() && cfg.CoolifyProjectUUID != "" && cfg.CoolifyServerUUID != "" {
		rt = &agent.RuntimePreviews{
			Coolify: coolifyClient,
			API:     primary,
			// Sweep/recover reports resolve their owning environment through
			// the persisted APIBaseURL (dual-home D2).
			EndpointFor: func(baseURL string) agent.RtEndpoint {
				for _, ep := range eps {
					if ep.BaseURL == baseURL {
						return agent.RtEndpoint{API: ep.Client, BaseURL: ep.BaseURL, ProjectUUID: ep.CoolifyProjectUUID}
					}
				}
				return agent.RtEndpoint{}
			},
			Ingress: caddy, // nil-ingress-safe when Caddy is off
			Store:   st,
			GitHTTP: git,
			Cfg: agent.RuntimePreviewConfig{
				CoolifyProjectUUID:     cfg.CoolifyProjectUUID,
				CoolifyServerUUID:      cfg.CoolifyServerUUID,
				CoolifyEnvironmentName: cfg.CoolifyEnvironmentName,
				GitBindHost:            cfg.GitBindHost,
				PortMin:                cfg.RuntimePreviewPortMin,
				PortMax:                cfg.RuntimePreviewPortMax,
				IdleTTL:                time.Duration(cfg.RuntimePreviewIdleTTLMinutes) * time.Minute,
				LogDir:                 cfg.RuntimePreviewLogDir,
				DomainSuffix:           cfg.DomainSuffix,
				MemoryLimit:            "128m",
				StateDir:               cfg.CacheDir + "/runtime-previews",
			},
		}
	}

	// Session previews share the Caddy/coolify plumbing but invert the Coolify
	// ownership: the agent materializes + routes + sweeps, the API builds. The
	// Coolify gate is the same token; an unconfigured token keeps the tier off.
	var sp *agent.SessionPreviews
	if coolifyClient.Configured() && cfg.CoolifyProjectUUID != "" && cfg.CoolifyServerUUID != "" {
		sp = &agent.SessionPreviews{
			Coolify: coolifyClient, // Stop only; the API owns create/deploy
			API:     primary,
			EndpointFor: func(baseURL string) agent.SpEndpoint {
				for _, ep := range eps {
					if ep.BaseURL == baseURL {
						return agent.SpEndpoint{API: ep.Client, BaseURL: ep.BaseURL}
					}
				}
				return agent.SpEndpoint{}
			},
			Logf:    logf,
			Ingress: caddy, // nil-ingress-safe when Caddy is off
			Store:   st,
			GitHTTP: git,
			Cfg: agent.SessionPreviewConfig{
				IdleTTL:      time.Duration(cfg.SessionPreviewIdleTTLMinutes) * time.Minute,
				LogDir:       cfg.RuntimePreviewLogDir,
				DomainSuffix: cfg.DomainSuffix,
				StateDir:     cfg.CacheDir + "/session-previews",
			},
		}
	}

	a := &agent.Agent{
		Endpoints:         eps,
		Store:             st,
		GitHTTP:           git,
		Ingress:           routes,
		RT:                rt,
		SP:                sp,
		WorkPollInterval:  time.Duration(cfg.WorkPollIntervalSec) * time.Second,
		WatermarkInterval: time.Duration(cfg.WatermarkIntervalSec) * time.Second,
		WorkIdleMax:       time.Duration(cfg.WorkPollIdleMaxSec) * time.Second,
		Logf:              logf,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return a.Run(ctx)
}

// runUserNode wires the BYO user-node daemon (byo-hosting-node D1-D3):
// node-credential client, deployment-only agent, ask forwarder, and the
// Caddy scaffold the operator's caddy unit runs.
func runUserNode(cfg *config.Config, logf agent.Logf) error {
	creds := cfg.NodeCreds
	client := api.NewNode(creds.APIBaseURL, creds.NodeID, creds.NodeSecret)
	st := store.New(cfg.CacheDir, client)
	st.ScratchRoot = cfg.ScratchRoot
	// The scratch root is the same-machine declare-only window (D3): create it
	// at boot so a missing directory does not masquerade as path-escape
	// refusal when the first localPath entry resolves.
	if err := os.MkdirAll(cfg.ScratchRoot, 0o755); err != nil {
		return fmt.Errorf("prepare scratch root %s: %w", cfg.ScratchRoot, err)
	}
	// {ScratchRoot}/bridge is the SHARED staging dir the user's bridge writes
	// local-tier tar exports into. Failure is loud but non-fatal: local
	// declares degrade to "path not allowed" until the operator fixes it.
	ensureBridgeExportDir(cfg.ScratchRoot, logf)

	// Ask forwarder first (fail loud at boot if the port is taken): Caddy's
	// on_demand_tls handshake path depends on it.
	ask := &agent.AskServer{
		Ask:  client.Ask,
		Addr: cfg.AskListenAddr,
		Logf: logf,
	}
	stopAsk, err := ask.Start()
	if err != nil {
		return err
	}
	defer stopAsk()

	var routes agent.Ingress
	if cfg.CaddyAdminAddr != "" {
		caddy := ingress.NewCaddy(cfg.CaddyAdminAddr, cfg.DomainSuffix)
		routes = caddy
		if err := ensureNodeCaddyfile(cfg.CaddyfilePath, cfg.DomainSuffix, cfg.CaddyAdminAddr, cfg.AskListenAddr, logf); err != nil {
			return err
		}
		// Caddy availability (hosting-agent-into-bridge D4): operator-managed
		// when on PATH (process stays theirs); downloaded + managed as a child
		// process on a clean machine, dying with this daemon.
		caddyPath, managed, err := ensureCaddy(cfg.CacheDir, logf)
		if err != nil {
			return err
		}
		if managed {
			caddyCmd, err := startManagedCaddy(caddyPath, cfg.CaddyfilePath, logf)
			if err != nil {
				return err
			}
			defer func() {
				if p := caddyCmd.Process; p != nil {
					_ = p.Signal(syscall.SIGTERM)
					_, _ = p.Wait()
				}
			}()
		}
	}

	// Wake channel (hosted-deploy-push-wakeup): connect the node room so the
	// platform can nudge an immediate poll when work lands, instead of the
	// idle backoff deciding when the row is first seen. User-node form only:
	// the platform form's identity is the shared INTERNAL_SECRET, which has no
	// lane on this socket (the API's type=node branch verifies registry
	// credentials) -- it keeps polling exactly as before.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pollNow := make(chan struct{}, 1)
	if wakeURL := agent.WakeURL(creds.APIBaseURL, creds.NodeID, creds.NodeSecret); wakeURL != "" {
		go agent.RunWakeLoop(ctx, wakeURL, pollNow, logf)
	} else {
		logf("wake channel disabled: no derivable URL (poll-only mode)")
	}

	a := &agent.Agent{
		Endpoints: []agent.APIEndpoint{{
			Client:  client,
			BaseURL: creds.APIBaseURL,
		}},
		Store:             st,
		Ingress:           routes, // nil-safe: no Caddy admin = route wiring disabled
		UserNode:          true,
		PollNow:           pollNow,
		WorkPollInterval:  time.Duration(cfg.WorkPollIntervalSec) * time.Second,
		WatermarkInterval: time.Duration(cfg.WatermarkIntervalSec) * time.Second,
		WorkIdleMax:       time.Duration(cfg.WorkPollIdleMaxSec) * time.Second,
		Logf:              logf,
	}
	logf("user node %s online (platform %s, scratch root %s)", creds.NodeID, creds.APIBaseURL, cfg.ScratchRoot)

	return a.Run(ctx)
}

// ensureNodeCaddyfile writes the on-demand-TLS scaffold for the user node's
// Caddy (byo-hosting-node D2): the ask callback hits the agent's local
// forwarder, per-label certs issue only after the platform authorizes, and
// the wildcard catch-all answers 404 for unrouted labels. Idempotent: an
// unchanged file is left alone; a rewrite is announced so the operator
// reloads Caddy. Dynamic host routes (admin API) are NOT part of this file --
// they re-assert on the agent's own cadence after any reload.
func ensureNodeCaddyfile(path, domainSuffix, adminAddr, askAddr string, logf agent.Logf) error {
	if adminAddr == "" {
		adminAddr = "127.0.0.1:2019"
	}
	if askAddr == "" {
		askAddr = "127.0.0.1:20190"
	}
	content := fmt.Sprintf(`{
	admin %s
	on_demand_tls {
		ask http://%s/ask
	}
}

*.%s {
	tls {
		on_demand
	}
	respond "no route" 404
}
`, adminAddr, askAddr, domainSuffix)
	if existing, err := os.ReadFile(path); err == nil && string(existing) == content {
		return nil // already in place; never touches a matching file
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("prepare %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write Caddyfile scaffold %s: %w", path, err)
	}
	logf("wrote Caddyfile scaffold %s (admin %s, ask %s, on-demand TLS for *.%s); (re)start or reload caddy to apply", path, adminAddr, askAddr, domainSuffix)
	return nil
}

// ensureBridgeExportDir prepares {scratchRoot}/bridge, the SHARED staging
// directory the user's bridge writes local-tier tar exports into
// (byo-hosting-node D3). The bridge runs as the desktop user while this
// daemon usually runs as root: the scratch root itself stays 0755 and the
// staging leaf is world-writable, so the bridge can create its
// per-deployment dirs; the security boundary that matters is the store's
// prefix+sha check on every import, not this directory mode. Failure is
// warned, not fatal — local declares then degrade to "path not allowed"
// until the operator fixes the permissions.
func ensureBridgeExportDir(scratchRoot string, logf agent.Logf) {
	dir := filepath.Join(scratchRoot, "bridge")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		logf("warning: could not prepare the bridge export dir %s (%v); same-machine local declares will fail", dir, err)
		return
	}
	// MkdirAll applies umask; chmod the leaf so a stricter umask cannot
	// silently keep the bridge out.
	if err := os.Chmod(dir, 0o777); err != nil {
		logf("warning: could not open the bridge export dir %s for writing (%v)", dir, err)
	}
}
