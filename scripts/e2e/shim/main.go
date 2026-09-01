// Command shim is the replay agent used by the parity e2e recipes
// (OpenSpec add-parity-e2e-verification). It is the standalone sibling of
// the test-binary shim in internal/bridge/replay_helper_test.go: same
// wire contract (OA_REPLAY_SCRIPT names a replay script; stdin/stdout are
// the ACP transport), usable outside `go test`.
//
// The script comes from OA_REPLAY_SCRIPT; a missing or unloadable script
// is a hard error with a non-zero exit — a silently idle agent would look
// like a hung task to the orchestrator, which is exactly the failure
// mode the e2e recipes exist to catch.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/binoctal/open-agents-bridge/internal/replay"
)

func main() {
	scriptPath := os.Getenv("OA_REPLAY_SCRIPT")
	if scriptPath == "" {
		fmt.Fprintln(os.Stderr, "e2e shim: OA_REPLAY_SCRIPT is not set")
		os.Exit(1)
	}
	script, err := replay.LoadScript(scriptPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e shim: %v\n", err)
		os.Exit(1)
	}
	// Blocks until the process is killed — the bridge, not the shim,
	// decides when the CLI dies (design D3 of add-replay-testing).
	if err := replay.RunPlayer(context.Background(), os.Stdin, os.Stdout, script); err != nil {
		fmt.Fprintf(os.Stderr, "e2e shim: %v\n", err)
		os.Exit(1)
	}
}
