package bridge

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/replay"
)

// FixtureWorkspace copies the committed replay fixture workspace into a
// temp dir and returns the copy's path. A replay run's fs/write frames hit
// the copy, never the committed fixture.
func FixtureWorkspace(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "..", "test", "fixtures", "replay", "workspace")
	dst := t.TempDir()
	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copy fixture workspace: %v", err)
	}
	return dst
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// TestReplayAgentHelper is the replay shim the G17 suite spawns as the CLI
// process. The replay tests point OA_REPLAY_SHIM at this test binary with
// OA_REPLAY_SHIM_ARGS="-test.run=TestReplayAgentHelper", and the script
// comes from OA_REPLAY_SCRIPT. In a normal `go test` run (no script env)
// it returns immediately.
//
// It NEVER returns while a script is playing: the player blocks until the
// process is killed, which is the whole point (design D3 — the bridge, not
// the shim, decides when the CLI dies). The go test framework's exit-time
// output therefore never pollutes the wire.
func TestReplayAgentHelper(t *testing.T) {
	scriptPath := os.Getenv("OA_REPLAY_SCRIPT")
	if scriptPath == "" {
		return
	}
	script, err := replay.LoadScript(scriptPath)
	if err != nil {
		t.Fatalf("replay helper: %v", err)
	}
	// The player outlives stdin EOF by design, so an interrupted or timed-out
	// test run used to leak one helper per session forever. Cap the leak: exit
	// shortly after the bridge side of the pipe closes, and never outlive a
	// hard ceiling even if the pipe somehow stays open.
	time.AfterFunc(helperMaxLifetime, func() { os.Exit(0) })
	in := &eofExitReader{r: os.Stdin}
	// Blocks until the process is killed; the returned error is unreachable
	// in practice (context.Background is never cancelled).
	if err := replay.RunPlayer(context.Background(), in, os.Stdout, script); err != nil {
		t.Fatalf("replay helper: %v", err)
	}
}

const (
	helperMaxLifetime = 15 * time.Minute
	// Long enough for a test that closes stdin and then still expects the
	// agent's trailing frames, short enough that orphans clear quickly.
	helperEOFGrace = 30 * time.Second
)

// eofExitReader schedules process exit once the wrapped reader hits EOF.
type eofExitReader struct{ r io.Reader }

func (e *eofExitReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		time.AfterFunc(helperEOFGrace, func() { os.Exit(0) })
	}
	return n, err
}
