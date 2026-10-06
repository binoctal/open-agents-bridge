package protocol

// Real-CLI verification of the P0 live mode switch (openspec
// execution-model-redesign, task 3.5). Opt-in only — these tests spawn the
// real claude ACP stack (npx @agentclientprotocol/claude-agent-acp) under an
// isolated CLAUDE_CONFIG_DIR seeded from the local login and burn real LLM
// tokens:
//
//	OA_REAL_CLI_E2E=1 go test ./internal/protocol -run RealClaude -v -count=1
//
// What they prove (and the fake-agent tests cannot):
//  1. accept-all -> default switches the ENGINE in place: the engine pid is
//     unchanged and the next write tool call is gated by a real
//     session/request_permission.
//  2. A project-level .claude/settings.json defaultMode=bypassPermissions
//     cannot loosen a session started in default: the first write is still
//     gated, because the mode is delivered via session/set_mode into the
//     isolated shared config dir (settings.json "{}").

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const realPromptWait = 180 * time.Second

// waitForTimeout is waitFor with a caller-chosen deadline: the shared helper
// in acp_setmode_test.go caps at 5s, far too short for a real LLM turn. Error
// messages seen so far are dumped on timeout — auth failures and the like
// surface there when a turn silently never completes.
func waitForTimeout(t *testing.T, sink *msgSink, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s; error/status messages so far: %s", d, what, sinkErrors(sink))
}

func sinkErrors(sink *msgSink) string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var b strings.Builder
	for _, m := range sink.msgs {
		if m.Type == MessageTypeError {
			b.WriteString("ERROR: ")
			b.WriteString(fmt.Sprint(m.Content))
			b.WriteString(" | ")
		}
	}
	return b.String()
}

func realCLISkip(t *testing.T) {
	t.Helper()
	if os.Getenv("OA_REAL_CLI_E2E") != "1" {
		t.Skip("set OA_REAL_CLI_E2E=1 to run real-CLI tests (uses the local claude login and LLM tokens)")
	}
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skipf("npx not found: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("home: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", ".credentials.json")); err != nil {
		t.Skip("no local claude login (~/.claude/.credentials.json)")
	}
}

// connectRealClaude starts the real claude ACP engine with an isolated config
// dir (settings "{}", credentials copied from the local login). acpModeID is
// the ACP mode id ("default" / "bypassPermissions" / ...).
func connectRealClaude(t *testing.T, workDir, acpModeID string) (*ACPAdapter, *msgSink) {
	t.Helper()
	return connectRealClaudeShared(t, workDir, acpModeID, "")
}

// connectRealClaudeShared is connectRealClaude with an explicit config dir:
// passing the same dir to two sessions of different modes is how the bridge's
// shared-dir design keeps one login across modes.
func connectRealClaudeShared(t *testing.T, workDir, acpModeID, configDir string) (*ACPAdapter, *msgSink) {
	t.Helper()

	// The mode id doubles as the bridge permission mode for terminal gating.
	bridgeMode := "default"
	if acpModeID == "bypassPermissions" {
		bridgeMode = "accept-all"
	}

	if configDir == "" {
		configDir = t.TempDir()
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	home, _ := os.UserHomeDir()
	for _, name := range []string{".credentials.json", ".claude.json"} {
		dst := filepath.Join(configDir, name)
		if _, err := os.Stat(dst); err == nil {
			continue // shared dir already seeded by an earlier session
		}
		data, err := os.ReadFile(filepath.Join(home, ".claude", name))
		if err == nil {
			if err := os.WriteFile(dst, data, 0o600); err != nil {
				t.Fatalf("seed %s: %v", name, err)
			}
		}
	}

	// Strip this process's own Claude Code session env so the engine does not
	// think it is a nested session (empty CustomEnv value = unset).
	customEnv := map[string]string{"CLAUDE_CONFIG_DIR": configDir}
	for _, k := range []string{"CLAUDECODE", "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ID", "CLAUDE_PID", "CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_EFFORT", "AI_AGENT"} {
		customEnv[k] = ""
	}

	a := NewACPAdapter()
	sink := &msgSink{}
	a.Subscribe(sink.add)
	t.Cleanup(func() { a.Disconnect() })
	if err := a.Connect(AdapterConfig{
		Command: "npx", Args: []string{"@agentclientprotocol/claude-agent-acp"},
		WorkDir: workDir, CustomEnv: customEnv,
		PermissionMode: bridgeMode, ACPModeID: acpModeID,
	}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	waitFor(t, "session exposed", func() bool { return a.getSessionID() != "" })
	return a, sink
}

// promptAndWaitIdle sends a prompt and waits for the engine to finish the
// turn (isProcessing back to false).
func promptAndWaitIdle(t *testing.T, a *ACPAdapter, sink *msgSink, prompt string) {
	t.Helper()
	if err := a.SendMessage(Message{Type: MessageTypeContent, Content: prompt}); err != nil {
		t.Fatalf("send prompt: %v", err)
	}
	waitForTimeout(t, sink, "turn end", realPromptWait, func() bool { return !a.isProcessing.Load() })
}

// waitPermission blocks until a permission request shows up and returns it.
func waitPermission(t *testing.T, sink *msgSink) PermissionRequest {
	t.Helper()
	var got *PermissionRequest
	waitForTimeout(t, sink, "permission request", realPromptWait, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		for i := range sink.msgs {
			if sink.msgs[i].Type == MessageTypePermission {
				if req, ok := sink.msgs[i].Content.(PermissionRequest); ok {
					got = &req
					return true
				}
			}
		}
		return false
	})
	return *got
}

func sawPermission(sink *msgSink) bool {
	return sink.has(func(m Message) bool { return m.Type == MessageTypePermission })
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// deepestDescendantPid walks /proc and returns the pid at the bottom of the
// process tree rooted at root (npx -> sh -> node -> claude). A stable value
// across SetMode proves the engine was switched, not restarted.
func deepestDescendantPid(root int) int {
	children := map[int][]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return root
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// The comm field may contain spaces; ppid is field 4 after the
		// parenthesized comm — cut everything up to the last ')'.
		s := string(data)
		if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
			fields := strings.Fields(s[i+2:])
			if len(fields) >= 2 {
				ppid, err := strconv.Atoi(fields[1])
				if err == nil {
					children[ppid] = append(children[ppid], pid)
				}
			}
		}
	}
	deepest := root
	for {
		kids := children[deepest]
		if len(kids) == 0 {
			return deepest
		}
		deepest = kids[0]
	}
}

