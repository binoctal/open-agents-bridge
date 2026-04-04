package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/open-agents/open-agents-bridge/internal/config"
	"github.com/spf13/cobra"
)

var (
	cleanAll   bool
	cleanForce bool
)

var cleanCmd = &cobra.Command{
	Use:   "clean [device-name]",
	Short: "Remove local device configuration",
	Long: `Remove local device configuration.

Examples:
  # Remove a specific device
  open-agents clean my-device

  # Remove all devices
  open-agents clean --all

  # Skip confirmation prompt
  open-agents clean --all --force`,
	Args: cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		if cleanAll {
			cleanAllDevices()
			return
		}

		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "Error: specify a device name, or use --all")
			fmt.Fprintln(os.Stderr, "Run 'open-agents devices' to see paired devices.")
			os.Exit(1)
		}

		cleanDevice(args[0])
	},
}

func cleanDevice(name string) {
	if !config.DeviceExists(name) {
		fmt.Fprintf(os.Stderr, "Error: device '%s' not found\n", name)
		fmt.Fprintln(os.Stderr, "Run 'open-agents devices' to see paired devices.")
		os.Exit(1)
	}

	cfg, err := config.LoadDevice(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading device config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Device: %s\n", name)
	fmt.Printf("  Device ID: %s\n", cfg.DeviceID)
	fmt.Printf("  Server:    %s\n", cfg.ServerURL)
	fmt.Println()

	if !cleanForce {
		fmt.Printf("Remove device '%s'? [y/N] ", name)
		reader := bufio.NewReader(os.Stdin)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(strings.ToLower(input))
		if input != "y" && input != "yes" {
			fmt.Println("Cancelled.")
			return
		}
	}

	if err := config.DeleteDevice(name); err != nil {
		fmt.Fprintf(os.Stderr, "Error removing device: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✓ Device '%s' removed.\n", name)
}

func cleanAllDevices() {
	devices, _ := config.ListDevices()

	if len(devices) == 0 {
		fmt.Println("No devices to clean.")
		return
	}

	fmt.Println("This will remove:")
	for _, name := range devices {
		fmt.Printf("  - %s\n", name)
	}
	fmt.Println()

	if !cleanForce {
		fmt.Print("Remove all devices? [y/N] ")
		reader := bufio.NewReader(os.Stdin)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(strings.ToLower(input))
		if input != "y" && input != "yes" {
			fmt.Println("Cancelled.")
			return
		}
	}

	for _, name := range devices {
		if err := config.DeleteDevice(name); err != nil {
			fmt.Fprintf(os.Stderr, "Error removing device '%s': %v\n", name, err)
		} else {
			fmt.Printf("✓ Removed device: %s\n", name)
		}
	}

	fmt.Println()
	fmt.Println("All devices removed. Run 'open-agents pair' to pair a new device.")
}

func init() {
	cleanCmd.Flags().BoolVar(&cleanAll, "all", false, "Remove all devices")
	cleanCmd.Flags().BoolVar(&cleanForce, "force", false, "Skip confirmation prompt")
}
