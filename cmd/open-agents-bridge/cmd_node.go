package main

import (
	"os"
	"strings"

	"github.com/binoctal/open-agents-bridge/node"
	"github.com/binoctal/open-agents-bridge/node/register"
	"github.com/spf13/cobra"
)

// The `node` subcommand family carries the node runtime that used to ship as
// the standalone hosting-agent binary (hosting-agent-into-bridge): `register`
// is the one-time BYO handshake, `run` is the daemon (user-node form with
// credentials, platform form without). The process model is unchanged -- a
// node daemon is independent of any bridge daemon on the same machine.

var nodeRegisterCode string
var nodeRegisterAPI string
var nodeRegisterOut string
var nodeRunCredentials string

var nodeCmd = &cobra.Command{
	Use:   "node",
	Short: "Run this machine as a deploy node",
	Long: `Register and run this machine as a deploy node.

"node register" performs the one-time registration handshake with an install
code from the dashboard "my nodes" page; "node run" starts the node daemon.
With node credentials present the daemon is a user-owned BYO node (agent-driven
docker compose, per-label HTTPS via Caddy); without them it reads the platform
node configuration from the environment.`,
}

var nodeRegisterCmd = &cobra.Command{
	Use:   "register",
	Short: "Register this machine as a deploy node (one-time install code)",
	RunE: func(cmd *cobra.Command, args []string) error {
		out := nodeRegisterOut
		if out == "" {
			out = os.Getenv("HOSTING_NODE_CREDENTIALS")
		}
		if out == "" {
			out = "/etc/hosting-agent/node.json"
		}
		return register.Run(cmd.Context(), register.Options{
			Code:           nodeRegisterCode,
			APIBaseURL:     strings.TrimRight(nodeRegisterAPI, "/"),
			CredentialsOut: out,
		})
	},
}

var nodeRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the node daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		return node.RunDaemon(nodeRunCredentials)
	},
}

func init() {
	nodeRegisterCmd.Flags().StringVar(&nodeRegisterCode, "code", "", "one-time install code from the web \"my nodes\" page")
	nodeRegisterCmd.Flags().StringVar(&nodeRegisterAPI, "api", "", "platform API origin, e.g. https://api.openagents.top")
	nodeRegisterCmd.Flags().StringVar(&nodeRegisterOut, "out", "", "credentials file to write (default $HOSTING_NODE_CREDENTIALS or /etc/hosting-agent/node.json)")
	nodeRunCmd.Flags().StringVar(&nodeRunCredentials, "node-credentials", "", "BYO node credentials file (user-node mode)")

	nodeCmd.AddCommand(nodeRegisterCmd)
	nodeCmd.AddCommand(nodeRunCmd)
}
