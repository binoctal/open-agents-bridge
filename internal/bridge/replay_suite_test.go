package bridge

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/replay"
)

// The G17 replay assertion suite. Each test drives a REAL Bridge — WS dial
// to the sink, task dispatch, ACP session over a spawned shim process,
// turn end, exit callback — and asserts the lifecycle contract against the
// merged uplink sequence. Nothing here skips: no provider key, no network,
// no claude binary is involved (the "agent" is a committed script played by
// this test binary's helper process).

// isTaskEvent matches an uplink event about the given task. workflow:*
// payloads carry taskId at the top level of the payload object.
func isTaskEvent(ev sinkEvent, typ, taskID string) bool {
	if ev.Type != typ {
		return false
	}
	var p struct {
		TaskID string `json:"taskId"`
	}
	return json.Unmarshal(ev.Payload, &p) == nil && p.TaskID == taskID
}

func isTaskProgressZero(ev sinkEvent, taskID string) bool {
	if !isTaskEvent(ev, "workflow:task_progress", taskID) {
		return false
	}
	var p struct {
		Progress *float64 `json:"progress"`
	}
	if json.Unmarshal(ev.Payload, &p) != nil || p.Progress == nil {
		return false
	}
	return *p.Progress == 0
}

func isTerminalCallback(ev sinkEvent, taskID string) bool {
	if ev.Channel != sinkChannelCallback {
		return false
	}
	return isTaskEvent(ev, "workflow:task_result", taskID) || isTaskEvent(ev, "workflow:task_error", taskID)
}

// countMatching counts events matching pred in the current snapshot.
func (s *replaySink) countMatching(pred func(sinkEvent) bool) int {
	n := 0
	for _, ev := range s.snapshot() {
		if pred(ev) {
			n++
		}
	}
	return n
}

// assertExactlyOneTerminal waits for the task's terminal callback, asserts
// there is exactly one (result XOR error, never both, never duplicates),
// and returns it. The grace window after the first terminal catches
// double-reporting regressions that a single waitFor would miss.
func (s *replaySink) assertExactlyOneTerminal(t *testing.T, taskID string) sinkEvent {
	t.Helper()
	first := s.waitFor(20*time.Second, "terminal callback (workflow:task_result xor workflow:task_error) for task "+taskID,
		func(ev sinkEvent) bool { return isTerminalCallback(ev, taskID) })

	time.Sleep(500 * time.Millisecond)
	if n := s.countMatching(func(ev sinkEvent) bool { return isTerminalCallback(ev, taskID) }); n != 1 {
		t.Fatalf("task %s: expected exactly 1 terminal callback, got %d", taskID, n)
	}
	results := s.countMatching(func(ev sinkEvent) bool { return isTaskEvent(ev, "workflow:task_result", taskID) })
	errors := s.countMatching(func(ev sinkEvent) bool { return isTaskEvent(ev, "workflow:task_error", taskID) })
	if results > 0 && errors > 0 {
		t.Fatalf("task %s: got both task_result (%d) and task_error (%d) — terminal report must be one or the other", taskID, results, errors)
	}
	return first
}

// assertStartOrdering checks the WS-side precondition ordering: task_started
// before the progress-0 report, both before any terminal callback.
func assertStartOrdering(t *testing.T, started, progress, terminal sinkEvent) {
	t.Helper()
	if !(started.Seq < progress.Seq) {
		t.Errorf("task_started (seq %d) must precede task_progress(0) (seq %d)", started.Seq, progress.Seq)
	}
	if !(progress.Seq < terminal.Seq) {
		t.Errorf("task_progress(0) (seq %d) must precede the terminal callback (seq %d)", progress.Seq, terminal.Seq)
	}
}

