package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/binoctal/open-agents-bridge/internal/config"
	"github.com/spf13/cobra"
)

var (
	cleanAll   bool
	cleanForce bool
)

var cleanCmd = &cobra.Command{
	Use:   "clean [machine-name]",
	Short: "Remove local machine configuration",
	Long: `Remove local machine configuration.

Examples:
  # Remove a specific machine
  open-agents-bridge clean my-machine

  # Remove all machines
  open-agents-bridge clean --all

  # Skip confirmation prompt
  open-agents-bridge clean --all --force`,
	Args: cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		if cleanAll {
			cleanAllMachines()
			return
		}

		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "Error: specify a machine name, or use --all")
			fmt.Fprintln(os.Stderr, "Run 'open-agents-bridge machines' to see paired machines.")
			os.Exit(1)
		}

		cleanMachine(args[0])
	},
}

func cleanMachine(name string) {
	if !config.MachineExists(name) {
		fmt.Fprintf(os.Stderr, "Error: machine '%s' not found\n", name)
		fmt.Fprintln(os.Stderr, "Run 'open-agents-bridge machines' to see paired machines.")
		os.Exit(1)
	}

	cfg, err := config.LoadMachine(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading machine config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Machine: %s\n", name)
	fmt.Printf("  Machine ID: %s\n", cfg.MachineID)
	fmt.Printf("  Server:    %s\n", cfg.ServerURL)
	fmt.Println()

	if !cleanForce {
		fmt.Printf("Remove machine '%s'? [y/N] ", name)
		reader := bufio.NewReader(os.Stdin)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(strings.ToLower(input))
		if input != "y" && input != "yes" {
			fmt.Println("Cancelled.")
			return
		}
	}

	if err := config.DeleteMachine(name); err != nil {
		fmt.Fprintf(os.Stderr, "Error removing machine: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✓ Machine '%s' removed.\n", name)
}

func cleanAllMachines() {
	machines, _ := config.ListMachines()

	if len(machines) == 0 {
		fmt.Println("No machines to clean.")
		return
	}

	fmt.Println("This will remove:")
	for _, name := range machines {
		fmt.Printf("  - %s\n", name)
	}
	fmt.Println()

	if !cleanForce {
		fmt.Print("Remove all machines? [y/N] ")
		reader := bufio.NewReader(os.Stdin)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(strings.ToLower(input))
		if input != "y" && input != "yes" {
			fmt.Println("Cancelled.")
			return
		}
	}

	for _, name := range machines {
		if err := config.DeleteMachine(name); err != nil {
			fmt.Fprintf(os.Stderr, "Error removing machine '%s': %v\n", name, err)
		} else {
			fmt.Printf("✓ Removed machine: %s\n", name)
		}
	}

	fmt.Println()
	fmt.Println("All machines removed. Run 'open-agents-bridge pair' to pair a new machine.")
}

func init() {
	cleanCmd.Flags().BoolVar(&cleanAll, "all", false, "Remove all machines")
	cleanCmd.Flags().BoolVar(&cleanForce, "force", false, "Skip confirmation prompt")
}
