package bridge

import (
	"errors"
	"testing"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/config"
	"github.com/binoctal/open-agents-bridge/internal/protocol"
	"github.com/binoctal/open-agents-bridge/internal/session"
)

// sendErrorAdapter behaves like the connected fakeAdapter except SendMessage
// always fails — the prompt-send-failure shape.
type sendErrorAdapter struct{ fakeAdapter }

func (f *sendErrorAdapter) SendMessage(protocol.Message) error {
	return errors.New("pipe closed")
}

func newWatchdogBridge(t *testing.T) *Bridge {
	t.Helper()
	return &Bridge{
		config:         &config.Config{DeviceID: "device-wd"},
		sessions:       session.NewManager(),
		statusTrackers: make(map[string]*statusTracker),
		taskMeta:       make(map[string]*taskMeta),
		taskWatchdogs:  make(map[string]*time.Timer),
	}
}

func adoptTaskSession(b *Bridge, id string, a protocol.Adapter) *session.Session {
	sess := &session.Session{
		ID:        id,
		CLIType:   "claude",
		WorkDir:   ".",
		Status:    "active",
		CreatedAt: time.Now(),
		Protocol:  protocol.NewManagerWithAdapter(a),
	}
	sess.SetMultiAgentMetadata("job-wd", id)
	b.sessions.Adopt(sess)
	return sess
}

// A prompt that went out but produced zero protocol activity must fail the
// task within the watchdog window — stop with a non-zero exit so the exit
// callback reports task_error (2026-09-22 prod: mission2 t1 idled 9+ min
// with the CLI process alive and the mission never finished).
func TestTaskWatchdog_FiresAndStopsSession(t *testing.T) {
	b := newWatchdogBridge(t)
	b.taskIdleTimeout = 50 * time.Millisecond
	adoptTaskSession(b, "task-idle", &fakeAdapter{connected: true})
	b.taskMeta["task-idle"] = &taskMeta{JobID: "job-wd", TaskID: "task-idle", Attempt: 2}

	exit := make(chan int, 1)
	b.sessions.SetExitCallback(func(_ string, code int, _ []byte) { exit <- code })

	b.armTaskWatchdog("task-idle")

	select {
	case code := <-exit:
		if code != 1 {
			t.Errorf("watchdog stop exitCode = %d, want 1 (non-zero so the result reports as error)", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog fired but session was never stopped")
	}
	if got := b.sessions.Get("task-idle"); got != nil {
		t.Error("watchdog stopped the session but it is still registered")
	}
	b.taskWatchdogMu.Lock()
	remaining := len(b.taskWatchdogs)
	b.taskWatchdogMu.Unlock()
	if remaining != 0 {
		t.Errorf("watchdog map has %d entries after firing, want 0", remaining)
	}
}

// Continuous protocol activity pushes the window forward: a slow but alive
// turn must never be reaped.
func TestTaskWatchdog_ResetDefersFiring(t *testing.T) {
	b := newWatchdogBridge(t)
	b.taskIdleTimeout = 200 * time.Millisecond
	adoptTaskSession(b, "task-slow", &fakeAdapter{connected: true})

	b.sessions.SetExitCallback(func(_ string, code int, _ []byte) {
		t.Errorf("watchdog fired (exit %d) despite continuous activity", code)
	})

	b.armTaskWatchdog("task-slow")
	deadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		b.resetTaskWatchdog("task-slow") // activity arrives continuously
		time.Sleep(50 * time.Millisecond)
	}
	b.cancelTaskWatchdog("task-slow")
	if b.sessions.Get("task-slow") == nil {
		t.Fatal("session was stopped while actively producing output")
	}
}

func TestTaskWatchdog_CancelPreventsFiring(t *testing.T) {
	b := newWatchdogBridge(t)
	b.taskIdleTimeout = 50 * time.Millisecond
	adoptTaskSession(b, "task-done", &fakeAdapter{connected: true})

	fired := make(chan int, 1)
	b.sessions.SetExitCallback(func(_ string, code int, _ []byte) { fired <- code })

	b.armTaskWatchdog("task-done")
	b.cancelTaskWatchdog("task-done") // the turn ended normally first

	select {
	case code := <-fired:
		t.Errorf("cancelled watchdog still fired (exit %d)", code)
	case <-time.After(300 * time.Millisecond):
		// expected: no fire after cancel
	}
}

func TestTaskWatchdog_ZeroTimeoutDisabled(t *testing.T) {
	b := newWatchdogBridge(t)
	b.taskIdleTimeout = 0 // tests opt out
	b.armTaskWatchdog("task-x")

	b.taskWatchdogMu.Lock()
	_, armed := b.taskWatchdogs["task-x"]
	b.taskWatchdogMu.Unlock()
	if armed {
		t.Error("armTaskWatchdog armed a timer with a zero timeout")
	}
}

// A prompt that never reached the CLI must surface as task_error within
// seconds: stop the session with a non-zero exit so the exit-callback path
// reports failure and the orchestrator re-dispatches — instead of parking
// the task until the 30-minute execution timeout.
func TestLaunchTaskSession_FailedPromptStopsSession(t *testing.T) {
	b := newWatchdogBridge(t)
	// Pre-adopted resumable session whose SendMessage always errors:
	// CreateWithIDAndSize takes the resume path and returns it as-is.
	adoptTaskSession(b, "task-sendfail", &sendErrorAdapter{fakeAdapter{connected: true}})

	exit := make(chan int, 1)
	b.sessions.SetExitCallback(func(_ string, code int, _ []byte) { exit <- code })

	b.launchTaskSession("job-wd", "task-sendfail", "claude", ".", 120, 30, "accept-edits", "do the thing", "title", 3)

	select {
	case code := <-exit:
		if code != 1 {
			t.Errorf("failed-prompt stop exitCode = %d, want 1", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("prompt send failed but the session was never stopped")
	}
	if b.sessions.Get("task-sendfail") != nil {
		t.Error("session with undeliverable prompt still registered")
	}
	b.taskWatchdogMu.Lock()
	_, armed := b.taskWatchdogs["task-sendfail"]
	b.taskWatchdogMu.Unlock()
	if armed {
		t.Error("watchdog armed for a session whose prompt never went out")
	}
}
