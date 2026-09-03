package session

import (
	"testing"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/protocol"
)

// --- Constructor & accessors ----------------------------------------------

func TestNewManager_Defaults(t *testing.T) {
	m := NewManager()
	if m.MaxConcurrent() != 3 {
		t.Errorf("default maxConcurrent = %d, want 3", m.MaxConcurrent())
	}
	if m.Count() != 0 {
		t.Errorf("new manager Count = %d, want 0", m.Count())
	}
	if m.ActiveCount() != 0 {
		t.Errorf("new manager ActiveCount = %d, want 0", m.ActiveCount())
	}
}

func TestManager_SetMaxConcurrent(t *testing.T) {
	m := NewManager()
	m.SetMaxConcurrent(8)
	if m.MaxConcurrent() != 8 {
		t.Errorf("MaxConcurrent = %d, want 8", m.MaxConcurrent())
	}
}

// --- Counting & stats (direct map population, no Connect) -----------------

// putActive inserts a bare Session (nil Protocol) directly into the manager
// map, bypassing Create/Connect. Tests in the same package can reach private
// fields.
func putSession(m *Manager, id, status string) *Session {
	s := &Session{
		ID:        id,
		CLIType:   "claude",
		Status:    status,
		CreatedAt: time.Now(),
		Protocol:  nil,
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	return s
}

func TestManager_Count(t *testing.T) {
	m := NewManager()
	putSession(m, "a", "active")
	putSession(m, "b", "completed")
	if m.Count() != 2 {
		t.Errorf("Count = %d, want 2", m.Count())
	}
}

func TestManager_ActiveCount(t *testing.T) {
	m := NewManager()
	putSession(m, "a", "active")
	putSession(m, "b", "active")
	putSession(m, "c", "completed")
	putSession(m, "d", "error")
	if m.ActiveCount() != 2 {
		t.Errorf("ActiveCount = %d, want 2", m.ActiveCount())
	}
}

func TestManager_GetStats(t *testing.T) {
	m := NewManager()
	putSession(m, "a", "active")
	putSession(m, "b", "active")
	putSession(m, "c", "completed")
	putSession(m, "d", "error")
	putSession(m, "e", "replaced")

	stats := m.GetStats()
	want := map[string]int{"total": 5, "active": 2, "completed": 1, "error": 1, "replaced": 1}
	for k, v := range want {
		if stats[k] != v {
			t.Errorf("stats[%q] = %d, want %d", k, stats[k], v)
		}
	}
}

// --- Get / List -----------------------------------------------------------

func TestManager_Get(t *testing.T) {
	m := NewManager()
	putSession(m, "s1", "active")
	if got := m.Get("s1"); got == nil || got.ID != "s1" {
		t.Errorf("Get(s1) = %+v, want s1", got)
	}
	if got := m.Get("missing"); got != nil {
		t.Errorf("Get(missing) = %+v, want nil", got)
	}
}

func TestManager_List(t *testing.T) {
	m := NewManager()
	putSession(m, "a", "active")
	putSession(m, "b", "active")
	if got := m.List(); len(got) != 2 {
		t.Errorf("List len = %d, want 2", len(got))
	}
}

// --- Queue ----------------------------------------------------------------

func TestManager_EnqueueDequeue_FIFO(t *testing.T) {
	m := NewManager()
	m.Enqueue(QueueItem{SessionID: "q1", CLIType: "claude"})
	m.Enqueue(QueueItem{SessionID: "q2", CLIType: "gemini"})

	first := m.DequeueNext()
	if first == nil || first.SessionID != "q1" {
		t.Errorf("first dequeue = %+v, want q1", first)
	}
	second := m.DequeueNext()
	if second == nil || second.SessionID != "q2" {
		t.Errorf("second dequeue = %+v, want q2", second)
	}
	if got := m.DequeueNext(); got != nil {
		t.Errorf("dequeue on empty queue = %+v, want nil", got)
	}
}

func TestManager_Enqueue_StampsEnqueuedAt(t *testing.T) {
	m := NewManager()
	before := time.Now()
	m.Enqueue(QueueItem{SessionID: "q1"})
	item := m.DequeueNext()
	if item == nil {
		t.Fatal("expected enqueued item")
	}
	if item.EnqueuedAt.Before(before) {
		t.Error("EnqueuedAt was not stamped by Enqueue")
	}
}

// --- Stop / StopAll (nil Protocol => no process, safe) --------------------

func TestManager_Stop_RemovesSession(t *testing.T) {
	m := NewManager()
	putSession(m, "s1", "active")
	if err := m.Stop("s1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if m.Count() != 0 {
		t.Errorf("after Stop Count = %d, want 0", m.Count())
	}
}

func TestManager_Stop_UnknownID_NoError(t *testing.T) {
	m := NewManager()
	if err := m.Stop("nope"); err != nil {
		t.Errorf("Stop(unknown) = %v, want nil", err)
	}
}

func TestManager_StopWithExitCode_FiresExitCallback(t *testing.T) {
	m := NewManager()
	s := putSession(m, "s1", "active")
	s.JobID = "job-1"
	s.TaskID = "task-1"
	s.Output = []byte("partial output")

	type exit struct {
		id       string
		exitCode int
		output   []byte
	}
	got := make(chan exit, 1)
	m.SetExitCallback(func(id string, code int, output []byte) {
		got <- exit{id, code, output}
	})

	if err := m.StopWithExitCode("s1", 0); err != nil {
		t.Fatalf("StopWithExitCode: %v", err)
	}

	select {
	case e := <-got:
		if e.id != "s1" {
			t.Errorf("callback id = %s, want s1", e.id)
		}
		if e.exitCode != 0 {
			t.Errorf("callback exitCode = %d, want 0", e.exitCode)
		}
		if string(e.output) != "partial output" {
			t.Errorf("callback output = %q, want partial output", string(e.output))
		}
	case <-time.After(time.Second):
		t.Fatal("exit callback not fired")
	}
}

func TestManager_StopWithExitCode_NoCallback_WithoutMetadata(t *testing.T) {
	// Sessions without JobID/TaskID must not invoke the exit callback.
	m := NewManager()
	putSession(m, "s1", "active")

	called := make(chan struct{}, 1)
	m.SetExitCallback(func(string, int, []byte) { called <- struct{}{} })

	if err := m.StopWithExitCode("s1", 1); err != nil {
		t.Fatalf("StopWithExitCode: %v", err)
	}

	select {
	case <-called:
		t.Error("exit callback fired for session without multi-agent metadata")
	case <-time.After(200 * time.Millisecond):
		// expected: callback not fired
	}
}

func TestManager_StopAll(t *testing.T) {
	m := NewManager()
	putSession(m, "a", "active")
	putSession(m, "b", "active")

	ids := m.StopAll()
	if len(ids) != 2 {
		t.Errorf("StopAll returned %d ids, want 2", len(ids))
	}
	if m.Count() != 0 {
		t.Errorf("after StopAll Count = %d, want 0", m.Count())
	}
}

// --- UpdateWorkDir --------------------------------------------------------

func TestManager_UpdateWorkDir(t *testing.T) {
	m := NewManager()
	putSession(m, "s1", "active")

	if err := m.UpdateWorkDir("s1", "/new/path"); err != nil {
		t.Fatalf("UpdateWorkDir: %v", err)
	}
	if got := m.Get("s1").WorkDir; got != "/new/path" {
		t.Errorf("WorkDir = %s, want /new/path", got)
	}

	if err := m.UpdateWorkDir("missing", "/x"); err == nil {
		t.Error("UpdateWorkDir(unknown) returned nil, want error")
	}
}

// --- Fallback -------------------------------------------------------------

func TestManager_GetFallbackCLI(t *testing.T) {
	m := NewManager()
	fallbacks := []FallbackConfig{
		{CLIType: "claude", Fallback: "qwen", OnError: "rate_limit"},
		{CLIType: "qwen", Fallback: "gemini", OnError: "any"},
	}
	if got := m.GetFallbackCLI("claude", fallbacks); got != "qwen" {
		t.Errorf("fallback for claude = %s, want qwen", got)
	}
	if got := m.GetFallbackCLI("goose", fallbacks); got != "" {
		t.Errorf("fallback for goose = %s, want empty", got)
	}
}

// --- canResumeSession (pure logic; nil/disconnected protocol => false) ----

func TestManager_canResumeSession(t *testing.T) {
	m := NewManager()
	base := &Session{
		ID:        "s1",
		CLIType:   "claude",
		WorkDir:   "/proj",
		Status:    "active",
		Protocol:  nil, // nil => cannot resume
		CreatedAt: time.Now(),
	}

	cases := []struct {
		name     string
		sess     *Session
		cliType  string
		workDir  string
		wantTrue bool
	}{
		{"nil protocol", base, "claude", "/proj", false},
		{"status not active", &Session{ID: "s2", CLIType: "claude", WorkDir: "/proj", Status: "completed", Protocol: nil}, "claude", "/proj", false},
		{"cliType mismatch", base, "gemini", "/proj", false},
		{"workDir mismatch", base, "claude", "/other", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := m.canResumeSession(c.sess, c.cliType, c.workDir)
			if got != c.wantTrue {
				t.Errorf("canResumeSession = %v, want %v", got, c.wantTrue)
			}
		})
	}
}

