package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/open-agents/open-agents-bridge/internal/config"
	"github.com/spf13/cobra"
)

var devicesJSON bool

var devicesCmd = &cobra.Command{
	Use:   "devices",
	Short: "List all paired devices",
	Long: `List all paired devices and show their key information.

Examples:
  # List devices
  open-agents devices

  # JSON output for scripting
  open-agents devices --json`,
	Run: func(cmd *cobra.Command, args []string) {
		type deviceInfo struct {
			Name        string `json:"name"`
			DeviceID    string `json:"deviceId"`
			ServerURL   string `json:"serverUrl"`
			Environment string `json:"environment"`
		}

		names, err := config.ListDevices()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error listing devices: %v\n", err)
			os.Exit(1)
		}

		if len(names) == 0 {
			fmt.Println("No devices paired yet.")
			fmt.Println("Run 'open-agents pair' to pair your first device.")
			return
		}

		var devices []deviceInfo
		for _, name := range names {
			cfg, err := config.LoadDevice(name)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not load device '%s': %v\n", name, err)
				continue
			}
			devices = append(devices, deviceInfo{
				Name:        name,
				DeviceID:    cfg.DeviceID,
				ServerURL:   cfg.ServerURL,
				Environment: cfg.GetEnvironment(),
			})
		}

		// JSON output
		if devicesJSON {
			data, err := json.MarshalIndent(devices, "", "  ")
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error marshaling JSON: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(string(data))
			return
		}

		// Table output
		fmt.Println("Paired Devices:")
		fmt.Println()

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  NAME\tDEVICE ID\tSERVER\tENV")
		fmt.Fprintln(w, "  ----\t---------\t------\t---")

		for _, d := range devices {
			shortID := d.DeviceID
			if len(shortID) > 12 {
				shortID = shortID[:12]
			}
			server := d.ServerURL
			if len(server) > 40 {
				server = server[:37] + "..."
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", d.Name, shortID, server, d.Environment)
		}
		w.Flush()
	},
}

func init() {
	devicesCmd.Flags().BoolVar(&devicesJSON, "json", false, "Output in JSON format")
}