// TestReplayTaskReachesTerminalState is the core G17 scenario: a task
// dispatched to the bridge must reach a terminal state and report it via
// the HTTP callback exactly once. The fixture's turn ends with
// stopReason=end_turn, so the success path must produce workflow:task_result.
func TestReplayTaskReachesTerminalState(t *testing.T) {
	sink := newReplaySink(t)
	startReplayBridge(t, sink, fixtureScript(t, "success.script.jsonl"), 3)

	sink.sendTaskAssign("job-success", "task-success", "replay", 1)

	started := sink.waitFor(20*time.Second, "WS workflow:task_started for task-success",
		func(ev sinkEvent) bool {
			return ev.Channel == sinkChannelWS && isTaskEvent(ev, "workflow:task_started", "task-success")
		})
	progress := sink.waitFor(20*time.Second, "WS workflow:task_progress(0) for task-success",
		func(ev sinkEvent) bool { return ev.Channel == sinkChannelWS && isTaskProgressZero(ev, "task-success") })
	terminal := sink.assertExactlyOneTerminal(t, "task-success")

	assertStartOrdering(t, started, progress, terminal)

	if terminal.Type != "workflow:task_result" {
		t.Fatalf("end_turn turn must report workflow:task_result, got %s (%s)", terminal.Type, terminal.Payload)
	}
	if got := payloadString(t, terminal, "jobId"); got != "job-success" {
		t.Errorf("task_result jobId = %q, want job-success", got)
	}
	if got := payloadString(t, terminal, "missionId"); got != "job-success" {
		t.Errorf("task_result missionId = %q, want job-success", got)
	}
	if got := payloadString(t, terminal, "taskId"); got != "task-success" {
		t.Errorf("task_result taskId = %q, want task-success", got)
	}
	if got := payloadString(t, terminal, "summary"); got == "" {
		t.Error("task_result summary is empty — the fixture's agent_message_chunk output should survive into the report")
	}
	// G19: the terminal callback must echo the dispatch generation verbatim
	// (task_assign attempt=1). A bridge that self-counts sessions would send 0.
	if got := payloadInt(t, terminal, "attempt"); got != 1 {
		t.Errorf("task_result attempt = %d, want 1 (echo of the task_assign payload)", got)
	}
}

// TestReplayTaskFailureReportsTaskError drives the failure fixture: the
// turn is cut off (stopReason=max_tokens), which taskTurnExitCode maps to
// a nonzero exit code — the report must be workflow:task_error, still
// exactly once.
func TestReplayTaskFailureReportsTaskError(t *testing.T) {
	sink := newReplaySink(t)
	startReplayBridge(t, sink, fixtureScript(t, "failure.script.jsonl"), 3)

	sink.sendTaskAssign("job-failure", "task-failure", "replay", 1)

	terminal := sink.assertExactlyOneTerminal(t, "task-failure")
	if terminal.Type != "workflow:task_error" {
		t.Fatalf("max_tokens turn must report workflow:task_error, got %s (%s)", terminal.Type, terminal.Payload)
	}
	if got := payloadString(t, terminal, "taskId"); got != "task-failure" {
		t.Errorf("task_error taskId = %q, want task-failure", got)
	}
	if got := payloadInt(t, terminal, "attempt"); got != 1 {
		t.Errorf("task_error attempt = %d, want 1 (echo of the task_assign payload)", got)
	}
}

// TestReplayQueuedTaskDrains exercises the pool-full queue: with
// MaxConcurrent=1, the second task must be enqueued (no task_started while
// the first holds the pool), and freeing the slot must drain it — each task
// reaching its own exactly-once terminal report. The hang fixture's turn
// never ends on its own, so "first task still holds the pool" is
// deterministic, not a race.
func TestReplayQueuedTaskDrains(t *testing.T) {
	sink := newReplaySink(t)
	b := startReplayBridge(t, sink, fixtureScript(t, "hang.script.jsonl"), 1)

	sink.sendTaskAssign("job-queued", "task-first", "replay", 1)
	sink.waitFor(20*time.Second, "WS workflow:task_started for task-first",
		func(ev sinkEvent) bool {
			return ev.Channel == sinkChannelWS && isTaskEvent(ev, "workflow:task_started", "task-first")
		})

	// Pool is full and will stay full (the fixture's turn never ends).
	sink.sendTaskAssign("job-queued", "task-second", "replay", 4)
	time.Sleep(500 * time.Millisecond)
	if n := sink.countMatching(func(ev sinkEvent) bool {
		return ev.Channel == sinkChannelWS && isTaskEvent(ev, "workflow:task_started", "task-second")
	}); n != 0 {
		t.Fatalf("task-second started with the pool full (MaxConcurrent=1, task-first active) — the queue path was bypassed")
	}

	// Free the slot exactly as the turn-end path does in production: the
	// session manager's stop fires capacityCallback -> drainTaskQueue.
	if err := b.sessions.Stop("task-first"); err != nil {
		t.Fatalf("stop task-first: %v", err)
	}
	terminalFirst := sink.assertExactlyOneTerminal(t, "task-first")

	sink.waitFor(20*time.Second, "WS workflow:task_started for drained task-second",
		func(ev sinkEvent) bool {
			return ev.Channel == sinkChannelWS && isTaskEvent(ev, "workflow:task_started", "task-second")
		})

	// The drained task uses the same hang fixture; end its turn the same way
	// so it also reaches its terminal report.
	if err := b.sessions.Stop("task-second"); err != nil {
		t.Fatalf("stop task-second: %v", err)
	}
	terminalSecond := sink.assertExactlyOneTerminal(t, "task-second")

	if terminalFirst.Type != "workflow:task_result" {
		t.Errorf("task-first (exit via manager stop, code 0) = %s, want workflow:task_result", terminalFirst.Type)
	}
	if !(terminalFirst.Seq < terminalSecond.Seq) {
		t.Errorf("task-second terminal (seq %d) must follow task-first terminal (seq %d)", terminalSecond.Seq, terminalFirst.Seq)
	}
	// G19 through the QUEUE path: task-second was enqueued (not started) with
	// attempt=4 — a deliberately non-consecutive generation, so neither a
	// dropped field (0) nor a session self-counter (1) can pass by accident.
	if got := payloadInt(t, terminalSecond, "attempt"); got != 4 {
		t.Errorf("drained task-second attempt = %d, want 4 (echoed through the queue)", got)
	}
}

