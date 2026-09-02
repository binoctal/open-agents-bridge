// Command shim is the replay agent used by the parity e2e recipes
// (OpenSpec add-parity-e2e-verification). It is the standalone sibling of
// the test-binary shim in internal/bridge/replay_helper_test.go: same
// wire contract (OA_REPLAY_SCRIPT names a replay script; stdin/stdout are
// the ACP transport), usable outside `go test`.
//
// Script selection, in one of two modes:
//
//   - OA_REPLAY_SCRIPT=<path>: every invocation replays the same script.
//   - OA_REPLAY_SCRIPT_SEQ=<p1>,<p2>,... plus OA_REPLAY_STATE=<file>: the
//     Nth invocation of the shim (counted via the state file, flock-guarded)
//     replays the (N-1)%len(list) path. The bridge WAITS for the initialize
//     response before sending session/prompt, so a shim cannot pick its
//     script by prompt content — it would deadlock. Session order is the
//     only signal available, and for the parity recipes it is exactly the
//     right one: dependencies force the upstream task's session to start
//     before the downstream's. The modulo wrap keeps parallel scenarios
//     (rule 6) working when the two sessions race.
//
// A missing or unloadable script is a hard error with a non-zero exit — a
// silently idle agent would look like a hung task to the orchestrator,
// which is exactly the failure mode the e2e recipes exist to catch.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/binoctal/open-agents-bridge/internal/replay"
)

func main() {
	scriptPath, err := resolveScript()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e shim: %v\n", err)
		os.Exit(1)
	}
	script, err := replay.LoadScript(scriptPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e shim: %v\n", err)
		os.Exit(1)
	}
	// File side effects first: the bridge launches this process with cwd =
	// the task's worktree, so a script's write entries land in that
	// worktree and CommitAll/PushBranch have real content to work with.
	if err := applyWrites(script.Writes); err != nil {
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

// applyWrites writes each script-declared file relative to the current
// working directory. Paths are kept inside the cwd: a script escaping its
// worktree would corrupt the shared repo the e2e drives.
func applyWrites(writes []replay.Write) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	for _, w := range writes {
		path := filepath.Clean(w.Path)
		if filepath.IsAbs(path) || strings.HasPrefix(path, "..") {
			return fmt.Errorf("write entry %q must be a relative path inside the working directory", w.Path)
		}
		full := filepath.Join(cwd, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("write %s: %w", w.Path, err)
		}
		if err := os.WriteFile(full, []byte(w.Content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", w.Path, err)
		}
	}
	return nil
}

// resolveScript picks the script path for this invocation according to the
// env-var contract above. SEQ mode atomically claims an index from the
// state file so concurrent invocations (parallel tasks) never take the
// same slot silently.
func resolveScript() (string, error) {
	seq := os.Getenv("OA_REPLAY_SCRIPT_SEQ")
	if seq == "" {
		p := os.Getenv("OA_REPLAY_SCRIPT")
		if p == "" {
			return "", errors.New("OA_REPLAY_SCRIPT is not set")
		}
		return p, nil
	}

	var scripts []string
	for _, part := range strings.Split(seq, ",") {
		if p := strings.TrimSpace(part); p != "" {
			scripts = append(scripts, p)
		}
	}
	if len(scripts) == 0 {
		return "", errors.New("OA_REPLAY_SCRIPT_SEQ is set but names no scripts")
	}

	state := os.Getenv("OA_REPLAY_STATE")
	if state == "" {
		return "", errors.New("OA_REPLAY_SCRIPT_SEQ requires OA_REPLAY_STATE to name the counter file")
	}

	idx, err := claimNextIndex(state)
	if err != nil {
		return "", fmt.Errorf("seq state %s: %w", state, err)
	}
	return scripts[idx%len(scripts)], nil
}

// claimNextIndex atomically reads-and-increments the counter file. The
// driver creates the file (content "0") before starting the bridge; here it
// must already exist — creating it on the fly would let a misconfigured run
// silently restart the sequence mid-mission.
func claimNextIndex(path string) (int, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return 0, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck

	buf := make([]byte, 32)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return 0, err
	}
	cur, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		return 0, fmt.Errorf("parse counter: %w", err)
	}
	if cur < 0 {
		return 0, fmt.Errorf("counter is negative: %d", cur)
	}

	if err := f.Truncate(0); err != nil {
		return 0, err
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(cur+1)), 0); err != nil {
		return 0, err
	}
	return cur, nil
}