// --- cleanupIdleSessions --------------------------------------------------

func TestManager_cleanupIdleSessions(t *testing.T) {
	m := NewManager()
	// Non-active, old => removed.
	putSession(m, "old-err", "error").CreatedAt = time.Now().Add(-48 * time.Hour)
	// Non-active, recent => kept.
	putSession(m, "new-err", "error").CreatedAt = time.Now()
	// Active, old => kept (only non-active are cleaned).
	putSession(m, "active-old", "active").CreatedAt = time.Now().Add(-48 * time.Hour)

	m.cleanupIdleSessions(24 * time.Hour)

	ids := map[string]bool{}
	for _, s := range m.List() {
		ids[s.ID] = true
	}
	if ids["old-err"] {
		t.Error("old non-active session should have been removed")
	}
	if !ids["new-err"] {
		t.Error("recent non-active session should have been kept")
	}
	if !ids["active-old"] {
		t.Error("active session should have been kept regardless of age")
	}
}

// --- Session methods (no live protocol) -----------------------------------

func TestSession_GetProtocolName_Nil(t *testing.T) {
	s := &Session{Protocol: nil}
	if got := s.GetProtocolName(); got != "none" {
		t.Errorf("GetProtocolName() = %q, want none", got)
	}
}

func TestSession_Send_NilProtocol_Error(t *testing.T) {
	s := &Session{ID: "s1", Protocol: nil}
	if err := s.Send("hello"); err == nil {
		t.Error("Send with nil protocol returned nil, want error")
	}
}