// TestReplayGoldenSequenceMatches is the script-rot guard: replaying the
// committed success fixture must reproduce the committed golden uplink
// sequence. The comparison window is workflow:* events (heartbeats,
// session:status and session:usage are transport noise the window exempts)
// and unstable fields (timestamps) are normalized in the helper.
func TestReplayGoldenSequenceMatches(t *testing.T) {
	sink := newReplaySink(t)
	startReplayBridge(t, sink, fixtureScript(t, "success.script.jsonl"), 3)

	sink.sendTaskAssign("job-success", "task-success", "replay", 1)
	sink.assertExactlyOneTerminal(t, "task-success")

	goldenPath := fixtureScript(t, "success.golden.jsonl")
	want, err := replay.LoadGolden(goldenPath)
	if err != nil {
		t.Fatalf("LoadGolden: %v", err)
	}

	got := normalizeWorkflowWindow(sink.snapshot())
	wantNorm := goldenWorkflowWindow(t, want)

	if len(got) != len(wantNorm) {
		t.Fatalf("workflow:* sequence length = %d, want %d\n got: %s\nwant: %s",
			len(got), len(wantNorm), renderGolden(got), renderGolden(wantNorm))
	}
	for i := range got {
		if got[i].Channel != wantNorm[i].Channel || got[i].Type != wantNorm[i].Type || string(got[i].Payload) != string(wantNorm[i].Payload) {
			t.Fatalf("workflow:* event %d mismatch\n got: %s %s %s\nwant: %s %s %s\nfull got: %s\nfull want: %s",
				i, got[i].Channel, got[i].Type, got[i].Payload,
				wantNorm[i].Channel, wantNorm[i].Type, wantNorm[i].Payload,
				renderGolden(got), renderGolden(wantNorm))
		}
	}
}

// goldenEvent is the normalized comparison unit for the golden diff.
type goldenEvent struct {
	Channel string
	Type    string
	Payload []byte
}

// normalizeWorkflowWindow windows the live merged sequence to workflow:*
// events and strips unstable fields (timestamps). This is the single
// exemption list — anything else the bridge starts emitting shows up as a
// diff, which is the point.
func normalizeWorkflowWindow(events []sinkEvent) []goldenEvent {
	out := make([]goldenEvent, 0, len(events))
	for _, ev := range events {
		if ev.Type != "workflow:task_started" &&
			ev.Type != "workflow:task_progress" &&
			ev.Type != "workflow:task_result" &&
			ev.Type != "workflow:task_error" {
			continue
		}
		out = append(out, goldenEvent{ev.Channel, ev.Type, normalizePayload(ev.Payload)})
	}
	return out
}

// goldenWorkflowWindow applies the same window+normalization to a loaded
// golden file so both sides of the diff pass through one code path.
func goldenWorkflowWindow(t *testing.T, g *replay.Golden) []goldenEvent {
	t.Helper()
	out := make([]goldenEvent, 0, len(g.Events))
	for _, ev := range g.Events {
		if ev.Type != "workflow:task_started" &&
			ev.Type != "workflow:task_progress" &&
			ev.Type != "workflow:task_result" &&
			ev.Type != "workflow:task_error" {
			continue
		}
		out = append(out, goldenEvent{ev.Channel, ev.Type, normalizePayload(ev.Payload)})
	}
	return out
}

