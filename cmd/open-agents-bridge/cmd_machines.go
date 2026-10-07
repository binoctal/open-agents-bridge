package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/binoctal/open-agents-bridge/internal/config"
	"github.com/spf13/cobra"
)

var machinesJSON bool

var machinesCmd = &cobra.Command{
	Use:   "machines",
	Short: "List all paired machines",
	Long: `List all paired machines and show their key information.

Examples:
  # List machines
  open-agents-bridge machines

  # JSON output for scripting
  open-agents-bridge machines --json`,
	Run: func(cmd *cobra.Command, args []string) {
		type machineInfo struct {
			Name        string `json:"name"`
			MachineID    string `json:"machineId"`
			ServerURL   string `json:"serverUrl"`
			Environment string `json:"environment"`
		}

		names, err := config.ListMachines()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error listing machines: %v\n", err)
			os.Exit(1)
		}

		if len(names) == 0 {
			fmt.Println("No machines paired yet.")
			fmt.Println("Run 'open-agents-bridge pair' to pair your first machine.")
			return
		}

		var machines []machineInfo
		for _, name := range names {
			cfg, err := config.LoadMachine(name)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not load machine '%s': %v\n", name, err)
				continue
			}
			machines = append(machines, machineInfo{
				Name:        name,
				MachineID:    cfg.MachineID,
				ServerURL:   cfg.ServerURL,
				Environment: cfg.GetEnvironment(),
			})
		}

		// JSON output
		if machinesJSON {
			data, err := json.MarshalIndent(machines, "", "  ")
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error marshaling JSON: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(string(data))
			return
		}

		// Table output
		fmt.Println("Paired Machines:")
		fmt.Println()

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  NAME\tMACHINE ID\tSERVER\tENV")
		fmt.Fprintln(w, "  ----\t---------\t------\t---")

		for _, d := range machines {
			shortID := d.MachineID
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
	machinesCmd.Flags().BoolVar(&machinesJSON, "json", false, "Output in JSON format")
}
