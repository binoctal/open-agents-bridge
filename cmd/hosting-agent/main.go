// hosting-agent runs on the Coolify ECS node (add-coolify-hosting design D2):
// it materializes deployment source trees from R2 into bare git repos, serves
// them over loopback git http for Coolify to pull, wires public hostnames
// through the Caddy admin API, and reports node capacity to the API worker.
// Since add-dynamic-preview-compute it also drives runtime preview instances
// (per-mission Coolify apps + idle auto-stop).
//
// Process contract: systemd restarts on failure; exit code 1 with a clear
// message is the loud-failure channel (missing secret, rejected secret).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/binoctal/open-agents/hosting-agent/internal/agent"
	"github.com/binoctal/open-agents/hosting-agent/internal/api"
	"github.com/binoctal/open-agents/hosting-agent/internal/config"
	"github.com/binoctal/open-agents/hosting-agent/internal/coolify"
	"github.com/binoctal/open-agents/hosting-agent/internal/githttp"
	"github.com/binoctal/open-agents/hosting-agent/internal/ingress"
	"github.com/binoctal/open-agents/hosting-agent/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "hosting-agent: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	for _, dir := range []string{cfg.CacheDir, cfg.CacheDir + "/objects", cfg.CacheDir + "/repos", cfg.CacheDir + "/runtime-previews"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("prepare %s: %w", dir, err)
		}
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
		Logf: func(format string, args ...any) {
			fmt.Printf(time.Now().UTC().Format(time.RFC3339)+" "+format+"\n", args...)
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return a.Run(ctx)
}