func writePrompt(name, marker string) string {
	return "Use the Write tool to create a file named " + name +
		" in the current working directory with exactly the text " + marker +
		". Do not use any other tool. When done reply with the single word " + marker + "."
}

func TestRealClaudeSetModeLiveSwitch(t *testing.T) {
	realCLISkip(t)
	workDir := t.TempDir()
	a, sink := connectRealClaude(t, workDir, "bypassPermissions")

	enginePid := deepestDescendantPid(a.cmd.Process.Pid)

	// accept-all: the write must go through with no permission round-trip.
	promptAndWaitIdle(t, a, sink, writePrompt("out1.txt", "DONE1"))
	out1 := filepath.Join(workDir, "out1.txt")
	waitForTimeout(t, sink, "out1.txt created (bypass mode)", 30*time.Second, func() bool { return fileExists(out1) })
	if sawPermission(sink) {
		t.Fatal("bypassPermissions session asked for permission")
	}

	// Live switch to default on the SAME engine.
	if err := a.SetMode("default", "default"); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	if pid := deepestDescendantPid(a.cmd.Process.Pid); pid != enginePid {
		t.Fatalf("engine pid changed across SetMode: %d -> %d (engine restarted, not switched)", enginePid, pid)
	}

	// default: the same kind of write must now be gated.
	out2 := filepath.Join(workDir, "out2.txt")
	if err := a.SendMessage(Message{Type: MessageTypeContent, Content: writePrompt("out2.txt", "DONE2")}); err != nil {
		t.Fatalf("send prompt 2: %v", err)
	}
	req := waitPermission(t, sink)
	if !strings.Contains(req.ToolName, "Write") && req.ToolInput == nil {
		t.Logf("permission request: tool=%q", req.ToolName)
	}
	if fileExists(out2) {
		t.Fatal("out2.txt exists although the write was never approved")
	}
	if err := a.SendMessage(Message{Type: MessageTypePermission, Content: PermissionResponse{ID: req.ID, OptionID: "reject_once"}}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	waitForTimeout(t, sink, "turn end after reject", realPromptWait, func() bool { return !a.isProcessing.Load() })
	if fileExists(out2) {
		t.Fatal("out2.txt created despite rejection")
	}
}

func TestRealClaudeProjectSettingsBypassStillGated(t *testing.T) {
	realCLISkip(t)
	workDir := t.TempDir()
	// The project tries to loosen the mode via its own settings; the shared
	// config dir + set_mode must win.
	if err := os.MkdirAll(filepath.Join(workDir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := map[string]interface{}{
		"permissions": map[string]interface{}{"defaultMode": "bypassPermissions"},
	}
	raw, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(workDir, ".claude", "settings.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	a, sink := connectRealClaude(t, workDir, "default")

	out := filepath.Join(workDir, "pwned.txt")
	if err := a.SendMessage(Message{Type: MessageTypeContent, Content: writePrompt("pwned.txt", "DONE3")}); err != nil {
		t.Fatalf("send prompt: %v", err)
	}
	req := waitPermission(t, sink)
	if fileExists(out) {
		t.Fatal("project settings bypassPermissions let the write through without asking")
	}
	if err := a.SendMessage(Message{Type: MessageTypePermission, Content: PermissionResponse{ID: req.ID, OptionID: "reject_once"}}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	waitForTimeout(t, sink, "turn end after reject", realPromptWait, func() bool { return !a.isProcessing.Load() })
}

// joinedContent concatenates every content chunk — streamed replies arrive
// fragmented ("ST"+"ILL"+"IN"), so per-message matching would flake.
func joinedContent(sink *msgSink) string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var b strings.Builder
	for _, m := range sink.msgs {
		if m.Type == MessageTypeContent {
			b.WriteString(fmt.Sprint(m.Content))
		}
	}
	return strings.ToLower(b.String())
}

// One shared CLAUDE_CONFIG_DIR serves every permission mode (P0: the old
// per-mode dirs split the login). A bypass session followed by a default
// session on the SAME dir must both stay authenticated.
func TestRealClaudeSharedConfigDirKeepsLogin(t *testing.T) {
	realCLISkip(t)
	shared := t.TempDir()

	first, sink1 := connectRealClaudeShared(t, t.TempDir(), "bypassPermissions", shared)
	promptAndWaitIdle(t, first, sink1, "Reply with the single word LOGINOK and nothing else.")
	if !strings.Contains(joinedContent(sink1), "loginok") {
		t.Fatalf("first (bypass) session never authenticated/replied; errors: %s", sinkErrors(sink1))
	}
	first.Disconnect()

	second, sink2 := connectRealClaudeShared(t, t.TempDir(), "default", shared)
	promptAndWaitIdle(t, second, sink2, "Reply with the single word STILLIN and nothing else.")
	if !strings.Contains(joinedContent(sink2), "stillin") {
		t.Fatalf("second (default) session on the shared dir lost the login; messages so far: %s; errors: %s", joinedContent(sink2), sinkErrors(sink2))
	}
}