// normalizePayload strips fields whose values are not stable across
// recordings (timestamps). Field names are listed explicitly; nothing is
// dropped silently.
func normalizePayload(raw []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	delete(m, "timestamp")
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

func renderGolden(events []goldenEvent) string {
	out := "["
	for i, ev := range events {
		if i > 0 {
			out += "\n "
		}
		out += "\n  {" + ev.Channel + " " + ev.Type + " " + string(ev.Payload) + "}"
	}
	return out + "\n]"
}

// TestReplayHandshakeReportArrivesAndMismatchSurvives is the rule-8 slice:
// the capability report must arrive right after the WS dial (it is the
// whole point of the registration handshake — the server cannot pair what
// it never receives), and the server's mismatch verdict must not take the
// bridge down: a degraded device that stops processing work would turn a
// loud failure back into the silent one this change exists to prevent.
func TestReplayHandshakeReportArrivesAndMismatchSurvives(t *testing.T) {
	sink := newReplaySink(t)
	startReplayBridge(t, sink, fixtureScript(t, "success.script.jsonl"), 1)

	report := sink.waitFor(20*time.Second, "WS bridge:capability_report after connect",
		func(ev sinkEvent) bool {
			return ev.Channel == sinkChannelWS && ev.Type == "bridge:capability_report"
		})

	// The sink's catch-all answers the probe route with 200, so the report
	// must carry a healthy probe conclusion and the build's version.
	if got := payloadString(t, report, "version"); got == "" {
		t.Error("capability_report version is empty — updater.Version must reach the wire")
	}
	if got := payloadString(t, report, "callbackProbe"); got != probeOK {
		t.Errorf("capability_report callbackProbe = %q, want %q (sink answers 200)", got, probeOK)
	}

	// The verdict arrives; the bridge must keep living with it. Liveness is
	// proven the only way that cannot lie: dispatch real work afterwards
	// and watch the lifecycle start.
	sink.sendHandshakeMismatch()
	sink.sendTaskAssign("job-hs", "task-hs", "replay", 1)
	sink.waitFor(20*time.Second, "WS workflow:task_started for task-hs after a mismatch verdict",
		func(ev sinkEvent) bool {
			return ev.Channel == sinkChannelWS && isTaskEvent(ev, "workflow:task_started", "task-hs")
		})
	terminal := sink.assertExactlyOneTerminal(t, "task-hs")
	if terminal.Type != "workflow:task_result" {
		t.Fatalf("post-mismatch task must still complete, got %s", terminal.Type)
	}
}

// TestReplayQuestionAnswerFlow is the parity rule 5 slice: an agent that
// asks a [QUESTION] mid-task must pause, not finish — the turn that carries
// the question ends (end_turn) while the question is pending, and that turn
// boundary must NOT report the half-done task as completed. The answer is
// injected as the next prompt, and only the FOLLOWING end_turn produces the
// terminal callback. Before the guard this exact fixture reported
// workflow:task_result the moment the asking turn ended.
func TestReplayQuestionAnswerFlow(t *testing.T) {
	sink := newReplaySink(t)
	startReplayBridge(t, sink, fixtureScript(t, "e2e-question.script.jsonl"), 1)

	sink.sendTaskAssign("job-q", "task-q", "replay", 1)

	question := sink.waitFor(20*time.Second, "WS workflow:task_question for task-q",
		func(ev sinkEvent) bool {
			return ev.Channel == sinkChannelWS && isTaskEvent(ev, "workflow:task_question", "task-q")
		})
	var q struct {
		Question string `json:"question"`
	}
	if err := json.Unmarshal(question.Payload, &q); err != nil || !strings.Contains(q.Question, "Redis") {
		t.Fatalf("task_question payload must carry the fixture question, got %s (%v)", question.Payload, err)
	}

	// The asking turn ends while the question is pending: wait for the idle
	// status that end_turn produces, then prove no terminal callback fires.
	sink.waitFor(20*time.Second, "WS agent:status idle after the asking turn",
		func(ev sinkEvent) bool {
			if ev.Channel != sinkChannelWS || ev.Type != "agent:status" {
				return false
			}
			var p struct {
				Status string `json:"status"`
			}
			return json.Unmarshal(ev.Payload, &p) == nil && p.Status == "idle"
		})
	time.Sleep(500 * time.Millisecond)
	if n := sink.countMatching(func(ev sinkEvent) bool { return isTerminalCallback(ev, "task-q") }); n != 0 {
		t.Fatalf("turn ended on a pending question: %d terminal callback(s) fired — the task is blocked on a human answer, not finished", n)
	}

	sink.sendTaskAnswer("task-q", "Use an in-memory LRU cache.")

	terminal := sink.assertExactlyOneTerminal(t, "task-q")
	if terminal.Type != "workflow:task_result" {
		t.Fatalf("answered question must let the task complete, got %s (%s)", terminal.Type, terminal.Payload)
	}
}

// waitForTaskReady blocks until the session for taskID exists AND its
// multi-agent metadata (jobID/taskID) is set. The WS start signals
// (task_started, progress-0) are emitted BEFORE SetMultiAgentMetadata, so a
// test that stops the session on task_started races the metadata write — and
// StopWithExitCode skips the exit callback for a session with no task
// metadata, silently dropping the terminal report. Polling the metadata is
// the deterministic gate.
func waitForTaskReady(t *testing.T, b *Bridge, taskID string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if sess := b.sessions.Get(taskID); sess != nil {
			if jobID, tid, _ := sess.GetMultiAgentMetadata(); jobID != "" && tid != "" {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("session %s never reported multi-agent metadata", taskID)
}

// TestReplayAttemptBindingAcrossRedispatch is the G19 slice: the same task
// dispatched twice — attempt 0 first, then re-dispatched as attempt 1 after
// its first terminal — and each terminal callback must carry ITS dispatch's
// generation, never the other's. The hang fixture keeps both sessions
// deterministic: the turn never ends on its own, so each round is ended
// explicitly — failure (nonzero exit) for the first generation, success (exit
// 0) for the second — matching the real-world sequence the guard exists for
// (a failed attempt retried and succeeding). A bridge that reports the
// "current" generation instead of the starting one, or mixes the two rounds'
// metadata, fails here even though the API-side guard would catch it later —
// the two ends assert independently by design.
func TestReplayAttemptBindingAcrossRedispatch(t *testing.T) {
	sink := newReplaySink(t)
	b := startReplayBridge(t, sink, fixtureScript(t, "hang.script.jsonl"), 1)

	// Generation 0: dispatch, then fail the run with a nonzero exit.
	sink.sendTaskAssign("job-redispatch", "task-redispatch", "replay", 0)
	sink.waitFor(20*time.Second, "WS workflow:task_started for task-redispatch (attempt 0)",
		func(ev sinkEvent) bool {
			return ev.Channel == sinkChannelWS && isTaskEvent(ev, "workflow:task_started", "task-redispatch")
		})
	waitForTaskReady(t, b, "task-redispatch")
	if err := b.sessions.StopWithExitCode("task-redispatch", 1); err != nil {
		t.Fatalf("stop task-redispatch (attempt 0): %v", err)
	}

	first := sink.waitFor(20*time.Second, "terminal callback for task-redispatch (attempt 0)",
		func(ev sinkEvent) bool { return isTerminalCallback(ev, "task-redispatch") })
	if first.Type != "workflow:task_error" {
		t.Fatalf("attempt 0 (nonzero exit) must report workflow:task_error, got %s (%s)", first.Type, first.Payload)
	}
	if got := payloadInt(t, first, "attempt"); got != 0 {
		t.Fatalf("attempt-0 terminal carries attempt = %d, want 0", got)
	}

	// Generation 1: re-dispatch the SAME task after its terminal, then let it
	// succeed.
	sink.sendTaskAssign("job-redispatch", "task-redispatch", "replay", 1)
	sink.waitFor(20*time.Second, "WS workflow:task_started for task-redispatch (attempt 1)",
		func(ev sinkEvent) bool {
			return ev.Channel == sinkChannelWS && isTaskEvent(ev, "workflow:task_started", "task-redispatch")
		})
	waitForTaskReady(t, b, "task-redispatch")
	if err := b.sessions.StopWithExitCode("task-redispatch", 0); err != nil {
		t.Fatalf("stop task-redispatch (attempt 1): %v", err)
	}

	second := sink.waitFor(20*time.Second, "second terminal callback for task-redispatch (attempt 1)",
		func(ev sinkEvent) bool { return isTerminalCallback(ev, "task-redispatch") && ev.Seq > first.Seq })
	if second.Type != "workflow:task_result" {
		t.Fatalf("attempt 1 (exit 0) must report workflow:task_result, got %s (%s)", second.Type, second.Payload)
	}
	if got := payloadInt(t, second, "attempt"); got != 1 {
		t.Fatalf("attempt-1 terminal carries attempt = %d, want 1 — generations crossed", got)
	}

	// Exactly two terminals for the task, in order, one per generation.
	terminals := sink.countMatching(func(ev sinkEvent) bool { return isTerminalCallback(ev, "task-redispatch") })
	if terminals != 2 {
		t.Fatalf("expected exactly 2 terminal callbacks (one per dispatch generation), got %d", terminals)
	}
}
