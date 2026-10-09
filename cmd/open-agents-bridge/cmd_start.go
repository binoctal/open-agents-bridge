package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/binoctal/open-agents-bridge/internal/bridge"
	"github.com/binoctal/open-agents-bridge/internal/config"
	"github.com/binoctal/open-agents-bridge/internal/instancelock"
	"github.com/binoctal/open-agents-bridge/internal/logger"
	"github.com/binoctal/open-agents-bridge/internal/tray"
	"github.com/spf13/cobra"
)

var (
	logLevel        string
	headless        bool
	machineName      string
	recordReplayDir string
	startUnsafe     bool
)

var startCmd = &cobra.Command{
	Use:   "start -d <machine>",
	Short: "Start the bridge daemon",
	Long: `Start the Open Agents bridge daemon. This connects your
local CLI tools to the cloud and enables remote monitoring
and control.

You must specify a machine name with --machine.

Examples:
  # Start a specific machine
  open-agents-bridge start -d work-pc

  # Start with debug logging
  open-agents-bridge start -d work-pc --log-level debug`,
	Run: func(cmd *cobra.Command, args []string) {
		// Determine which machine to use
		targetMachine := machineName
		if targetMachine == "" {
			targetMachine = os.Getenv("OPEN_AGENTS_MACHINE")
		}

		if targetMachine == "" && !config.SessionEnvActive() {
			fmt.Fprintln(os.Stderr, "Error: machine name is required.")
			fmt.Fprintln(os.Stderr, "Usage: open-agents-bridge start -d <machine>")
			fmt.Fprintln(os.Stderr)
			names, _ := config.ListMachines()
			if len(names) == 0 {
				fmt.Fprintln(os.Stderr, "No machines paired yet. Run 'open-agents-bridge pair' first.")
			} else {
				fmt.Fprintln(os.Stderr, "Available machines:")
				for _, n := range names {
					fmt.Fprintf(os.Stderr, "  - %s\n", n)
				}
			}
			os.Exit(1)
		}

		// Setup rotating logger
		l, err := logger.New()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating logger: %v\n", err)
			os.Exit(1)
		}
		defer l.Close()

		// Redirect standard library log to custom logger
		log.SetOutput(l.Writer())
		log.SetFlags(0)

		// Set log level from flag
		logger.SetGlobalLevel(logLevel)

		var cfg *config.Config

		if config.SessionEnvActive() {
			// Cloud session container: identity comes from the environment.
			cfg, err = config.FromSessionEnv()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			targetMachine = cfg.MachineID
		} else {
			cfg, err = config.LoadMachine(targetMachine)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: machine '%s' not found.\n", targetMachine)
			fmt.Fprintln(os.Stderr, "Run 'open-agents-bridge machines' to see available machines.")
			os.Exit(1)
		}

		// 5c.2: a hand-edited config must not redirect an official build to
		// an unofficial server (the machine token would go with it). Cloud
		// session containers get their URL from the platform environment.
		if !config.SessionEnvActive() {
			cp := requireAllowedServer(cfg.ServerURL, startUnsafe)
			fmt.Printf("  Control plane: %s (%s)\n", cp.Host, cp.Label())
		}

		machineDisplay := cfg.MachineName
		if machineDisplay == "" {
			machineDisplay = targetMachine
		}

		// One bridge per machine on this machine: take the kernel lock before any
		// network call so a second launch (service + manual start, a stray
		// terminal) exits instead of fighting the first over the connection.
		instLock, err := instancelock.Acquire(instancelock.PathFor(config.ConfigDir(), cfg.MachineID))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v.\n", err)
			fmt.Fprintf(os.Stderr, "Stop the other bridge for '%s' first (open-agents-bridge status).\n", machineDisplay)
			os.Exit(1)
		}
		defer instLock.Close()

		fmt.Printf("Starting Open Agents Bridge...\n")
		fmt.Printf("  Machine:   %s\n", machineDisplay)
		fmt.Printf("  Server:   %s\n", cfg.ServerURL)
		fmt.Printf("  MachineID: %s\n", cfg.MachineID)
		fmt.Printf("  📋 Logs:    %s\n", filepath.Join(logger.GetLogDir(), "bridge.log"))
		fmt.Println()

		b, err := bridge.New(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating bridge: %v\n", err)
			os.Exit(1)
		}

		// G17 replay recording: one JSONL wire-frame script per session,
		// used to produce the replay fixtures. Diagnostic mode — off unless
		// the flag is passed.
		if recordReplayDir != "" {
			if err := b.EnableReplayRecording(recordReplayDir); err != nil {
				fmt.Fprintf(os.Stderr, "Error enabling replay recording: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("  🎥 Recording replay scripts to: %s\n", recordReplayDir)
		}

		// Setup system tray notification
		trayTitle := "Open Agents"
		if cfg.MachineName != "" {
			trayTitle = fmt.Sprintf("Open Agents (%s)", cfg.MachineName)
		}
		t := tray.New(trayTitle)
		t.SetRunning(true)
		t.ShowNotification("Open Agents", fmt.Sprintf("Bridge started (%s)", machineDisplay))

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

		go func() {
			<-sigChan
			logger.Info("Shutting down...")
			t.SetRunning(false)
			t.ShowNotification("Open Agents", "Bridge stopped")
			b.Stop()
		}()

		if err := b.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Bridge error: %v\n", err)
			fmt.Fprintf(os.Stderr, "   See logs at: %s\n", filepath.Join(logger.GetLogDir(), "bridge.log"))
			os.Exit(1)
		}

	},
}

func init() {
	startCmd.Flags().StringVarP(&logLevel, "log-level", "l", "info", "Log level (error, warn, info, debug)")
	startCmd.Flags().BoolVarP(&headless, "headless", "H", false, "Run in headless mode (no system tray)")
	startCmd.Flags().BoolVar(&startUnsafe, "unsafe-server", false, "Allow a non-official server in an official build")
	startCmd.Flags().StringVarP(&machineName, "machine", "d", "", "Machine name to start (required)")
	startCmd.Flags().StringVar(&recordReplayDir, "record-replay-dir", "", "Record ACP wire frames of every session to <dir>/<sessionID>.jsonl (replay fixture production)")
}
