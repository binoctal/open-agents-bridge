package main

import (
	"fmt"

	"github.com/open-agents/open-agents-bridge/internal/service"
	"github.com/spf13/cobra"
)

var serviceDevice string

var serviceCmd = &cobra.Command{
	Use:   "service",
	Short: "Manage the bridge as a system service",
}

var serviceInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install the bridge as a system service",
	Run: func(cmd *cobra.Command, args []string) {
		mgr := service.New()
		if err := mgr.Install(serviceDevice); err != nil {
			fmt.Printf("Failed to install: %v\n", err)
			return
		}
		fmt.Println("Service installed successfully.")
	},
}

var serviceStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the system service",
	Run: func(cmd *cobra.Command, args []string) {
		mgr := service.New()
		if err := mgr.Start(); err != nil {
			fmt.Printf("Failed to start: %v\n", err)
			return
		}
		fmt.Println("Service started.")
	},
}

var serviceStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the system service",
	Run: func(cmd *cobra.Command, args []string) {
		mgr := service.New()
		if err := mgr.Stop(); err != nil {
			fmt.Printf("Failed to stop: %v\n", err)
			return
		}
		fmt.Println("Service stopped.")
	},
}

var serviceUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Uninstall the system service",
	Run: func(cmd *cobra.Command, args []string) {
		mgr := service.New()
		if err := mgr.Uninstall(); err != nil {
			fmt.Printf("Failed to uninstall: %v\n", err)
			return
		}
		fmt.Println("Service uninstalled.")
	},
}

var serviceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show service status",
	Run: func(cmd *cobra.Command, args []string) {
		mgr := service.New()
		status, _ := mgr.Status()
		fmt.Printf("Service status: %s\n", status)
	},
}

func init() {
	serviceInstallCmd.Flags().StringVarP(&serviceDevice, "device", "d", "", "Device name (required)")
	serviceCmd.AddCommand(serviceInstallCmd)
	serviceCmd.AddCommand(serviceStartCmd)
	serviceCmd.AddCommand(serviceStopCmd)
	serviceCmd.AddCommand(serviceUninstallCmd)
	serviceCmd.AddCommand(serviceStatusCmd)
}
