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

	client := api.New(cfg.APIBaseURL, cfg.AgentID, cfg.InternalSecret)
	st := store.New(cfg.CacheDir, client)
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
			API:     client,
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

	a := &agent.Agent{
		Client:            client,
		Store:             st,
		GitHTTP:           git,
		Ingress:           routes,
		RT:                rt,
		WorkPollInterval:  time.Duration(cfg.WorkPollIntervalSec) * time.Second,
		WatermarkInterval: time.Duration(cfg.WatermarkIntervalSec) * time.Second,
		Logf: func(format string, args ...any) {
			fmt.Printf(time.Now().UTC().Format(time.RFC3339)+" "+format+"\n", args...)
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return a.Run(ctx)
}
