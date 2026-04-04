package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/open-agents/open-agents-bridge/internal/bridge"
	"github.com/open-agents/open-agents-bridge/internal/config"
	"github.com/open-agents/open-agents-bridge/internal/logger"
	"github.com/open-agents/open-agents-bridge/internal/tray"
	"github.com/spf13/cobra"
)

var (
	logLevel   string
	headless   bool
	deviceName string
)

var startCmd = &cobra.Command{
	Use:   "start -d <device>",
	Short: "Start the bridge daemon",
	Long: `Start the Open Agents bridge daemon. This connects your
local CLI tools to the cloud and enables remote monitoring
and control.

You must specify a device name with --device.

Examples:
  # Start a specific device
  open-agents start -d work-pc

  # Start with debug logging
  open-agents start -d work-pc --log-level debug`,
	Run: func(cmd *cobra.Command, args []string) {
		// Determine which device to use
		targetDevice := deviceName
		if targetDevice == "" {
			targetDevice = os.Getenv("OPEN_AGENTS_DEVICE")
		}

		if targetDevice == "" {
			fmt.Fprintln(os.Stderr, "Error: device name is required.")
			fmt.Fprintln(os.Stderr, "Usage: open-agents start -d <device>")
			fmt.Fprintln(os.Stderr)
			names, _ := config.ListDevices()
			if len(names) == 0 {
				fmt.Fprintln(os.Stderr, "No devices paired yet. Run 'open-agents pair' first.")
			} else {
				fmt.Fprintln(os.Stderr, "Available devices:")
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

		cfg, err = config.LoadDevice(targetDevice)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: device '%s' not found.\n", targetDevice)
			fmt.Fprintln(os.Stderr, "Run 'open-agents devices' to see available devices.")
			os.Exit(1)
		}

		deviceDisplay := cfg.DeviceName
		if deviceDisplay == "" {
			deviceDisplay = targetDevice
		}

		fmt.Printf("Starting Open Agents Bridge...\n")
		fmt.Printf("  Device:   %s\n", deviceDisplay)
		fmt.Printf("  Server:   %s\n", cfg.ServerURL)
		fmt.Printf("  DeviceID: %s\n", cfg.DeviceID)
		fmt.Printf("  📋 Logs:    %s\n", filepath.Join(logger.GetLogDir(), "bridge.log"))
		fmt.Println()

		b, err := bridge.New(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating bridge: %v\n", err)
			os.Exit(1)
		}

		// Setup system tray notification
		trayTitle := "Open Agents"
		if cfg.DeviceName != "" {
			trayTitle = fmt.Sprintf("Open Agents (%s)", cfg.DeviceName)
		}
		t := tray.New(trayTitle)
		t.SetRunning(true)
		t.ShowNotification("Open Agents", fmt.Sprintf("Bridge started (%s)", deviceDisplay))

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
	startCmd.Flags().StringVarP(&deviceName, "device", "d", "", "Device name to start (required)")
}
