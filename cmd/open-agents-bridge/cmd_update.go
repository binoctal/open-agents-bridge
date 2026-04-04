package main

import (
	"fmt"
	"os"

	"github.com/open-agents/open-agents-bridge/internal/updater"
	"github.com/spf13/cobra"
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Check for and install updates",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("Current version: %s\n", updater.Version)
		fmt.Println("Checking for updates...")

		result, err := updater.CheckUpdate()
		if err != nil {
			fmt.Printf("Failed to check for updates: %v\n", err)
			os.Exit(1)
		}

		if !result.HasUpdate {
			fmt.Println("You are running the latest version.")
			return
		}

		fmt.Printf("New version available: %s\n", result.LatestVersion)

		if result.DownloadURL == "" {
			fmt.Println("No binary available for your platform.")
			os.Exit(1)
		}

		fmt.Println("Downloading update...")
		tmpPath, err := updater.DownloadUpdate(result.DownloadURL)
		if err != nil {
			fmt.Printf("Failed to download: %v\n", err)
			os.Exit(1)
		}

		fmt.Println("Installing update...")
		if err := updater.ApplyUpdate(tmpPath); err != nil {
			fmt.Printf("Failed to install: %v\n", err)
			os.Remove(tmpPath)
			os.Exit(1)
		}

		fmt.Println("✓ Update installed successfully!")
		fmt.Println("Please restart the bridge.")
	},
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("open-agents-bridge %s\n", updater.GetVersionInfo())
	},
}
