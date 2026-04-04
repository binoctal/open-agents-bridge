package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "open-agents-bridge",
	Short: "Open Agents Bridge - Connect AI CLI tools to the cloud",
	Long: `Open Agents Bridge is a local daemon that connects your AI CLI tools
(Kiro, Claude, Cline, Codex, Gemini) to the Open Agents cloud platform.

It enables remote monitoring, permission management, and real-time
collaboration across multiple devices.`,
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(pairCmd)
	rootCmd.AddCommand(devicesCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(serviceCmd)
	rootCmd.AddCommand(logsCmd)
	rootCmd.AddCommand(cleanCmd)
	rootCmd.AddCommand(updateCmd)
	rootCmd.AddCommand(versionCmd)
}
