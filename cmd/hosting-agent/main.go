// hosting-agent runs on the Coolify ECS node (add-coolify-hosting design D2):
// it materializes deployment source trees from R2 into bare git repos, serves
// them over loopback git http for Coolify to pull, wires public hostnames
// through the Caddy admin API, and reports node capacity to the API worker.
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
	"github.com/binoctal/open-agents/hosting-agent/internal/githttp"
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
	for _, dir := range []string{cfg.CacheDir, cfg.CacheDir + "/objects", cfg.CacheDir + "/repos"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("prepare %s: %w", dir, err)
		}
	}

	client := api.New(cfg.APIBaseURL, cfg.AgentID, cfg.InternalSecret)
	st := store.New(cfg.CacheDir, client)
	git := githttp.New(cfg.CacheDir+"/repos", cfg.GitBindHost, cfg.GitPortMin, cfg.GitPortMax)

	a := &agent.Agent{
		Client:            client,
		Store:             st,
		GitHTTP:           git,
		Ingress:           agent.NewCaddyIngress(cfg.CaddyAdminAddr, cfg.DomainSuffix),
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
