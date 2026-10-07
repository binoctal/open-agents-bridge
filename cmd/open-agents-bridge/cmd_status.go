package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/binoctal/open-agents-bridge/internal/config"
	"github.com/spf13/cobra"
)

var statusMachine string

var statusCmd = &cobra.Command{
	Use:   "status -d <machine>",
	Short: "Show bridge status for a machine",
	Long:  `Display the current status of the Open Agents bridge for a specific machine.`,
	Run: func(cmd *cobra.Command, args []string) {
		machine := statusMachine
		if machine == "" {
			machine = os.Getenv("OPEN_AGENTS_MACHINE")
		}

		if machine == "" {
			fmt.Fprintln(os.Stderr, "Error: machine name is required (--machine or OPEN_AGENTS_MACHINE)")
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

		cfg, err := config.LoadMachine(machine)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: machine '%s' not found.\n", machine)
			os.Exit(1)
		}

		fmt.Println("Open Agents Bridge Status")
		fmt.Println("=========================")
		fmt.Printf("Machine:       %s\n", machine)
		fmt.Printf("Machine ID:    %s\n", cfg.MachineID)
		fmt.Printf("User ID:      %s\n", cfg.UserID)
		fmt.Printf("Server:       %s\n", cfg.ServerURL)
		fmt.Printf("Environment:  %s\n", cfg.GetEnvironment())
		fmt.Println()

		running := isBridgeRunning(machine)
		if running {
			fmt.Println("Status: Running")
		} else {
			fmt.Println("Status: Not running")
			fmt.Printf("Run 'open-agents-bridge start -d %s' to start the bridge.\n", machine)
		}
	},
}

func init() {
	statusCmd.Flags().StringVarP(&statusMachine, "machine", "d", "", "Machine name (required)")
}

// isBridgeRunning checks if a bridge process for the given machine is running
func isBridgeRunning(machine string) bool {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("tasklist", "/FI", "IMAGENAME eq open-agents-bridge.exe", "/FO", "CSV", "/NH")
	} else {
		cmd = exec.Command("pgrep", "-f", fmt.Sprintf("open-agents-bridge.*start.*-d.*%s", machine))
	}

	output, err := cmd.Output()
	if err != nil {
		return false
	}

	if runtime.GOOS == "windows" {
		return strings.Contains(string(output), "open-agents-bridge.exe")
	}
	return strings.TrimSpace(string(output)) != ""
}
