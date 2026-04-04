package main

import (
	"fmt"
	"os"

	"github.com/open-agents/open-agents-bridge/internal/config"
	"github.com/spf13/cobra"
)

var statusDevice string

var statusCmd = &cobra.Command{
	Use:   "status -d <device>",
	Short: "Show bridge status for a device",
	Long:  `Display the current status of the Open Agents bridge for a specific device.`,
	Run: func(cmd *cobra.Command, args []string) {
		device := statusDevice
		if device == "" {
			device = os.Getenv("OPEN_AGENTS_DEVICE")
		}

		if device == "" {
			fmt.Fprintln(os.Stderr, "Error: device name is required (--device or OPEN_AGENTS_DEVICE)")
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

		cfg, err := config.LoadDevice(device)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: device '%s' not found.\n", device)
			os.Exit(1)
		}

		fmt.Println("Open Agents Bridge Status")
		fmt.Println("=========================")
		fmt.Printf("Device:       %s\n", device)
		fmt.Printf("Device ID:    %s\n", cfg.DeviceID)
		fmt.Printf("User ID:      %s\n", cfg.UserID)
		fmt.Printf("Server:       %s\n", cfg.ServerURL)
		fmt.Printf("Environment:  %s\n", cfg.GetEnvironment())
		fmt.Println()

		// TODO: Check if bridge is running
		fmt.Println("Status: Configured (not running)")
		fmt.Printf("Run 'open-agents start -d %s' to start the bridge.\n", device)
	},
}

func init() {
	statusCmd.Flags().StringVarP(&statusDevice, "device", "d", "", "Device name (required)")
}