func TestSession_Resize_NilProtocol_Error(t *testing.T) {
	s := &Session{ID: "s1", Protocol: nil}
	if err := s.Resize(80, 24); err == nil {
		t.Error("Resize with nil protocol returned nil, want error")
	}
}

func TestSession_MultiAgentMetadata(t *testing.T) {
	s := &Session{ID: "s1"}
	before := time.Now()
	s.SetMultiAgentMetadata("job-9", "task-9")
	jobID, taskID, startedAt := s.GetMultiAgentMetadata()
	if jobID != "job-9" || taskID != "task-9" {
		t.Errorf("metadata = (%s,%s), want (job-9,task-9)", jobID, taskID)
	}
	if startedAt.Before(before) {
		t.Error("StartedAt not stamped by SetMultiAgentMetadata")
	}
}

// --- applyPermissionMode (pure config mutation) ---------------------------

func TestManager_applyPermissionMode(t *testing.T) {
	m := NewManager()
	cases := []struct {
		name  string
		mode  string
		cli   string
		check func(*testing.T, *protocol.AdapterConfig)
	}{
		{
			name: "claude accept-all sets env and skip flag",
			mode: "accept-all",
			cli:  "claude",
			check: func(t *testing.T, c *protocol.AdapterConfig) {
				if c.CustomEnv["CLAUDE_PERMISSION_MODE"] != "accept-all" {
					t.Errorf("CLAUDE_PERMISSION_MODE = %q", c.CustomEnv["CLAUDE_PERMISSION_MODE"])
				}
				if !contains(c.Args, "--dangerously-skip-permissions") {
					t.Errorf("Args = %v, want skip-permissions flag", c.Args)
				}
			},
		},
		{
			name: "claude-pty accept-all treated as claude",
			mode: "accept-all",
			cli:  "claude-pty",
			check: func(t *testing.T, c *protocol.AdapterConfig) {
				if c.CustomEnv["CLAUDE_PERMISSION_MODE"] != "accept-all" {
					t.Errorf("CLAUDE_PERMISSION_MODE = %q", c.CustomEnv["CLAUDE_PERMISSION_MODE"])
				}
			},
		},
		{
			name: "gemini plan sets env only",
			mode: "plan",
			cli:  "gemini",
			check: func(t *testing.T, c *protocol.AdapterConfig) {
				if c.CustomEnv["GEMINI_PERMISSION_MODE"] != "plan" {
					t.Errorf("GEMINI_PERMISSION_MODE = %q", c.CustomEnv["GEMINI_PERMISSION_MODE"])
				}
			},
		},
		{
			name: "goose accept-edits sets goose mode",
			mode: "accept-edits",
			cli:  "goose",
			check: func(t *testing.T, c *protocol.AdapterConfig) {
				if c.CustomEnv["GOOSE_MODE"] != "auto-edit" {
					t.Errorf("GOOSE_MODE = %q", c.CustomEnv["GOOSE_MODE"])
				}
			},
		},
		{
			name: "aider accept-all appends --yes",
			mode: "accept-all",
			cli:  "aider",
			check: func(t *testing.T, c *protocol.AdapterConfig) {
				if !contains(c.Args, "--yes") {
					t.Errorf("Args = %v, want --yes", c.Args)
				}
			},
		},
		{
			name: "default mode initializes CustomEnv but sets no flags",
			mode: "default",
			cli:  "claude",
			check: func(t *testing.T, c *protocol.AdapterConfig) {
				if c.CustomEnv == nil {
					t.Error("CustomEnv should be initialized")
				}
				if _, ok := c.CustomEnv["CLAUDE_PERMISSION_MODE"]; ok {
					t.Error("default mode should not set CLAUDE_PERMISSION_MODE")
				}
				if len(c.Args) != 0 {
					t.Errorf("Args = %v, want empty", c.Args)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &protocol.AdapterConfig{}
			m.applyPermissionMode(c.mode, c.cli, cfg)
			c.check(t, cfg)
		})
	}
}

func contains(slice []string, want string) bool {
	for _, s := range slice {
		if s == want {
			return true
		}
	}
	return false
}

// --- getCLICommand (cliType -> command mapping) ---------------------------

func TestManager_getCLICommand(t *testing.T) {
	m := NewManager()
	cases := []struct {
		cli     string
		wantCmd string
		wantArg string // first arg substring, "" if no args
	}{
		{"claude", "npx", "@agentclientprotocol/claude-agent-acp"},
		{"claude-pty", "claude", ""},
		{"qwen", "qwen-code", "--experimental-acp"},
		{"goose", "goose", "acp"},
		{"gemini", "gemini-cli", "--acp"},
		{"dsh", "dsh", "--profile"},
		{"kiro", "kiro", "chat"},
		{"cline", "cline", ""},
		{"codex", "codex", ""},
		{"aider", "aider", "--no-auto-commits"},
		{"weird-cli", "weird-cli", ""}, // unknown => passthrough, no args
	}
	for _, c := range cases {
		t.Run(c.cli, func(t *testing.T) {
			cmd, args, err := m.getCLICommand(c.cli)
			if err != nil {
				t.Errorf("getCLICommand(%q): %v", c.cli, err)
				return
			}
			if cmd != c.wantCmd {
				t.Errorf("command = %q, want %q", cmd, c.wantCmd)
			}
			if c.wantArg == "" {
				if len(args) != 0 {
					t.Errorf("args = %v, want none", args)
				}
			} else {
				if len(args) == 0 || !contains(args, c.wantArg) && args[0] != c.wantArg {
					// accept either the arg appearing anywhere, or first arg
					found := false
					for _, a := range args {
						if a == c.wantArg {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("args = %v, want to contain %q", args, c.wantArg)
					}
				}
			}
		})
	}
}
